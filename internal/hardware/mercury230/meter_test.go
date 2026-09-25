package mercury230

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kmlebedev/EVcomm/internal/rtu"
	"github.com/kmlebedev/EVcomm/internal/sim"
)

var testTiming = rtu.Timing{Response: 150 * time.Millisecond, FrameGap: time.Millisecond, StatusTail: 10 * time.Millisecond}

var (
	pwLevel1 = [6]byte{1, 1, 1, 1, 1, 1}
	pwLevel2 = [6]byte{2, 2, 2, 2, 2, 2}
)

func newSim(t *testing.T, mod func(*sim.Mercury230Config)) *sim.Mercury230 {
	t.Helper()
	cfg := sim.DefaultMercury230Config()
	cfg.ResponseDelay = time.Millisecond
	if mod != nil {
		mod(&cfg)
	}
	m, err := sim.NewMercury230(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newMeter(t *testing.T, s *sim.Mercury230, cfg MeterConfig) *Meter {
	t.Helper()
	conn, err := s.Dialer(testTiming)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if cfg.Address == 0 {
		cfg.Address = 145
	}
	if cfg.AccessLevel == 0 {
		cfg.AccessLevel, cfg.Password = 1, pwLevel1
	}
	return NewMeter(conn, cfg)
}

func TestMeterSessionAgainstSim(t *testing.T) {
	s := newSim(t, nil)
	s.SetEnergy([3]float64{1000, 2000, 3000})
	s.SetLoad([3]float64{7360, 0, -12.34})
	var trace []Exchange
	m := newMeter(t, s, MeterConfig{AccessLevel: 1, Password: pwLevel1, Trace: func(ex Exchange) { trace = append(trace, ex) }})
	ctx := context.Background()

	if err := m.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := m.ReadSerial(ctx)
	if err != nil || id.Serial != "32874766" {
		t.Fatalf("serial = %+v, %v", id, err)
	}
	// Без канала энергия недоступна: Meter сам открывает канал по коду 5.
	e, ex, err := m.ReadPhaseEnergy(ctx)
	if err != nil || e[0] != 1000 || e[1] != 2000 || e[2] != 3000 {
		t.Fatalf("phase energy = %v, %v", e, err)
	}
	if !rtu.CheckCRC(ex.Resp) || len(ex.Resp) != 15 || !bytes.Equal(ex.Req, []byte{0x91, 0x05, 0x60, 0x00, 0x14, 0xD9}) {
		t.Fatalf("raw exchange: % X / % X", ex.Req, ex.Resp)
	}
	var opened bool
	for _, x := range trace {
		if x.Request == "open" {
			opened = true
			if bytes.Contains(x.Req[3:9], []byte{1}) {
				t.Fatalf("password not masked in trace: % X", x.Req)
			}
		}
	}
	if !opened {
		t.Fatal("channel was not reopened after status 5")
	}

	full, err := m.ReadIdentity(ctx)
	if err != nil || full.Firmware != "9.0.0" || !full.Variant.PhaseEnergy() {
		t.Fatalf("identity = %+v, %v", full, err)
	}
	pw, err := m.ReadGroupPower(ctx, 0)
	if err != nil || pw.PhaseCW != [3]int32{736000, 0, -1234} || pw.TotalCW != 736000-1234 {
		t.Fatalf("group power = %+v, %v", pw, err)
	}
	pp, err := m.ReadPhasePower(ctx)
	if err != nil || pp != pw {
		t.Fatalf("phase power = %+v, want %+v (%v)", pp, pw, err)
	}
	te, _, err := m.ReadTotalEnergy(ctx)
	if err != nil || te.APlus != 6000 {
		t.Fatalf("total = %+v, %v", te, err)
	}
	if err := m.CloseChannel(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestMeterStatusCodes(t *testing.T) {
	s := newSim(t, func(c *sim.Mercury230Config) {
		c.PowerLevel = 2
		c.GroupPowerBWRI = 0x01
		c.NoPhaseEnergy = true
		c.NoPassport = true
	})
	m := newMeter(t, s, MeterConfig{})
	ctx := context.Background()

	if err := m.OpenWith(ctx, 1, [6]byte{0x31, 0x31, 0x31, 0x31, 0x31, 0x31}); !IsStatus(err, StatusAccessDenied) {
		t.Fatalf("wrong password: %v", err)
	}
	if err := m.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadGroupPower(ctx, 0x00); !IsStatus(err, StatusInvalidCommand) {
		t.Fatalf("unsupported BWRI: %v", err)
	}
	if _, err := m.ReadGroupPower(ctx, 0x01); !IsStatus(err, StatusAccessDenied) {
		t.Fatalf("power at level 1: %v", err)
	}
	if _, _, err := m.ReadPhaseEnergy(ctx); !errors.Is(err, ErrMasked) {
		t.Fatalf("masked phase energy: %v", err)
	}
	id, err := m.ReadIdentity(ctx) // 08 01 00 → код 1 → 08 00 + 08 12
	if err != nil || id.Serial != "32874766" || !id.HasVariant || id.Variant.PhaseEnergy() || id.Firmware != "" {
		t.Fatalf("fallback identity = %+v, %v", id, err)
	}
	if err := m.OpenWith(ctx, 2, pwLevel2); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadGroupPower(ctx, 0x01); err != nil {
		t.Fatalf("power at level 2: %v", err)
	}
	s.SetInternalError(true)
	if _, _, err := m.ReadTotalEnergy(ctx); !IsStatus(err, StatusInternal) {
		t.Fatalf("internal error: %v", err)
	}
}

func TestMeterSilence(t *testing.T) {
	s := newSim(t, nil)
	m := newMeter(t, s, MeterConfig{})
	s.SetSilent(true)
	start := time.Now()
	_, err := m.ReadSerial(context.Background())
	if !errors.Is(err, ErrTimeout) || Classify(err) != KindLink {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 2*testTiming.Response {
		t.Fatalf("timeout retried: %s", d) // таймаут не повторяется, его считает адаптер
	}
}

// corruptConn портит первый ответ: Meter должен повторить запрос один раз.
type corruptConn struct {
	rtu.Conn
	left int
}

func (c *corruptConn) Transact(ctx context.Context, req []byte, n int) ([]byte, error) {
	b, err := c.Conn.Transact(ctx, req, n)
	if err == nil && c.left > 0 {
		c.left--
		b = append([]byte(nil), b...)
		b[len(b)-1] ^= 0xFF
	}
	return b, err
}

func TestMeterRetriesCorruptFrameOnce(t *testing.T) {
	s := newSim(t, nil)
	conn, err := s.Dialer(testTiming)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	cc := &corruptConn{Conn: conn, left: 1}
	m := NewMeter(cc, MeterConfig{Address: 145})
	if _, err := m.ReadSerial(context.Background()); err != nil {
		t.Fatalf("one corrupt frame: %v", err)
	}
	cc.left = 2
	if _, err := m.ReadSerial(context.Background()); !errors.Is(err, ErrCRC) {
		t.Fatalf("two corrupt frames: %v", err)
	}
}
