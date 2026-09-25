package controller_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kmlebedev/EVcomm/internal/controller"
	"github.com/kmlebedev/EVcomm/internal/domain"
	"github.com/kmlebedev/EVcomm/internal/hardware/mr6c"
	"github.com/kmlebedev/EVcomm/internal/registry"
	"github.com/kmlebedev/EVcomm/internal/sim"
	"github.com/kmlebedev/EVcomm/internal/supervision"
)

var safety = mr6c.SafetyConfig{PollTimeoutS: 1, Source: 2}

func testPost() registry.Post {
	return registry.Post{
		ID: "post-test", Gateway: "127.0.0.1:502", MR6CAddress: 1, Mode: domain.Mode3x1, HasK4: true,
		InputRead: registry.InputReadContiguous,
		Timing: registry.Timing{
			LeaseTTL: 300 * time.Millisecond, TickPeriod: 20 * time.Millisecond, PollPeriod: 20 * time.Millisecond,
			ModbusTimeout: 100 * time.Millisecond, StaleAfter: 200 * time.Millisecond, FeedbackTimeout: 200 * time.Millisecond,
			ReconnectMin: 10 * time.Millisecond, ReconnectMax: 50 * time.Millisecond,
		},
		Safety: registry.Safety{PollTimeoutS: 1, Source: registry.SafetySourcePollTimeout},
		Limits: registry.Limits{PhaseMaxA: 60, EVSEMaxA: 32},
	}
}

type rig struct {
	sim  *sim.MR6C
	ctl  *controller.Post
	post registry.Post
}

func newRig(t *testing.T, configured bool, dial func(*sim.MR6C) mr6c.Dialer) *rig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	post := testPost()
	m := sim.NewMR6C(1, safety, configured, nil)
	go m.Run(ctx)
	if dial == nil {
		dial = (*sim.MR6C).Dialer
	}
	lease := &supervision.Lease{}
	ad := mr6c.New(mr6c.Config{
		Name: string(post.ID), Dial: dial(m), Gate: lease, PollPeriod: post.Timing.PollPeriod,
		RealState: true, Safety: safety, ReconnectMin: post.Timing.ReconnectMin, ReconnectMax: post.Timing.ReconnectMax,
	})
	ctl := controller.New(controller.Config{Post: post, HW: ad, Lease: lease})
	go func() { _ = ad.Run(ctx) }()
	go func() { _ = ctl.Run(ctx) }()
	r := &rig{sim: m, ctl: ctl, post: post}
	r.waitState(t, domain.LineL1, domain.LineOff, domain.LineFault)
	return r
}

func (r *rig) line(t *testing.T, id domain.LineID) controller.LineStatus {
	t.Helper()
	st, err := r.ctl.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range st.Lines {
		if l.ID == id {
			return l
		}
	}
	t.Fatalf("no line %s", id)
	return controller.LineStatus{}
}

func (r *rig) waitState(t *testing.T, id domain.LineID, want ...domain.LineState) controller.LineStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		l := r.line(t, id)
		for _, w := range want {
			if l.State == w {
				return l
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("line %s: state %s (%s), want %v", id, l.State, l.Reason, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var seq int

func (r *rig) run(t *testing.T, id domain.LineID, on bool) domain.CommandResult {
	t.Helper()
	seq++
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := domain.Command{ID: fmt.Sprintf("c%d", seq), Line: id, On: on}
	if _, err := r.ctl.Submit(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	res, err := r.ctl.Wait(ctx, cmd.ID)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestOnOffConfirmedByFeedback(t *testing.T) {
	r := newRig(t, true, nil)
	res := r.run(t, domain.LineL1, true)
	if res.Status != domain.CommandConfirmed {
		t.Fatalf("ON: %s %s", res.Status, res.Reason)
	}
	if res.Timings.WriteAck <= 0 || res.Timings.Feedback < r.sim.ContactorDelay {
		t.Fatalf("timings: %+v", res.Timings)
	}
	l := r.line(t, domain.LineL1)
	if l.State != domain.LineOn || !l.Relay.Value || l.NCClosed.Value {
		t.Fatalf("line after ON: %+v", l)
	}
	res = r.run(t, domain.LineL1, false)
	if res.Status != domain.CommandConfirmed {
		t.Fatalf("OFF: %s %s", res.Status, res.Reason)
	}
	if l := r.line(t, domain.LineL1); l.State != domain.LineOff || !l.NCClosed.Value {
		t.Fatalf("line after OFF: %+v", l)
	}
}

func TestRepeatedCommandIDIsIdempotent(t *testing.T) {
	r := newRig(t, true, nil)
	ctx := context.Background()
	cmd := domain.Command{ID: "same", Line: domain.LineL2, On: true}
	if _, err := r.ctl.Submit(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	if res, _ := r.ctl.Wait(ctx, "same"); res.Status != domain.CommandConfirmed {
		t.Fatalf("ON: %+v", res)
	}
	r.run(t, domain.LineL2, false)
	// Повтор старой ON с тем же ID не должен снова включить линию.
	res, err := r.ctl.Submit(ctx, cmd)
	if err != nil || res.Status != domain.CommandConfirmed {
		t.Fatalf("resubmit: %+v %v", res, err)
	}
	time.Sleep(100 * time.Millisecond)
	if l := r.line(t, domain.LineL2); l.State != domain.LineOff || l.Relay.Value {
		t.Fatalf("stale ON replayed: %+v", l)
	}
}

func TestWatchdogReleasesOutputsWhenLoopHangs(t *testing.T) {
	r := newRig(t, true, nil)
	if res := r.run(t, domain.LineL1, true); res.Status != domain.CommandConfirmed {
		t.Fatalf("ON: %+v", res)
	}
	hang := r.post.Timing.LeaseTTL + time.Duration(safety.PollTimeoutS)*time.Second + 300*time.Millisecond
	if err := r.ctl.DebugHang(context.Background(), hang); err != nil {
		t.Fatal(err)
	}
	st := r.sim.Stats()
	if st.SafeEntries != 1 {
		t.Fatalf("safe mode entries = %d, want 1", st.SafeEntries)
	}
	l := r.waitState(t, domain.LineL1, domain.LineFault)
	if !strings.Contains(l.Reason, "outputs found released") {
		t.Fatalf("reason: %s", l.Reason)
	}
	// Нет самопроизвольного ON после восстановления.
	time.Sleep(200 * time.Millisecond)
	if r.sim.Stats().Coils[0] {
		t.Fatal("K1 switched on again after safe mode")
	}
	if err := r.ctl.Reset(context.Background(), domain.LineL1); err != nil {
		t.Fatal(err)
	}
	if res := r.run(t, domain.LineL1, true); res.Status != domain.CommandConfirmed {
		t.Fatalf("ON after reset: %+v", res)
	}
}

func TestContactorDidNotOperate(t *testing.T) {
	r := newRig(t, true, nil)
	r.sim.SetFault(1, sim.FaultStuckReleased)
	res := r.run(t, domain.LineL1, true)
	if res.Status != domain.CommandFailed || !strings.Contains(res.Reason, "contactor did not operate") {
		t.Fatalf("ON: %s %s", res.Status, res.Reason)
	}
	r.waitState(t, domain.LineL1, domain.LineFault)
	time.Sleep(100 * time.Millisecond)
	if r.sim.Stats().Coils[0] {
		t.Fatal("K1 left ON after failed confirmation")
	}
}

func TestContactorDidNotRelease(t *testing.T) {
	r := newRig(t, true, nil)
	r.run(t, domain.LineL2, true)
	r.sim.SetFault(2, sim.FaultStuckEngaged)
	res := r.run(t, domain.LineL2, false)
	if res.Status != domain.CommandFailed || !strings.Contains(res.Reason, "did not release") {
		t.Fatalf("OFF: %s %s", res.Status, res.Reason)
	}
	r.waitState(t, domain.LineL2, domain.LineFault)
}

func TestOpenNCBeforeOnForbidsEnable(t *testing.T) {
	r := newRig(t, true, nil)
	r.sim.SetFault(3, sim.FaultNCOpen)
	r.waitState(t, domain.LineL3, domain.LineFault)
	if res := r.run(t, domain.LineL3, true); res.Status != domain.CommandFailed {
		t.Fatalf("ON with open NC: %+v", res)
	}
	if r.sim.Stats().Coils[2] {
		t.Fatal("K3 switched on")
	}
}

func TestSafetyMismatchForbidsEnable(t *testing.T) {
	r := newRig(t, false, nil)
	res := r.run(t, domain.LineL1, true)
	if res.Status != domain.CommandFailed || !strings.Contains(res.Reason, "safety config") {
		t.Fatalf("ON: %s %s", res.Status, res.Reason)
	}
	if r.sim.Stats().Coils[0] {
		t.Fatal("K1 switched on with unverified config")
	}
}

func TestModeInterlock(t *testing.T) {
	r := newRig(t, true, nil)
	if res := r.run(t, domain.Line3P, true); res.Status != domain.CommandFailed {
		t.Fatalf("3P in mode 3x1: %+v", res)
	}
	r.run(t, domain.LineL1, true)
	if err := r.ctl.SetMode(context.Background(), domain.Mode1x3); err == nil {
		t.Fatal("mode switch allowed while L1 is ON")
	}
	r.run(t, domain.LineL1, false)
	if err := r.ctl.SetMode(context.Background(), domain.Mode1x3); err != nil {
		t.Fatal(err)
	}
	if res := r.run(t, domain.LineL1, true); res.Status != domain.CommandFailed {
		t.Fatalf("L1 in mode 1x3: %+v", res)
	}
	if res := r.run(t, domain.Line3P, true); res.Status != domain.CommandConfirmed {
		t.Fatalf("3P in mode 1x3: %+v", res)
	}
}

func TestOverModbusTCP(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	var stop func() error
	r := newRig(t, true, func(m *sim.MR6C) mr6c.Dialer {
		if stop, err = m.Serve("tcp://" + addr); err != nil {
			t.Fatal(err)
		}
		return mr6c.TCPDialer(addr, m.UnitID, 100*time.Millisecond)
	})
	t.Cleanup(func() { _ = stop() })
	if res := r.run(t, domain.LineL1, true); res.Status != domain.CommandConfirmed {
		t.Fatalf("ON: %+v", res)
	}
	if res := r.run(t, domain.LineL1, false); res.Status != domain.CommandConfirmed {
		t.Fatalf("OFF: %+v", res)
	}
}
