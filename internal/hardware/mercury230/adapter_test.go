package mercury230

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kmlebedev/EVcomm/internal/domain"
	"github.com/kmlebedev/EVcomm/internal/rtu"
	"github.com/kmlebedev/EVcomm/internal/sim"
)

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func testLogger() *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func adapterConfig(dial rtu.Dialer) Config {
	return Config{
		Name:           "test",
		Dial:           dial,
		Address:        145,
		AccessLevel:    1,
		Password:       pwLevel1,
		ExpectedSerial: "32874766",
		PowerPeriod:    20 * time.Millisecond,
		EnergyPeriod:   50 * time.Millisecond,
		IdentityPeriod: 100 * time.Millisecond,
		ReconnectMin:   10 * time.Millisecond,
		ReconnectMax:   50 * time.Millisecond,
		Logger:         testLogger(),
	}
}

func startAdapter(t *testing.T, cfg Config) *Adapter {
	t.Helper()
	a := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return a
}

func online(a *Adapter) bool { return a.MeterStatus().State == StateOnline }

func TestAdapterPollsAndSnapshots(t *testing.T) {
	s := newSim(t, nil)
	s.SetEnergy([3]float64{1000, 2000, 3000})
	s.SetLoad([3]float64{3000, 0, 1500})
	cfg := adapterConfig(s.Dialer(testTiming))
	// Фаза счётчика 1 подключена к линии L2, фаза 2 — к L1 (монтажная схема).
	cfg.PhaseMap = [3]domain.LineID{domain.LineL2, domain.LineL1, domain.LineL3}
	a := startAdapter(t, cfg)
	waitFor(t, "online", 2*time.Second, func() bool {
		return online(a) && !a.LatestPower().ObservedAt.IsZero() && !a.LatestEnergy().ObservedAt.IsZero()
	})

	st := a.MeterStatus()
	if !st.Capabilities.PhaseEnergy || !st.Capabilities.GroupPower || !st.Capabilities.PowerAllowed || st.Mismatch {
		t.Fatalf("status = %+v", st)
	}
	if p := a.Power(domain.LineL2); p.Value != 3000 || p.Quality != domain.QualityFresh || p.Source != "mercury230:32874766" {
		t.Fatalf("P(L2) = %+v", p)
	}
	if p := a.Power(domain.Line3P); p.Value != 4500 {
		t.Fatalf("P(3P) = %+v", p)
	}
	if e := a.Energy(domain.LineL1); e.Value != 2000 || e.Quality != domain.QualityFresh {
		t.Fatalf("AP(L1) = %+v", e)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ev, err := a.EnergySnapshot(ctx, domain.LineL3)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Wh < 3000 || ev.Register != "A+ phase 3" || ev.Serial != "32874766" || len(ev.RawResp) != 15 || !rtu.CheckCRC(ev.RawResp) {
		t.Fatalf("evidence L3 = %+v", ev)
	}
	ev, err = a.EnergySnapshot(ctx, domain.Line3P)
	if err != nil || ev.Register != "A+ total" || ev.Wh < 6000 || len(ev.RawResp) != 19 {
		t.Fatalf("evidence 3P = %+v, %v", ev, err)
	}
	if _, err := a.EnergySnapshot(ctx, "L9"); !errors.Is(err, ErrLineNotMetered) {
		t.Fatalf("unknown line: %v", err)
	}
	// Прирост в пределах дискретности проходит проверки, скачок — disputed.
	before := a.LatestEnergy()
	s.SetEnergy([3]float64{float64(before.PhaseWh[0]) + 1, float64(before.PhaseWh[1]), float64(before.PhaseWh[2])})
	waitFor(t, "energy update", time.Second, func() bool { return a.LatestEnergy().PhaseWh[0] == before.PhaseWh[0]+1 })
	if e := a.LatestEnergy(); e.Quality != domain.QualityFresh {
		t.Fatalf("energy disputed: %v", e.Issues)
	}
	s.SetEnergy([3]float64{float64(before.PhaseWh[0]) + 1000, float64(before.PhaseWh[1]), float64(before.PhaseWh[2])})
	waitFor(t, "implausible jump", time.Second, func() bool { return a.Energy(domain.LineL2).Quality == domain.QualityDisputed })
}

func TestAdapterFallbacks(t *testing.T) {
	s := newSim(t, func(c *sim.Mercury230Config) {
		c.GroupPowerBWRI = -1 // 08 16 не поддержан → 3 × 08 11
		c.NoPhaseEnergy = true
	})
	s.SetLoad([3]float64{100, 200, 300})
	a := startAdapter(t, adapterConfig(s.Dialer(testTiming)))
	waitFor(t, "online", 2*time.Second, func() bool { return online(a) && !a.LatestEnergy().ObservedAt.IsZero() })

	caps := a.MeterStatus().Capabilities
	if caps.PhaseEnergy || caps.GroupPower || !caps.PowerAllowed {
		t.Fatalf("caps = %+v", caps)
	}
	waitFor(t, "power", time.Second, func() bool { return a.Power(domain.LineL3).Value == 300 })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := a.EnergySnapshot(ctx, domain.LineL1); !errors.Is(err, ErrPhaseEnergyUnsupported) {
		t.Fatalf("L1 snapshot: %v", err)
	}
	if _, err := a.EnergySnapshot(ctx, domain.Line3P); err != nil {
		t.Fatalf("3P snapshot: %v", err)
	}
	if e := a.Energy(domain.LineL1); e.Quality != domain.QualityUnknown {
		t.Fatalf("AP(L1) without phase accounting = %+v", e)
	}
	if !hasAlarm(a, "per-phase A+") {
		t.Fatalf("alarms = %v", a.MeterStatus().Alarms)
	}
}

func hasAlarm(a *Adapter, sub string) bool {
	for _, al := range a.MeterStatus().Alarms {
		if strings.Contains(al, sub) {
			return true
		}
	}
	return false
}

func TestAdapterPowerNeedsLevel2(t *testing.T) {
	s := newSim(t, func(c *sim.Mercury230Config) { c.PowerLevel = 2 })
	a := startAdapter(t, adapterConfig(s.Dialer(testTiming)))
	waitFor(t, "energy", 2*time.Second, func() bool { return online(a) && !a.LatestEnergy().ObservedAt.IsZero() })
	if caps := a.MeterStatus().Capabilities; caps.PowerAllowed {
		t.Fatalf("caps = %+v", caps)
	}
	if !hasAlarm(a, "access level") || a.Power(domain.LineL1).Quality != domain.QualityUnknown {
		t.Fatalf("alarms = %v, P = %+v", a.MeterStatus().Alarms, a.Power(domain.LineL1))
	}
}

func TestAdapterWrongPassword(t *testing.T) {
	s := newSim(t, nil)
	cfg := adapterConfig(s.Dialer(testTiming))
	cfg.Password = [6]byte{0x31, 0x31, 0x31, 0x31, 0x31, 0x31}
	a := startAdapter(t, cfg)
	waitFor(t, "config error", 2*time.Second, func() bool { return a.MeterStatus().State == StateConfigError })
	if !strings.Contains(a.MeterStatus().LastError, "access level") {
		t.Fatalf("status = %+v", a.MeterStatus())
	}
}

func TestAdapterSerialMismatchDisputesEnergy(t *testing.T) {
	s := newSim(t, nil)
	cfg := adapterConfig(s.Dialer(testTiming))
	cfg.ExpectedSerial = "00000001"
	a := startAdapter(t, cfg)
	waitFor(t, "energy", 2*time.Second, func() bool { return !a.LatestEnergy().ObservedAt.IsZero() })
	if !a.Status().Mismatch || a.Energy(domain.LineL1).Quality != domain.QualityDisputed {
		t.Fatalf("status %+v, AP %+v", a.Status(), a.Energy(domain.LineL1))
	}
}

// Замена счётчика во время работы: номер перечитывается раз в IdentityPeriod.
func TestAdapterSerialChangeAndRegisterReset(t *testing.T) {
	s := newSim(t, nil)
	s.SetEnergy([3]float64{5000, 5000, 5000})
	a := startAdapter(t, adapterConfig(s.Dialer(testTiming)))
	waitFor(t, "energy", 2*time.Second, func() bool { return a.LatestEnergy().PhaseWh[0] == 5000 })

	s.SetEnergy([3]float64{0, 0, 0}) // сброс регистров
	waitFor(t, "disputed after reset", time.Second, func() bool {
		e := a.LatestEnergy()
		return e.Quality == domain.QualityDisputed && strings.Contains(strings.Join(e.Issues, ";"), "decreased")
	})
	if err := s.SetSerial("11223344"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "mismatch", time.Second, func() bool { return a.Status().Mismatch })
}

func TestAdapterChannelExpiry(t *testing.T) {
	s := newSim(t, nil)
	a := startAdapter(t, adapterConfig(s.Dialer(testTiming)))
	waitFor(t, "online", 2*time.Second, func() bool { return online(a) && !a.LatestEnergy().ObservedAt.IsZero() })
	reconnects := a.MeterStatus().Reconnects
	s.ExpireChannel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// Снимок не теряется: код 5 → 01h → повтор.
	if _, err := a.EnergySnapshot(ctx, domain.LineL1); err != nil {
		t.Fatal(err)
	}
	if st := a.MeterStatus(); st.Reconnects != reconnects || st.State != StateOnline {
		t.Fatalf("status = %+v", st)
	}
}

func TestAdapterSilenceReconnects(t *testing.T) {
	s := newSim(t, nil)
	cfg := adapterConfig(s.Dialer(testTiming))
	cfg.PowerStaleAfter = 100 * time.Millisecond
	a := startAdapter(t, cfg)
	waitFor(t, "online", 2*time.Second, func() bool { return online(a) && !a.LatestPower().ObservedAt.IsZero() })

	s.SetSilent(true)
	waitFor(t, "no response", 3*time.Second, func() bool {
		st := a.MeterStatus()
		return !st.MeterOK && (st.State == StateNoResponse || st.State == StateConnecting)
	})
	waitFor(t, "stale power", time.Second, func() bool { return a.Power(domain.LineL1).Quality == domain.QualityStale })
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := a.EnergySnapshot(ctx, domain.LineL1); err == nil {
		t.Fatal("snapshot succeeded while meter is silent")
	}

	s.SetSilent(false)
	waitFor(t, "back online", 3*time.Second, func() bool { return online(a) && a.Power(domain.LineL1).Quality == domain.QualityFresh })
	if a.MeterStatus().Reconnects < 2 {
		t.Fatalf("reconnects = %d", a.MeterStatus().Reconnects)
	}
}

func TestAdapterNegativePowerAlarm(t *testing.T) {
	s := newSim(t, nil)
	s.SetLoad([3]float64{-50, 10, 0})
	a := startAdapter(t, adapterConfig(s.Dialer(testTiming)))
	waitFor(t, "alarm", 2*time.Second, func() bool { return hasAlarm(a, "negative active power on meter phase 1") })
	s.SetLoad([3]float64{50, 10, 0})
	waitFor(t, "alarm cleared", time.Second, func() bool { return !hasAlarm(a, "negative") })
}

// Через TCP, как с WB-MGE: сегментированный ответ, второй клиент отклоняется,
// при остановке канал закрывается и слот моста освобождается.
func TestAdapterOverTCPGateway(t *testing.T) {
	s := newSim(t, func(c *sim.Mercury230Config) {
		c.Segment, c.SegmentDelay = 4, 2*time.Millisecond
		c.SlotReleaseDelay = 50 * time.Millisecond // FIN обрабатывается не мгновенно
	})
	addr, stop, err := s.Serve("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop() }()
	dial := rtu.TCPDialer(addr.String(), testTiming)

	a := New(adapterConfig(dial))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	waitFor(t, "online", 2*time.Second, func() bool { return online(a) && !a.LatestEnergy().ObservedAt.IsZero() })

	second := New(adapterConfig(dial))
	sctx, scancel := context.WithCancel(context.Background())
	sdone := make(chan struct{})
	go func() { _ = second.Run(sctx); close(sdone) }()
	waitFor(t, "second client busy", 2*time.Second, func() bool { return second.MeterStatus().State == StateGatewayBusy })
	scancel()
	<-sdone
	if !online(a) {
		t.Fatalf("first client displaced: %+v", a.MeterStatus())
	}

	cancel()
	<-done
	// Слот освобождается, как только мост обработает FIN: 02h и Close при остановке.
	// Подключение, пришедшее раньше обработки FIN, мост ещё отклоняет — отсюда повторы.
	var lastErr error
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		c, err := net.DialTimeout("tcp", addr.String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		m := NewMeter(rtu.NewConn(c, testTiming), MeterConfig{Address: 145})
		_, lastErr = m.ReadSerial(context.Background())
		_ = c.Close()
		if lastErr == nil {
			return
		}
		if !errors.Is(lastErr, rtu.ErrGatewayBusy) {
			t.Fatalf("after stop: %v", lastErr)
		}
	}
	t.Fatalf("slot not released within 1s: %v", lastErr)
}
