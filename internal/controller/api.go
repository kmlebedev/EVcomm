package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kmlebedev/EVcomm/internal/domain"
	"github.com/kmlebedev/EVcomm/internal/hardware/mr6c"
	"github.com/kmlebedev/EVcomm/internal/policy"
)

var ErrUnknownLine = errors.New("controller: unknown line")

type LineStatus struct {
	ID       domain.LineID            `json:"id"`
	Output   int                      `json:"output"`
	Feedback int                      `json:"feedback_input"`
	State    domain.LineState         `json:"state"`
	Desired  bool                     `json:"desired_on"`
	Reason   string                   `json:"reason"`
	Since    time.Time                `json:"since"`
	Relay    domain.Observation[bool] `json:"relay"`
	Real     domain.Observation[bool] `json:"real_state"`
	NCClosed domain.Observation[bool] `json:"nc_closed"`
	Command  *domain.CommandResult    `json:"command,omitempty"`
}

type PostStatus struct {
	ID            domain.PostID `json:"id"`
	Mode          domain.Mode   `json:"mode"`
	Link          mr6c.Status   `json:"link"`
	LeaseDeadline time.Time     `json:"lease_deadline"`
	Snapshot      mr6c.Snapshot `json:"snapshot"`
	SnapshotAge   time.Duration `json:"snapshot_age"`
	Lines         []LineStatus  `json:"lines"`
}

// do выполняет f в goroutine цикла управления.
func (p *Post) do(ctx context.Context, f func(context.Context)) error {
	done := make(chan struct{})
	select {
	case p.reqs <- func(c context.Context) { f(c); close(done) }:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Submit принимает команду. Повтор с тем же ID возвращает сохранённый результат
// и не создаёт нового переключения. Новая команда по линии вытесняет незавершённую.
func (p *Post) Submit(ctx context.Context, cmd domain.Command) (domain.CommandResult, error) {
	var (
		res domain.CommandResult
		err error
	)
	derr := p.do(ctx, func(context.Context) { res, err = p.submit(cmd, time.Now()) })
	return res, errors.Join(derr, err)
}

func (p *Post) submit(cmd domain.Command, now time.Time) (domain.CommandResult, error) {
	if cmd.ID == "" {
		return domain.CommandResult{}, errors.New("controller: empty command id")
	}
	if r, ok := p.cmds[cmd.ID]; ok {
		return *r, nil
	}
	l := p.findLine(cmd.Line)
	if l == nil {
		return domain.CommandResult{}, fmt.Errorf("%w %q", ErrUnknownLine, cmd.Line)
	}
	r := &domain.CommandResult{Command: cmd, Status: domain.CommandAccepted, AcceptedAt: now}
	p.remember(r)
	if l.cmd != nil {
		p.finishCmd(l, domain.CommandFailed, "superseded by "+cmd.ID, now)
	}
	l.cmd = r

	if cmd.On {
		switch l.state {
		case domain.LineOn:
			if l.desired {
				p.finishCmd(l, domain.CommandConfirmed, "already ON", now)
				return *r, nil
			}
		case domain.LineOff, domain.LineEnabling:
		default:
			p.finishCmd(l, domain.CommandFailed, "line is "+string(l.state)+": "+l.reason, now)
			return *r, nil
		}
		l.desired = true
		return *r, nil
	}

	l.desired = false
	if l.state == domain.LineOff {
		p.finishCmd(l, domain.CommandConfirmed, "already OFF", now)
	}
	return *r, nil
}

func (p *Post) remember(r *domain.CommandResult) {
	p.cmds[r.Command.ID] = r
	p.cmdOrder = append(p.cmdOrder, r.Command.ID)
	for len(p.cmdOrder) > maxCommandHistory {
		if old := p.cmds[p.cmdOrder[0]]; old != nil && !old.Status.Terminal() {
			break
		}
		delete(p.cmds, p.cmdOrder[0])
		p.cmdOrder = p.cmdOrder[1:]
	}
}

// Command возвращает результат команды.
func (p *Post) Command(ctx context.Context, id string) (domain.CommandResult, bool, error) {
	var (
		res domain.CommandResult
		ok  bool
	)
	err := p.do(ctx, func(context.Context) {
		var r *domain.CommandResult
		if r, ok = p.cmds[id]; ok {
			res = *r
		}
	})
	return res, ok, err
}

// Wait ждёт завершения команды.
func (p *Post) Wait(ctx context.Context, id string) (domain.CommandResult, error) {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		res, ok, err := p.Command(ctx, id)
		if err != nil {
			return res, err
		}
		if !ok {
			return res, fmt.Errorf("controller: unknown command %q", id)
		}
		if res.Status.Terminal() {
			return res, nil
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-t.C:
		}
	}
}

func (p *Post) Status(ctx context.Context) (PostStatus, error) {
	var st PostStatus
	err := p.do(ctx, func(context.Context) {
		now := time.Now()
		st = PostStatus{
			ID:            p.post.ID,
			Mode:          p.mode,
			Link:          p.hw.Status(),
			LeaseDeadline: p.lease.Deadline(now),
			Snapshot:      p.snap,
		}
		if p.snap.Seq != 0 {
			st.SnapshotAge = now.Sub(p.snap.ReadAt)
		}
		for _, l := range p.lines {
			ls := LineStatus{
				ID: l.cfg.ID, Output: l.cfg.Output, Feedback: l.cfg.Feedback,
				State: l.state, Desired: l.desired, Reason: l.reason, Since: l.since,
				Relay: l.relay, Real: l.real, NCClosed: l.feedback,
			}
			if l.cmd != nil {
				c := *l.cmd
				ls.Command = &c
			}
			st.Lines = append(st.Lines, ls)
		}
	})
	return st, err
}

// Reset снимает FAULT/LOCKED после проверки причины: линия должна быть подтверждённо отпущена.
func (p *Post) Reset(ctx context.Context, id domain.LineID) error {
	var err error
	derr := p.do(ctx, func(context.Context) {
		l := p.findLine(id)
		switch {
		case l == nil:
			err = fmt.Errorf("%w %q", ErrUnknownLine, id)
		case l.state != domain.LineFault && l.state != domain.LineLocked:
			err = fmt.Errorf("line %s is %s, nothing to reset", id, l.state)
		case l.relay.Quality != domain.QualityFresh || l.relay.Value || l.real.Value || !l.feedback.Value:
			err = fmt.Errorf("line %s is not confirmed released (K%d=%v real=%v NC closed=%v, %s)",
				id, l.cfg.Output, l.relay.Value, l.real.Value, l.feedback.Value, l.relay.Quality)
		default:
			l.faultAfterOff, l.lostWhileOn, l.watchdogExpected = "", "", false
			p.setState(l, domain.LineOff, "reset by operator", time.Now())
		}
	})
	return errors.Join(derr, err)
}

// SetMode меняет режим поста. Требуется подтверждённый OFF всех линий и выдержка паузы.
func (p *Post) SetMode(ctx context.Context, mode domain.Mode) error {
	var err error
	derr := p.do(ctx, func(context.Context) {
		if !mode.Valid() || (mode == domain.Mode1x3 && !p.post.HasK4) {
			err = fmt.Errorf("mode %q is not available on post %s", mode, p.post.ID)
			return
		}
		if err = policy.CheckModeSwitch(p.lineViews()); err != nil {
			return
		}
		var last time.Time
		for _, l := range p.lines {
			if l.lastSwitch.After(last) {
				last = l.lastSwitch
			}
		}
		if wait := p.t.ModeSwitchPause - time.Since(last); wait > 0 {
			err = fmt.Errorf("mode switch pause: wait %s", wait.Round(100*time.Millisecond))
			return
		}
		p.log.Info("mode changed", "from", p.mode, "to", mode)
		p.mode = mode
	})
	return errors.Join(derr, err)
}

// AllOff отправляет OFF всем линиям и возвращает ID команд.
func (p *Post) AllOff(ctx context.Context, prefix string) ([]string, error) {
	var ids []string
	err := p.do(ctx, func(context.Context) {
		now := time.Now()
		for _, l := range p.lines {
			id := fmt.Sprintf("%s-%s-%d", prefix, l.cfg.ID, now.UnixNano())
			if _, err := p.submit(domain.Command{ID: id, Line: l.cfg.ID, On: false}, now); err == nil {
				ids = append(ids, id)
			}
		}
	})
	return ids, err
}

// DebugHang блокирует цикл управления на d — стендовая проверка watchdog при живом адаптере.
func (p *Post) DebugHang(ctx context.Context, d time.Duration) error {
	return p.do(ctx, func(context.Context) {
		p.log.Warn("debug: control loop hang", "duration", d)
		time.Sleep(d)
	})
}
