package registry

import (
	"strings"
	"testing"
	"time"

	"github.com/kmlebedev/EVcomm/internal/domain"
)

func TestLoadBenchConfig(t *testing.T) {
	r, err := Load("../../configs/bench.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := r.Post("bench01")
	if !ok {
		t.Fatal("bench01 not found")
	}
	if p.Timing.LeaseTTL != time.Second || p.Safety.PollTimeoutS != 3 || p.Safety.Source != SafetySourcePollTimeout {
		t.Fatalf("defaults not applied: %+v %+v", p.Timing, p.Safety)
	}
	if n := len(p.Lines()); n != 4 {
		t.Fatalf("lines = %d, want 4", n)
	}
	m := p.Meter
	if m == nil || m.Driver != MeterMercury230 || m.Address != 145 || m.Serial != "32874766" || m.ResponseTimeout != 300*time.Millisecond ||
		m.EnergyPeriod != 15*time.Second || len(m.PhaseMap) != 3 {
		t.Fatalf("meter: %+v", m)
	}
}

func TestMeterDefaultsAndPassword(t *testing.T) {
	r, err := Parse([]byte(`
defaults: {meter: {power_period: 2s}}
posts:
  - {id: p1, gateway: "10.0.0.1:502", mr6c_address: 1, meter: {driver: mercury230, gateway: "10.0.0.1:503", address: 7, password_env: TEST_MERCURY_PW}}
`))
	if err != nil {
		t.Fatal(err)
	}
	m := r.Posts[0].Meter
	if m.PowerPeriod != 2*time.Second || m.ResponseTimeout != 300*time.Millisecond || m.AccessLevel != 1 ||
		len(m.PhaseMap) != 3 || m.PhaseMap[0] != domain.LineL1 {
		t.Fatalf("meter: %+v", m)
	}
	if _, err := m.Password(); err == nil {
		t.Fatal("password from unset env accepted")
	}
	t.Setenv("TEST_MERCURY_PW", "313131313131")
	if pw, err := m.Password(); err != nil || pw != [6]byte{0x31, 0x31, 0x31, 0x31, 0x31, 0x31} {
		t.Fatalf("pw = % X, %v", pw, err)
	}
	t.Setenv("TEST_MERCURY_PW", "111111")
	if _, err := m.Password(); err == nil {
		t.Fatal("6-char password accepted: must be 6 raw bytes in hex")
	}
}

func TestPostOverridesDefaults(t *testing.T) {
	r, err := Parse([]byte(`
defaults: {lease_ttl: 800ms}
posts:
  - {id: p1, gateway: "10.0.0.1:502", mr6c_address: 3, timing: {feedback_timeout: 1s}, safety: {poll_timeout_s: 5}}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := r.Posts[0]
	if p.Timing.LeaseTTL != 800*time.Millisecond || p.Timing.FeedbackTimeout != time.Second || p.Timing.PollPeriod != 100*time.Millisecond {
		t.Fatalf("timing: %+v", p.Timing)
	}
	if p.Safety.PollTimeoutS != 5 || p.Safety.Source != SafetySourcePollTimeout || p.Mode != domain.Mode3x1 {
		t.Fatalf("post: %+v", p)
	}
	if len(p.Lines()) != 3 {
		t.Fatal("post without K4 must have 3 lines")
	}
}

func TestValidate(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"lease >= poll timeout": {`{id: p, gateway: "h:502", mr6c_address: 1, timing: {lease_ttl: 3s}}`, "lease_ttl"},
		"1x3 without K4":        {`{id: p, gateway: "h:502", mr6c_address: 1, mode: 1x3}`, "requires has_k4"},
		"bad gateway":           {`{id: p, gateway: "h", mr6c_address: 1}`, "host:port"},
		"bad address":           {`{id: p, gateway: "h:502", mr6c_address: 0}`, "mr6c_address"},
		"bus power only source": {`{id: p, gateway: "h:502", mr6c_address: 1, safety: {source: bus_power}}`, "safety.source"},
		"meter driver":          {`{id: p, gateway: "h:502", mr6c_address: 1, meter: {driver: modbus}}`, "driver"},
		"meter same port":       {`{id: p, gateway: "h:502", mr6c_address: 1, meter: {driver: mercury230, gateway: "h:502", address: 1, password_env: X}}`, "must differ"},
		"meter address":         {`{id: p, gateway: "h:502", mr6c_address: 1, meter: {driver: mercury230, gateway: "h:503", address: 254, password_env: X}}`, "1..240"},
		"meter password env":    {`{id: p, gateway: "h:502", mr6c_address: 1, meter: {driver: mercury230, gateway: "h:503", address: 1}}`, "password_env"},
		"meter phase map":       {`{id: p, gateway: "h:502", mr6c_address: 1, meter: {driver: mercury230, gateway: "h:503", address: 1, password_env: X, phase_map: [L1, L1, L3]}}`, "permutation"},
		"meter serial":          {`{id: p, gateway: "h:502", mr6c_address: 1, meter: {driver: mercury230, gateway: "h:503", address: 1, password_env: X, serial: "12ab"}}`, "8 digits"},
		"meter level":           {`{id: p, gateway: "h:502", mr6c_address: 1, meter: {driver: mercury230, gateway: "h:503", address: 1, password_env: X, access_level: 3}}`, "access_level"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte("posts:\n  - " + tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
