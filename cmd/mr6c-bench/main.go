// Command mr6c-bench — стенд «Go → WB-MGE (Modbus TCP) → MR6C → контактор с НЗ»:
// ручное управление линиями, серии ON/OFF с задержками p50/p99 и проверка watchdog.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/kmlebedev/EVcomm/internal/controller"
	"github.com/kmlebedev/EVcomm/internal/domain"
	"github.com/kmlebedev/EVcomm/internal/registry"
)

const help = `Команды:
  status                    состояние поста, связи, аренды и линий
  on <line> | off <line>    линии: L1 L2 L3 3P; off all — все
  reset <line>              снять FAULT (линия должна быть отпущена)
  mode 3x1|1x3              сменить режим (все линии OFF, выдержана пауза)
  verify                    перечитать эталон настроек MR6C
  apply-safety              ЗАПИСАТЬ эталон в MR6C (только стенд, с подтверждением)
  cycle <line> <n>          n циклов ON/OFF, задержки p50/p99 (min_switch_interval из реестра)
  hang <duration>           заблокировать цикл управления (адаптер жив)
  watchdog <line>           ON → зависание > lease_ttl+poll_timeout_s → проверка OFF и отсутствия авто-ON
  help | quit`

type bench struct {
	st      *controller.Station
	inLines <-chan string
	seq     int
}

func main() {
	regPath := flag.String("registry", "configs/bench.yaml", "post registry")
	postID := flag.String("post", "", "post id (default: first in registry)")
	verbose := flag.Bool("v", false, "debug logs")
	flag.Parse()

	lvl := slog.LevelInfo
	if *verbose {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	reg, err := registry.Load(*regPath)
	if err != nil {
		fatal(err)
	}
	if len(reg.Posts) == 0 {
		fatal(errors.New("registry has no posts"))
	}
	post := reg.Posts[0]
	if *postID != "" {
		var ok bool
		if post, ok = reg.Post(domain.PostID(*postID)); !ok {
			fatal(fmt.Errorf("post %q not found", *postID))
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	runCtx, stopRun := context.WithCancel(context.Background())
	st := controller.NewStation(post, nil, 0, log)
	done := make(chan struct{})
	go func() { st.Run(runCtx); close(done) }()

	fmt.Printf("Пост %s: шлюз %s, MR6C адрес %d, режим %s\n", post.ID, post.Gateway, post.MR6CAddress, post.Mode)
	fmt.Printf("lease_ttl=%s poll_timeout_s=%d → ожидаемое отпускание ≤ %s + время контактора\n",
		post.Timing.LeaseTTL, post.Safety.PollTimeoutS, post.Timing.LeaseTTL+time.Duration(post.Safety.PollTimeoutS)*time.Second)
	fmt.Printf("PID %d — проверка watchdog: kill -STOP %d; через 5 с kill -CONT %d\n\n%s\n", os.Getpid(), os.Getpid(), os.Getpid(), help)

	lines := make(chan string)
	go func() {
		in := bufio.NewScanner(os.Stdin)
		for in.Scan() {
			lines <- in.Text()
		}
		close(lines)
	}()
	b := &bench{st: st, inLines: lines}

loop:
	for {
		fmt.Print("> ")
		select {
		case <-ctx.Done():
			break loop
		case l, ok := <-lines:
			if !ok {
				break loop
			}
			if quit := b.exec(ctx, strings.Fields(l)); quit {
				break loop
			}
		}
	}

	fmt.Println("\nОтключение всех линий…")
	if err := st.ShutdownOff(context.Background(), 3*time.Second); err != nil {
		fmt.Println("OFF не подтверждён:", err, "— MR6C отключит выходы по таймауту опроса")
	}
	stopRun()
	<-done
}

func (b *bench) exec(ctx context.Context, args []string) (quit bool) {
	if len(args) == 0 {
		return false
	}
	ctl := b.st.Ctl
	var err error
	switch cmd, rest := args[0], args[1:]; cmd {
	case "help", "?":
		fmt.Println(help)
	case "quit", "exit", "q":
		return true
	case "status", "s":
		err = b.status(ctx)
	case "on", "off":
		if len(rest) != 1 {
			return b.usage("on|off <line>")
		}
		if cmd == "off" && rest[0] == "all" {
			err = b.st.ShutdownOff(ctx, 3*time.Second)
			break
		}
		var res domain.CommandResult
		if res, err = b.switchLine(ctx, domain.LineID(strings.ToUpper(rest[0])), cmd == "on"); err == nil {
			printResult(res)
		}
	case "reset":
		if len(rest) != 1 {
			return b.usage("reset <line>")
		}
		err = ctl.Reset(ctx, domain.LineID(strings.ToUpper(rest[0])))
	case "mode":
		if len(rest) != 1 {
			return b.usage("mode 3x1|1x3")
		}
		err = ctl.SetMode(ctx, domain.Mode(rest[0]))
	case "verify":
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		rep, verr := b.st.Adapter.VerifySafetyConfig(cctx)
		cancel()
		fmt.Printf("эталон: %s (прошивка %q)\n", rep, rep.Firmware)
		err = verr
	case "apply-safety":
		err = b.applySafety(ctx)
	case "cycle":
		if len(rest) != 2 {
			return b.usage("cycle <line> <n>")
		}
		n, perr := strconv.Atoi(rest[1])
		if perr != nil || n < 1 {
			return b.usage("cycle <line> <n>")
		}
		err = b.cycle(ctx, domain.LineID(strings.ToUpper(rest[0])), n)
	case "hang":
		if len(rest) != 1 {
			return b.usage("hang <duration>")
		}
		d, perr := time.ParseDuration(rest[0])
		if perr != nil {
			return b.usage("hang <duration>, например 5s")
		}
		err = ctl.DebugHang(ctx, d)
	case "watchdog":
		if len(rest) != 1 {
			return b.usage("watchdog <line>")
		}
		err = b.watchdog(ctx, domain.LineID(strings.ToUpper(rest[0])))
	default:
		fmt.Println("неизвестная команда; help — список")
	}
	if err != nil {
		fmt.Println("ошибка:", err)
	}
	return false
}

func (b *bench) usage(u string) bool {
	fmt.Println("использование:", u)
	return false
}

func (b *bench) switchLine(ctx context.Context, id domain.LineID, on bool) (domain.CommandResult, error) {
	b.seq++
	cmd := domain.Command{ID: fmt.Sprintf("bench-%d-%d", os.Getpid(), b.seq), Line: id, On: on}
	ctx, cancel := context.WithTimeout(ctx, b.st.Post.Timing.MinSwitchInterval+5*time.Second)
	defer cancel()
	if _, err := b.st.Ctl.Submit(ctx, cmd); err != nil {
		return domain.CommandResult{}, err
	}
	return b.st.Ctl.Wait(ctx, cmd.ID)
}

func (b *bench) status(ctx context.Context) error {
	st, err := b.st.Ctl.Status(ctx)
	if err != nil {
		return err
	}
	link := st.Link
	fmt.Printf("пост %s  режим %s  связь: tcp=%v модуль=%v трафик=%s  подключений %d\n",
		st.ID, st.Mode, link.Connected, link.ModuleOK, map[bool]string{true: "ОСТАНОВЛЕН (аренда)", false: "идёт"}[link.Silenced], link.Reconnects)
	fmt.Printf("эталон MR6C: %s  прошивка %q\n", link.Safety, link.Safety.Firmware)
	if link.LastError != "" {
		fmt.Printf("последняя ошибка (%s назад): %s\n", time.Since(link.LastErrorAt).Round(time.Millisecond), link.LastError)
	}
	fmt.Printf("аренда до %s (через %s)  снимок #%d возраст %s: %s\n",
		st.LeaseDeadline.Format("15:04:05.000"), time.Until(st.LeaseDeadline).Round(time.Millisecond),
		st.Snapshot.Seq, st.SnapshotAge.Round(time.Millisecond), st.Snapshot)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "линия\tвыход\tсостояние\tтребуется\tK\treal\tНЗ\tкачество\tпричина")
	for _, l := range st.Lines {
		fmt.Fprintf(w, "%s\tK%d/вх%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", l.ID, l.Output, l.Feedback, l.State,
			onOff(l.Desired), onOff(l.Relay.Value), onOff(l.Real.Value), map[bool]string{true: "замкнут", false: "разомкнут"}[l.NCClosed.Value],
			l.Relay.Quality, l.Reason)
	}
	return w.Flush()
}

func (b *bench) applySafety(ctx context.Context) error {
	fmt.Print("Будут записаны регистры 5, 6, 8–14, 16, 19, 930–951 модуля. Введите yes: ")
	select {
	case l := <-b.inLines:
		if strings.TrimSpace(l) != "yes" {
			fmt.Println("отменено")
			return nil
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rep, err := b.st.Adapter.ApplySafetyConfig(cctx)
	fmt.Printf("эталон после записи: %s\n", rep)
	return err
}

type series struct{ writeAck, realState, feedback []time.Duration }

func (s *series) add(t domain.CommandTimings) {
	s.writeAck = append(s.writeAck, t.WriteAck)
	s.realState = append(s.realState, t.RealState)
	s.feedback = append(s.feedback, t.Feedback)
}

func (b *bench) cycle(ctx context.Context, id domain.LineID, n int) error {
	var on, off series
	failures := 0
	started := time.Now()
	for i := range n {
		for _, want := range []bool{true, false} {
			res, err := b.switchLine(ctx, id, want)
			if err != nil {
				return err
			}
			if res.Status != domain.CommandConfirmed {
				failures++
				fmt.Printf("цикл %d %s: %s %s\n", i+1, onOff(want), res.Status, res.Reason)
				if want {
					continue
				}
				return fmt.Errorf("OFF не подтверждён, серия остановлена")
			}
			if want {
				on.add(res.Timings)
			} else {
				off.add(res.Timings)
			}
		}
		if (i+1)%10 == 0 {
			fmt.Printf("… %d/%d\n", i+1, n)
		}
	}
	fmt.Printf("%d циклов за %s, неудач %d. Задержки от начала записи:\n", n, time.Since(started).Round(time.Second), failures)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\tответ FC05\tвыход+real\tНЗ")
	for _, r := range []struct {
		name string
		s    series
	}{{"ON", on}, {"OFF", off}} {
		fmt.Fprintf(w, "%s p50/p99/max\t%s\t%s\t%s\n", r.name, pct(r.s.writeAck), pct(r.s.realState), pct(r.s.feedback))
	}
	return w.Flush()
}

func (b *bench) watchdog(ctx context.Context, id domain.LineID) error {
	p := b.st.Post
	res, err := b.switchLine(ctx, id, true)
	if err != nil {
		return err
	}
	if res.Status != domain.CommandConfirmed {
		return fmt.Errorf("ON не подтверждён: %s %s", res.Status, res.Reason)
	}
	hang := p.Timing.LeaseTTL + time.Duration(p.Safety.PollTimeoutS)*time.Second + time.Second
	fmt.Printf("%s ON. Цикл управления блокируется на %s; адаптер должен замолчать через %s, MR6C — отключить выходы\n",
		id, hang, p.Timing.LeaseTTL)
	if err := b.st.Ctl.DebugHang(ctx, hang); err != nil {
		return err
	}
	// Первый свежий снимок после паузы: контроллер сначала читает, затем пишет.
	deadline := time.Now().Add(3 * time.Second)
	var ls controller.LineStatus
	for time.Now().Before(deadline) {
		st, err := b.st.Ctl.Status(ctx)
		if err != nil {
			return err
		}
		ls = findLine(st, id)
		if ls.State == domain.LineFault || ls.State == domain.LineOff {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	switch {
	case strings.Contains(ls.Reason, "outputs found released"):
		fmt.Println("PASS: MR6C отключил выход по таймауту опроса:", ls.Reason)
	case strings.Contains(ls.Reason, "watchdog did not release"):
		fmt.Println("FAIL: выход остался включён после паузы:", ls.Reason)
	default:
		fmt.Printf("НЕОПРЕДЕЛЕНО: %s — %s\n", ls.State, ls.Reason)
	}
	time.Sleep(time.Second)
	st, err := b.st.Ctl.Status(ctx)
	if err != nil {
		return err
	}
	if ls = findLine(st, id); ls.Relay.Value {
		fmt.Println("FAIL: выход снова включён после восстановления связи")
	} else {
		fmt.Println("PASS: самопроизвольного ON нет; для продолжения — reset", id)
	}
	return nil
}

func findLine(st controller.PostStatus, id domain.LineID) controller.LineStatus {
	for _, l := range st.Lines {
		if l.ID == id {
			return l
		}
	}
	return controller.LineStatus{}
}

func printResult(r domain.CommandResult) {
	fmt.Printf("%s %s: %s", r.Command.Line, onOff(r.Command.On), r.Status)
	if r.Reason != "" {
		fmt.Printf(" (%s)", r.Reason)
	}
	if r.Status == domain.CommandConfirmed && r.Timings.WriteAck > 0 {
		fmt.Printf("  FC05 %s, выход+real %s, НЗ %s", r.Timings.WriteAck.Round(100*time.Microsecond),
			r.Timings.RealState.Round(100*time.Microsecond), r.Timings.Feedback.Round(100*time.Microsecond))
	}
	fmt.Println()
}

func pct(ds []time.Duration) string {
	if len(ds) == 0 {
		return "—"
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	at := func(q float64) time.Duration { return s[min(len(s)-1, int(q*float64(len(s))))] }
	r := func(d time.Duration) string { return d.Round(100 * time.Microsecond).String() }
	return r(at(0.5)) + "/" + r(at(0.99)) + "/" + r(s[len(s)-1])
}

func onOff(v bool) string {
	if v {
		return "ON"
	}
	return "OFF"
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "mr6c-bench:", err)
	os.Exit(1)
}
