// Package controller — автомат состояний поста: последовательности ON/OFF с
// подтверждением по выходу, фактическому состоянию и НЗ, сверка и аренда.
//
// Каждым постом владеет одна goroutine (Run). Она же — и только она — продлевает
// аренду, без которой адаптер MR6C не обменивается с модулем.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/kmlebedev/EVcomm/internal/domain"
	"github.com/kmlebedev/EVcomm/internal/hardware/mr6c"
	"github.com/kmlebedev/EVcomm/internal/policy"
	"github.com/kmlebedev/EVcomm/internal/registry"
	"github.com/kmlebedev/EVcomm/internal/supervision"
)

// Hardware — контракт оборудования поста.
type Hardware interface {
	SetOutput(ctx context.Context, k int, on bool) (mr6c.WriteResult, error)
	Watch() <-chan mr6c.Snapshot
	Status() mr6c.Status
}

type Config struct {
	Post   registry.Post
	HW     Hardware
	Lease  *supervision.Lease
	Logger *slog.Logger
}

const (
	maxCommandHistory = 10000
	faultRetryOff     = 500 * time.Millisecond
)

type line struct {
	cfg        registry.Line
	state      domain.LineState
	desired    bool
	reason     string
	since      time.Time
	lastSwitch time.Time // последнее подтверждённое переключение
	cmd        *domain.CommandResult

	deadline     time.Time // срок подтверждения ENABLING/DISABLING
	writeStarted time.Time
	lastOffWrite time.Time

	// lostWhileOn — причина потери достоверности, пока линия была под напряжением.
	// После восстановления такая линия уходит в FAULT до решения оператора.
	lostWhileOn      string
	watchdogExpected bool   // потеря длилась дольше leaseTTL + poll_timeout_s
	faultAfterOff    string // после подтверждённого OFF перейти в FAULT с этой причиной

	relay, real, feedback domain.Observation[bool]
}

type Post struct {
	post  registry.Post
	t     registry.Timing
	hw    Hardware
	lease *supervision.Lease
	log   *slog.Logger

	mode     domain.Mode
	lines    []*line
	snap     mr6c.Snapshot
	lastStep time.Time

	cmds     map[string]*domain.CommandResult
	cmdOrder []string

	reqs chan func(context.Context)
}

func New(cfg Config) *Post {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	p := &Post{
		post:  cfg.Post,
		t:     cfg.Post.Timing,
		hw:    cfg.HW,
		lease: cfg.Lease,
		log:   cfg.Logger.With("post", cfg.Post.ID, "component", "controller"),
		mode:  cfg.Post.Mode,
		cmds:  map[string]*domain.CommandResult{},
		reqs:  make(chan func(context.Context)),
	}
	now := time.Now()
	for _, lc := range cfg.Post.Lines() {
		// Старт без автоматического ON: всё требуемое — OFF, фактическое — неизвестно до сверки.
		p.lines = append(p.lines, &line{cfg: lc, state: domain.LineUnknown, reason: "startup", since: now})
	}
	return p
}

// Run — цикл управления поста. Возвращается при отмене ctx.
func (p *Post) Run(ctx context.Context) error {
	tick := time.NewTicker(p.t.TickPeriod)
	defer tick.Stop()
	p.lastStep = time.Now()
	p.step(ctx, p.lastStep)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		case s := <-p.hw.Watch():
			p.observe(s)
		case f := <-p.reqs:
			f(ctx)
		}
		p.step(ctx, time.Now())
	}
}

func (p *Post) observe(s mr6c.Snapshot) {
	if s.Seq > p.snap.Seq {
		p.snap = s
	}
}

func (p *Post) pollTimeout() time.Duration {
	return time.Duration(p.post.Safety.PollTimeoutS) * time.Second
}

func (p *Post) writeTimeout() time.Duration { return 2 * p.t.ModbusTimeout }

// step — один такт: проверка аренды, сверка наблюдений, продвижение автоматов линий.
func (p *Post) step(ctx context.Context, now time.Time) {
	if gap := now.Sub(p.lastStep); gap > p.t.LeaseTTL {
		// Цикл не выполнялся дольше аренды: адаптер молчал, MR6C мог уйти в безопасный режим.
		p.log.Error("control loop stalled, lease expired", "gap", gap.Round(time.Millisecond))
		p.markLost(now, fmt.Sprintf("control loop stalled for %s", gap.Round(time.Millisecond)),
			gap > p.t.LeaseTTL+p.pollTimeout())
	}
	p.lastStep = now
	p.lease.Extend(now, p.t.LeaseTTL)

	s := p.snap
	if s.Seq == 0 || now.Sub(s.ReadAt) > p.t.StaleAfter {
		p.markStale(now)
		return
	}
	for _, l := range p.lines {
		p.stepLine(ctx, l, s, now)
	}
}

// markStale: нет свежих данных — запрет включений, запрос OFF, состояние UNKNOWN (правило 5).
func (p *Post) markStale(now time.Time) {
	var anyChanged bool
	for _, l := range p.lines {
		for _, o := range []*domain.Observation[bool]{&l.relay, &l.real, &l.feedback} {
			if o.Quality == domain.QualityFresh {
				o.Quality = domain.QualityStale
			}
		}
		if l.state != domain.LineUnknown && l.state != domain.LineFault && l.state != domain.LineLocked {
			anyChanged = true
		}
	}
	if anyChanged {
		p.markLost(now, "no fresh data from MR6C", false)
	}
}

// markLost переводит линии в UNKNOWN и снимает требуемое ON.
func (p *Post) markLost(now time.Time, reason string, watchdogExpected bool) {
	for _, l := range p.lines {
		energized := l.desired || l.state == domain.LineOn || l.state == domain.LineEnabling || l.state == domain.LineDisabling
		if energized && l.lostWhileOn == "" {
			l.lostWhileOn = reason
		}
		if energized {
			l.watchdogExpected = l.watchdogExpected || watchdogExpected
		}
		l.desired = false
		p.failCmd(l, now, reason, true)
		if l.state != domain.LineFault && l.state != domain.LineLocked && l.state != domain.LineUnknown {
			p.setState(l, domain.LineUnknown, reason, now)
		}
	}
}

func (p *Post) stepLine(ctx context.Context, l *line, s mr6c.Snapshot, now time.Time) {
	out, real, nc := s.Output(l.cfg.Output), s.Real(l.cfg.Output), s.Input(l.cfg.Feedback)
	l.relay = domain.Observation[bool]{Value: out, Source: fmt.Sprintf("mr6c:coil:K%d", l.cfg.Output), ObservedAt: s.ReadAt, Quality: domain.QualityFresh}
	l.real = domain.Observation[bool]{Value: real, Source: fmt.Sprintf("mr6c:real:K%d", l.cfg.Output), ObservedAt: s.ReadAt, Quality: domain.QualityFresh}
	l.feedback = domain.Observation[bool]{Value: nc, Source: fmt.Sprintf("mr6c:input:%d:nc", l.cfg.Feedback), ObservedAt: s.ReadAt, Quality: domain.QualityFresh}

	released := !out && !real && nc // НЗ замкнут: механизм отпущен (не доказывает отсутствие напряжения)
	engaged := out && real && !nc
	obs := fmt.Sprintf("K%d=%v real=%v NC(in%d)=%s", l.cfg.Output, out, real, l.cfg.Feedback, ncString(nc))

	switch l.state {
	case domain.LineUnknown:
		lost, wdExpected := l.lostWhileOn, l.watchdogExpected
		l.lostWhileOn, l.watchdogExpected = "", false
		switch {
		case released && lost != "":
			p.setState(l, domain.LineFault, lost+"; outputs found released", now)
			p.settleOff(l, now, s.ReadAt)
		case released:
			p.setState(l, domain.LineOff, "reconciled: "+obs, now)
			p.settleOff(l, now, s.ReadAt)
		case !out && !real && !nc:
			p.setState(l, domain.LineFault, "NC feedback open while output is off: "+obs, now)
		case wdExpected && (out || real):
			p.setState(l, domain.LineFault, "watchdog did not release output: "+lost+": "+obs, now)
			p.writeOff(ctx, l, now)
		default:
			l.faultAfterOff = lost
			p.beginOff(ctx, l, now, "reconcile: "+obs)
		}

	case domain.LineOff:
		switch {
		case !released:
			p.setState(l, domain.LineFault, "unexpected change while OFF: "+obs, now)
			p.failCmd(l, now, "line fault", false)
			l.desired = false
			if out || real {
				p.writeOff(ctx, l, now)
			}
		case l.desired:
			p.tryEnable(ctx, l, now)
		}

	case domain.LineEnabling:
		if out && real && l.cmd != nil && l.cmd.Timings.RealState == 0 {
			l.cmd.Timings.RealState = s.ReadAt.Sub(l.writeStarted)
		}
		switch {
		case engaged:
			l.lastSwitch = now
			p.setState(l, domain.LineOn, "confirmed: "+obs, now)
			if l.cmd != nil && l.cmd.Command.On {
				l.cmd.Timings.Feedback = s.ReadAt.Sub(l.writeStarted)
				p.finishCmd(l, domain.CommandConfirmed, "", now)
			}
			if !l.desired {
				p.beginOff(ctx, l, now, "OFF requested while enabling")
			}
		case !l.desired:
			p.beginOff(ctx, l, now, "OFF requested while enabling")
		case now.After(l.deadline) && released:
			// Запись не дошла или не исполнилась: питание не подано.
			l.desired = false
			p.failCmd(l, now, "ON not executed by module: "+obs, false)
			p.setState(l, domain.LineOff, "ON not executed: "+obs, now)
		case now.After(l.deadline):
			l.desired = false
			reason := "ON not confirmed: " + obs
			if out && real && nc {
				reason = "contactor did not operate (NC still closed): " + obs
			}
			p.failCmd(l, now, reason, false)
			p.setState(l, domain.LineFault, reason, now)
			p.writeOff(ctx, l, now)
		}

	case domain.LineOn:
		switch {
		case !engaged:
			// Обратная связь разошлась с командой: авария и блокировка линии.
			l.desired = false
			p.setState(l, domain.LineFault, "feedback mismatch while ON: "+obs, now)
			if out || real {
				p.writeOff(ctx, l, now)
			}
		case !l.desired:
			p.beginOff(ctx, l, now, "OFF requested")
		}

	case domain.LineDisabling:
		switch {
		case released:
			l.lastSwitch = now
			if reason := l.faultAfterOff; reason != "" {
				l.faultAfterOff = ""
				p.setState(l, domain.LineFault, reason+"; output switched off after reconcile", now)
			} else {
				p.setState(l, domain.LineOff, "confirmed: "+obs, now)
			}
			p.settleOff(l, now, s.ReadAt)
		case now.After(l.deadline):
			reason := "OFF not confirmed: " + obs
			if !out && !real && !nc {
				reason = "contactor did not release or NC circuit open: " + obs
			}
			p.failCmd(l, now, reason, false)
			p.setState(l, domain.LineFault, reason, now)
		}

	case domain.LineFault, domain.LineLocked:
		if (out || real) && now.Sub(l.lastOffWrite) >= faultRetryOff {
			p.writeOff(ctx, l, now)
		}
		if released {
			p.settleOff(l, now, s.ReadAt)
		}
	}
}

func (p *Post) tryEnable(ctx context.Context, l *line, now time.Time) {
	cmd := l.cmd
	fail := func(reason string) {
		l.desired = false
		p.failCmd(l, now, reason, false)
		p.log.Warn("enable refused", "line", l.cfg.ID, "reason", reason)
	}
	if cmd != nil && !cmd.Command.ExpiresAt.IsZero() && now.After(cmd.Command.ExpiresAt) {
		fail("command expired")
		return
	}
	if st := p.hw.Status(); !st.Safety.OK() {
		fail("MR6C safety config not verified: " + st.Safety.String())
		return
	}
	if err := policy.CheckEnable(p.post, p.mode, l.cfg, p.lineViews()); err != nil {
		fail(err.Error())
		return
	}
	if wait := p.t.MinSwitchInterval - now.Sub(l.lastSwitch); wait > 0 {
		return // команда ждёт в статусе accepted
	}
	if cmd != nil {
		cmd.Status = domain.CommandInProgress
	}
	l.writeStarted = now
	res, err := p.write(ctx, l, true)
	if err != nil && !errors.Is(err, mr6c.ErrOutcomeUnknown) {
		// Запись точно не выполнена.
		fail("write ON failed: " + err.Error())
		return
	}
	if cmd != nil && res.Level >= mr6c.ConfirmWriteAck {
		cmd.Timings.WriteAck = res.WriteAck
		if res.Level >= mr6c.ConfirmRealState {
			cmd.Timings.RealState = res.Readback
		}
	}
	l.deadline = now.Add(p.writeTimeout() + p.t.FeedbackTimeout)
	p.setState(l, domain.LineEnabling, "ON written ("+res.Level.String()+")", now)
}

func (p *Post) beginOff(ctx context.Context, l *line, now time.Time, reason string) {
	l.desired = false
	if l.cmd != nil && !l.cmd.Command.On {
		l.cmd.Status = domain.CommandInProgress
	}
	l.writeStarted = now
	res := p.writeOff(ctx, l, now)
	if l.cmd != nil && !l.cmd.Command.On && res.Level >= mr6c.ConfirmWriteAck {
		l.cmd.Timings.WriteAck = res.WriteAck
		if res.Level >= mr6c.ConfirmRealState {
			l.cmd.Timings.RealState = res.Readback
		}
	}
	l.deadline = now.Add(p.writeTimeout() + p.t.FeedbackTimeout)
	p.setState(l, domain.LineDisabling, reason, now)
}

func (p *Post) writeOff(ctx context.Context, l *line, now time.Time) mr6c.WriteResult {
	l.lastOffWrite = now
	res, err := p.write(ctx, l, false)
	if err != nil {
		p.log.Warn("write OFF failed", "line", l.cfg.ID, "err", err)
	}
	return res
}

func (p *Post) write(ctx context.Context, l *line, on bool) (mr6c.WriteResult, error) {
	wctx, cancel := context.WithTimeout(ctx, p.writeTimeout())
	defer cancel()
	res, err := p.hw.SetOutput(wctx, l.cfg.Output, on)
	if res.Level >= mr6c.ConfirmOutput {
		p.observe(res.Snapshot)
	}
	return res, err
}

// settleOff подтверждает ожидающую OFF-команду, когда механизм отпущен.
func (p *Post) settleOff(l *line, now, readAt time.Time) {
	if l.cmd == nil || l.cmd.Status.Terminal() {
		return
	}
	if l.cmd.Command.On {
		p.finishCmd(l, domain.CommandFailed, "line is "+string(l.state), now)
		return
	}
	if !l.writeStarted.IsZero() && l.state != domain.LineFault {
		l.cmd.Timings.Feedback = readAt.Sub(l.writeStarted)
	}
	reason := ""
	if l.state == domain.LineFault {
		reason = "line in FAULT, output released: " + l.reason
	}
	p.finishCmd(l, domain.CommandConfirmed, reason, now)
}

func (p *Post) setState(l *line, st domain.LineState, reason string, now time.Time) {
	if l.state == st && l.reason == reason {
		return
	}
	lvl := slog.LevelInfo
	if st == domain.LineFault || st == domain.LineLocked {
		lvl = slog.LevelError
	}
	p.log.Log(context.Background(), lvl, "line state", "line", l.cfg.ID, "from", l.state, "to", st, "reason", reason)
	l.state, l.reason, l.since = st, reason, now
}

func (p *Post) failCmd(l *line, now time.Time, reason string, all bool) {
	if l.cmd == nil || l.cmd.Status.Terminal() {
		return
	}
	// При потере достоверности OFF-команду нельзя считать ни выполненной, ни проваленной.
	if all && !l.cmd.Command.On {
		return
	}
	p.finishCmd(l, domain.CommandFailed, reason, now)
}

func (p *Post) finishCmd(l *line, st domain.CommandStatus, reason string, now time.Time) {
	l.cmd.Status, l.cmd.Reason, l.cmd.FinishedAt = st, reason, now
	p.log.Info("command finished", "id", l.cmd.Command.ID, "line", l.cmd.Command.Line, "on", l.cmd.Command.On,
		"status", st, "reason", reason, "write_ack", l.cmd.Timings.WriteAck, "real_state", l.cmd.Timings.RealState, "feedback", l.cmd.Timings.Feedback)
	l.cmd = nil
}

func (p *Post) lineViews() []policy.LineView {
	v := make([]policy.LineView, len(p.lines))
	for i, l := range p.lines {
		v[i] = policy.LineView{Line: l.cfg, State: l.state}
	}
	return v
}

func (p *Post) findLine(id domain.LineID) *line {
	for _, l := range p.lines {
		if l.cfg.ID == id {
			return l
		}
	}
	return nil
}

func ncString(closed bool) string {
	if closed {
		return "closed"
	}
	return "open"
}
