package mercury230

import (
	"fmt"
	"time"

	"github.com/kmlebedev/EVcomm/internal/domain"
)

// PowerSnapshot — мгновенная активная мощность: для лимитов фаз и диагностики,
// не для расчётов.
type PowerSnapshot struct {
	TotalCW    int32    // сотые доли Вт, со знаком
	PhaseCW    [3]int32 // фазы счётчика 1…3
	ObservedAt time.Time
}

// EnergySnapshot — снимок энергии: основа расчёта сессии. Только целые Вт·ч.
type EnergySnapshot struct {
	Serial     string
	HasPhase   bool      // пофазный учёт поддержан, PhaseWh заполнены
	PhaseWh    [3]uint32 // A+ фаз 1…3 от сброса, сумма тарифов
	TotalWh    uint32    // A+ суммарно, для сверки и для трёхфазной линии
	ObservedAt time.Time // время хоста (NTP) по получении ответа
	// Сырые кадры для неизменяемого журнала: 05 60 00 и 05 00 00.
	PhaseReq, PhaseResp []byte
	TotalReq, TotalResp []byte
	Quality             domain.Quality
	Issues              []string // причины disputed
}

// ValidationConfig — пороги проверки показаний (раздел 6.5).
type ValidationConfig struct {
	MaxPhaseW   float64 // U·I_max линии: 230 В × 32 А
	Margin      float64 // запас к правдоподобному приросту
	SlackWh     uint32  // дискретность регистров при проверке прироста
	ReconcileWh uint32  // допуск |ΣΔAP − ΔTotal|
}

func DefaultValidation() ValidationConfig {
	return ValidationConfig{MaxPhaseW: 230 * 32, Margin: 1.2, SlackWh: 2, ReconcileWh: 5}
}

// Validator проверяет снимки энергии относительно предыдущего. Базой всегда
// становится последний снимок: сброс регистров или замена счётчика дают
// одно событие disputed, а не бесконечную цепочку.
type Validator struct {
	cfg  ValidationConfig
	prev EnergySnapshot
	has  bool
}

func NewValidator(cfg ValidationConfig) *Validator { return &Validator{cfg: cfg} }

func (v *Validator) Reset() { v.has = false }

// Check выставляет Quality и Issues снимка и запоминает его как базу.
func (v *Validator) Check(s *EnergySnapshot) {
	if s.Quality == "" {
		s.Quality = domain.QualityFresh
	}
	if v.has {
		s.Issues = append(s.Issues, v.compare(v.prev, *s)...)
	}
	if len(s.Issues) > 0 {
		s.Quality = domain.QualityDisputed
	}
	v.prev, v.has = *s, true
}

func (v *Validator) compare(prev, cur EnergySnapshot) []string {
	var issues []string
	if prev.Serial != cur.Serial {
		return append(issues, fmt.Sprintf("serial changed %q → %q", prev.Serial, cur.Serial))
	}
	dt := cur.ObservedAt.Sub(prev.ObservedAt)
	maxWh := func(phases float64) uint32 {
		if dt <= 0 {
			return ^uint32(0)
		}
		return uint32(phases*v.cfg.MaxPhaseW*dt.Hours()*v.cfg.Margin) + v.cfg.SlackWh
	}
	if cur.TotalWh < prev.TotalWh {
		issues = append(issues, fmt.Sprintf("A+ total decreased %d → %d Wh", prev.TotalWh, cur.TotalWh))
	} else if d := cur.TotalWh - prev.TotalWh; d > maxWh(3) {
		issues = append(issues, fmt.Sprintf("A+ total increment %d Wh in %s implausible (max %d)", d, dt.Round(time.Millisecond), maxWh(3)))
	}
	if !prev.HasPhase || !cur.HasPhase {
		return issues
	}
	var sum int64
	decreased := false
	for i := range cur.PhaseWh {
		if cur.PhaseWh[i] < prev.PhaseWh[i] {
			decreased = true
			issues = append(issues, fmt.Sprintf("A+ phase %d decreased %d → %d Wh", i+1, prev.PhaseWh[i], cur.PhaseWh[i]))
			continue
		}
		d := cur.PhaseWh[i] - prev.PhaseWh[i]
		sum += int64(d)
		if d > maxWh(1) {
			issues = append(issues, fmt.Sprintf("A+ phase %d increment %d Wh in %s implausible (max %d)", i+1, d, dt.Round(time.Millisecond), maxWh(1)))
		}
	}
	if !decreased && cur.TotalWh >= prev.TotalWh {
		dTotal := int64(cur.TotalWh - prev.TotalWh)
		if diff := sum - dTotal; diff > int64(v.cfg.ReconcileWh) || -diff > int64(v.cfg.ReconcileWh) {
			issues = append(issues, fmt.Sprintf("ΣΔAP %d Wh ≠ ΔA+ total %d Wh (tolerance %d)", sum, dTotal, v.cfg.ReconcileWh))
		}
	}
	return issues
}
