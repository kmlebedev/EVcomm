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
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte("posts:\n  - " + tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
