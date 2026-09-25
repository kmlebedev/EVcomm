package mercury230

import (
	"strings"
	"testing"
	"time"

	"github.com/kmlebedev/EVcomm/internal/domain"
)

func TestValidator(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	base := EnergySnapshot{Serial: "32874766", HasPhase: true, PhaseWh: [3]uint32{1000, 2000, 3000}, TotalWh: 6000, ObservedAt: t0}
	next := func(dt time.Duration, phase [3]uint32, total uint32) EnergySnapshot {
		s := base
		s.PhaseWh, s.TotalWh, s.ObservedAt = phase, total, t0.Add(dt)
		return s
	}
	for name, tc := range map[string]struct {
		cur  EnergySnapshot
		want string // пусто — fresh
	}{
		// 7,36 кВт на фазе 1 в течение 15 с — 30,7 Вт·ч.
		"normal growth":     {next(15*time.Second, [3]uint32{1030, 2000, 3001}, 6031), ""},
		"no change":         {next(15*time.Second, [3]uint32{1000, 2000, 3000}, 6000), ""},
		"phase decreased":   {next(15*time.Second, [3]uint32{999, 2000, 3000}, 6000), "phase 1 decreased"},
		"total decreased":   {next(15*time.Second, [3]uint32{1000, 2000, 3000}, 5000), "total decreased"},
		"implausible phase": {next(15*time.Second, [3]uint32{1100, 2000, 3000}, 6100), "phase 1 increment"},
		"reconcile":         {next(15*time.Second, [3]uint32{1020, 2000, 3000}, 6000), "ΣΔAP"},
		"serial changed":    {func() EnergySnapshot { s := next(time.Second, base.PhaseWh, 6000); s.Serial = "11111111"; return s }(), "serial changed"},
	} {
		t.Run(name, func(t *testing.T) {
			v := NewValidator(DefaultValidation())
			b := base
			v.Check(&b)
			if b.Quality != domain.QualityFresh {
				t.Fatalf("first snapshot: %s %v", b.Quality, b.Issues)
			}
			cur := tc.cur
			v.Check(&cur)
			if tc.want == "" {
				if cur.Quality != domain.QualityFresh || len(cur.Issues) != 0 {
					t.Fatalf("got %s %v", cur.Quality, cur.Issues)
				}
				return
			}
			if cur.Quality != domain.QualityDisputed || !strings.Contains(strings.Join(cur.Issues, "; "), tc.want) {
				t.Fatalf("got %s %v, want issue %q", cur.Quality, cur.Issues, tc.want)
			}
		})
	}
}

// После сброса регистров спор возникает один раз: база — последний снимок.
func TestValidatorRebasesAfterDispute(t *testing.T) {
	v := NewValidator(DefaultValidation())
	t0 := time.Now()
	s1 := EnergySnapshot{Serial: "1", HasPhase: true, PhaseWh: [3]uint32{500, 0, 0}, TotalWh: 500, ObservedAt: t0}
	s2 := EnergySnapshot{Serial: "1", HasPhase: true, PhaseWh: [3]uint32{0, 0, 0}, TotalWh: 0, ObservedAt: t0.Add(time.Second)}
	s3 := EnergySnapshot{Serial: "1", HasPhase: true, PhaseWh: [3]uint32{1, 0, 0}, TotalWh: 1, ObservedAt: t0.Add(2 * time.Second)}
	v.Check(&s1)
	v.Check(&s2)
	v.Check(&s3)
	if s2.Quality != domain.QualityDisputed || s3.Quality != domain.QualityFresh {
		t.Fatalf("s2 %s %v, s3 %s %v", s2.Quality, s2.Issues, s3.Quality, s3.Issues)
	}
}

// Без пофазного учёта проверяются только суммарные показания.
func TestValidatorTotalOnly(t *testing.T) {
	v := NewValidator(DefaultValidation())
	t0 := time.Now()
	s1 := EnergySnapshot{Serial: "1", TotalWh: 100, ObservedAt: t0}
	s2 := EnergySnapshot{Serial: "1", TotalWh: 190, ObservedAt: t0.Add(15 * time.Second)} // 3 × 7,36 кВт × 15 с ≈ 92 Вт·ч
	v.Check(&s1)
	v.Check(&s2)
	if s2.Quality != domain.QualityFresh {
		t.Fatalf("%s %v", s2.Quality, s2.Issues)
	}
}
