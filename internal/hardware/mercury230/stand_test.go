package mercury230

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/kmlebedev/EVcomm/internal/domain"
	"github.com/kmlebedev/EVcomm/internal/rtu"
)

// TestStand — адаптер против реального счётчика через WB-MGE. Без MERCURY230_GATEWAY пропускается.
//
//	MERCURY230_GATEWAY=192.168.1.50:503 MERCURY230_ADDRESS=145 MERCURY230_PASSWORD=010101010101 \
//	MERCURY230_SERIAL=32874766 MERCURY230_LEVEL=1 go test ./internal/hardware/mercury230 -run TestStand -v
//
// Порт моста на время теста должен быть свободен: wb-mqtt-serial и другие мастера остановлены.
func TestStand(t *testing.T) {
	gw := os.Getenv("MERCURY230_GATEWAY")
	if gw == "" {
		t.Skip("MERCURY230_GATEWAY not set: stand test skipped")
	}
	addr, err := strconv.Atoi(os.Getenv("MERCURY230_ADDRESS"))
	if err != nil || addr < 1 || addr > 240 {
		t.Fatal("MERCURY230_ADDRESS must be 1..240")
	}
	pw, err := ParsePassword(os.Getenv("MERCURY230_PASSWORD"))
	if err != nil {
		t.Fatal("MERCURY230_PASSWORD: ", err)
	}
	level := uint8(1)
	if os.Getenv("MERCURY230_LEVEL") == "2" {
		level = 2
	}
	bwri := byte(0)
	if os.Getenv("MERCURY230_BWRI") == "1" {
		bwri = 1
	}

	cfg := Config{
		Name: "stand", Dial: rtu.TCPDialer(gw, rtu.DefaultTiming()),
		Address: uint8(addr), AccessLevel: level, Password: pw, GroupPowerBWRI: bwri,
		ExpectedSerial: os.Getenv("MERCURY230_SERIAL"),
		PowerPeriod:    time.Second, EnergyPeriod: 5 * time.Second,
		ReconnectMin: time.Second, ReconnectMax: 5 * time.Second,
		Logger: testLogger(),
	}
	a := startAdapter(t, cfg)
	waitFor(t, "meter online", 30*time.Second, func() bool { return online(a) && !a.LatestEnergy().ObservedAt.IsZero() })

	st := a.MeterStatus()
	t.Logf("identity: serial %s, made %s, firmware %q, variant % X", st.Identity.Serial,
		st.Identity.Manufactured.Format("2006-01-02"), st.Identity.Firmware, st.Identity.Variant[:])
	t.Logf("capabilities: %+v; alarms: %v", st.Capabilities, st.Alarms)
	if st.Mismatch {
		t.Errorf("serial %s differs from MERCURY230_SERIAL", st.Identity.Serial)
	}
	if !st.Capabilities.PhaseEnergy {
		t.Error("per-phase A+ (05 60 00) not supported by this meter")
	}
	if !st.Capabilities.PowerAllowed {
		t.Error("instantaneous power not available at this access level (try MERCURY230_LEVEL=2)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, line := range []domain.LineID{domain.LineL1, domain.LineL2, domain.LineL3, domain.Line3P} {
		ev, err := a.EnergySnapshot(ctx, line)
		if err != nil {
			t.Errorf("%s: %v", line, err)
			continue
		}
		t.Logf("%s: %s = %d Wh, quality %s %v, rx % X", line, ev.Register, ev.Wh, ev.Quality, ev.Issues, ev.RawResp)
		if ev.Quality != domain.QualityFresh || !rtu.CheckCRC(ev.RawResp) {
			t.Errorf("%s: evidence %+v", line, ev)
		}
	}
	if st.Capabilities.PowerAllowed {
		first := a.LatestPower().ObservedAt
		waitFor(t, "power updates", 5*time.Second, func() bool { return a.LatestPower().ObservedAt.Sub(first) >= 2*time.Second })
		for _, line := range []domain.LineID{domain.LineL1, domain.LineL2, domain.LineL3, domain.Line3P} {
			p := a.Power(line)
			t.Logf("P(%s) = %.2f W (%s)", line, p.Value, p.Quality)
			if p.Quality != domain.QualityFresh {
				t.Errorf("P(%s) quality %s", line, p.Quality)
			}
		}
	}
	t.Logf("transactions %d, errors %d, reconnects %d", a.MeterStatus().Transactions, a.MeterStatus().Errors, a.MeterStatus().Reconnects)
}
