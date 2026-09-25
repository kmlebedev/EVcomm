package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kmlebedev/EVcomm/internal/domain"
	"github.com/kmlebedev/EVcomm/internal/hardware/mercury230"
)

// watch — проверки 6, 7, 8, 13: адаптер поста в штатном режиме, внеочередной снимок
// энергии каждые interval, приросты по фазам и сверка с суммой.
func (b *bench) watch(ctx context.Context, interval, duration time.Duration, csvPath string) error {
	if duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}
	cfg := b.adapterConfig()
	cfg.EnergyPeriod = time.Hour // снимки делает watch, чтобы шаг был ровно interval
	a := mercury230.New(cfg)
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(runCtx); close(done) }()
	defer func() { stop(); <-done }()

	var w *csv.Writer
	if csvPath != "" {
		f, err := os.Create(csvPath)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		w = csv.NewWriter(f)
		defer w.Flush()
		_ = w.Write([]string{"time", "state", "p1_w", "p2_w", "p3_w", "ap1_wh", "ap2_wh", "ap3_wh", "total_wh", "quality", "issues", "error"})
	}

	b.printf("наблюдение: снимок каждые %s%s; Ctrl-C — итог\n", interval, map[bool]string{true: ", до " + duration.String(), false: ""}[duration > 0])
	b.printf("%-8s %-12s %27s %32s %10s %16s %s\n", "время", "состояние", "P1/P2/P3, Вт", "AP1/AP2/AP3, Вт·ч", "A+ total", "ΔAP1/2/3 ΔTotal", "качество")

	var (
		first, prev *mercury230.EnergySnapshot
		changes     [3]int
		powerSum    [3]float64
		powerN      int
		errs        int
		disputed    []string
	)
	t := time.NewTicker(interval)
	defer t.Stop()
	sample := func() {
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		s, err := a.ReadEnergy(rctx)
		cancel()
		st := a.MeterStatus()
		p := a.LatestPower()
		now := time.Now().Format("15:04:05")
		pFresh := !p.ObservedAt.IsZero() && time.Since(p.ObservedAt) < 3*cfg.PowerPeriod+time.Second
		pw := "—"
		if pFresh {
			pw = fmt.Sprintf("%.0f/%.0f/%.0f", float64(p.PhaseCW[0])/100, float64(p.PhaseCW[1])/100, float64(p.PhaseCW[2])/100)
			for i := range powerSum {
				powerSum[i] += float64(p.PhaseCW[i]) / 100
			}
			powerN++
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			errs++
			b.printf("%-8s %-12s %27s  ошибка: %v\n", now, st.State, pw, err)
			if w != nil {
				_ = w.Write([]string{now, string(st.State), "", "", "", "", "", "", "", "", "", err.Error()})
			}
			return
		}
		if first == nil {
			first = &s
		}
		delta := "—"
		if prev != nil {
			var d [3]int64
			for i := range d {
				d[i] = int64(s.PhaseWh[i]) - int64(prev.PhaseWh[i])
				if d[i] != 0 {
					changes[i]++
				}
			}
			delta = fmt.Sprintf("%d/%d/%d %d", d[0], d[1], d[2], int64(s.TotalWh)-int64(prev.TotalWh))
		}
		prev = &s
		q := string(s.Quality)
		if s.Quality == domain.QualityDisputed {
			disputed = append(disputed, now+": "+strings.Join(s.Issues, "; "))
			q += " (" + strings.Join(s.Issues, "; ") + ")"
		}
		ap := "—"
		if s.HasPhase {
			ap = fmt.Sprintf("%d/%d/%d", s.PhaseWh[0], s.PhaseWh[1], s.PhaseWh[2])
		}
		b.printf("%-8s %-12s %27s %32s %10d %16s %s\n", now, st.State, pw, ap, s.TotalWh, delta, q)
		if w != nil {
			_ = w.Write([]string{now, string(st.State), num(p.PhaseCW[0]), num(p.PhaseCW[1]), num(p.PhaseCW[2]),
				fmt.Sprint(s.PhaseWh[0]), fmt.Sprint(s.PhaseWh[1]), fmt.Sprint(s.PhaseWh[2]), fmt.Sprint(s.TotalWh),
				string(s.Quality), strings.Join(s.Issues, "; "), ""})
			w.Flush()
		}
	}

	// Первый снимок — после подключения.
	online := time.NewTimer(30 * time.Second)
	defer online.Stop()
wait:
	for a.MeterStatus().State != mercury230.StateOnline {
		select {
		case <-ctx.Done():
			break wait
		case <-online.C:
			b.printf("счётчик не на связи за 30 с: %s %s — продолжаю\n", a.MeterStatus().State, a.MeterStatus().LastError)
			break wait
		case <-time.After(50 * time.Millisecond):
		}
	}
	if ctx.Err() == nil {
		sample()
	}
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-t.C:
			sample()
		}
	}

	st := a.MeterStatus()
	r := &report{out: b.out}
	fmt.Fprintln(b.out)
	if first == nil || prev == nil || prev == first {
		r.add("6–8", "наблюдение", vInfo, "недостаточно снимков (ошибок %d)", errs)
		return nil
	}
	span := prev.ObservedAt.Sub(first.ObservedAt)
	r.add("6, 8", "приросты и дискретность", vInfo, "за %s, снимков с ошибкой %d", span.Round(time.Second), errs)
	var sum int64
	for i := range 3 {
		d := int64(prev.PhaseWh[i]) - int64(first.PhaseWh[i])
		sum += d
		meanP := 0.0
		if powerN > 0 {
			meanP = powerSum[i] / float64(powerN)
		}
		line := fmt.Sprintf("фаза %d (%s): ΔAP %d Вт·ч, средняя P %.0f Вт → ожидалось ≈ %.1f Вт·ч; AP менялась в %d снимках",
			i+1, b.opt.phaseMap[i], d, meanP, meanP*span.Hours(), changes[i])
		if meanP > 0 {
			line += fmt.Sprintf("; 1 Вт·ч при такой P — каждые %s", time.Duration(3600/meanP*float64(time.Second)).Round(10*time.Millisecond))
		}
		r.sub("%s", line)
	}
	dTotal := int64(prev.TotalWh) - int64(first.TotalWh)
	diff := sum - dTotal
	v := vPass
	if !prev.HasPhase {
		v = vInfo
	} else if diff > 5 || diff < -5 {
		v = vFail
	}
	r.add("7", "ΣΔAPn = ΔTotal", v, "за %s: ΣΔAPn %d, ΔTotal %d, расхождение %d Вт·ч (допуск ±5)", span.Round(time.Second), sum, dTotal, diff)
	if len(disputed) == 0 {
		r.add("13", "достоверность снимков", vPass, "уменьшений AP и спорных снимков нет; ошибок чтения %d, подключений %d", errs, st.Reconnects)
	} else {
		r.add("13", "достоверность снимков", vFail, "спорных снимков %d; ошибок чтения %d, подключений %d", len(disputed), errs, st.Reconnects)
		for _, d := range disputed {
			r.sub("%s", d)
		}
	}
	r.summary()
	return nil
}

func num(cw int32) string { return strconv.FormatFloat(float64(cw)/100, 'f', 2, 64) }
