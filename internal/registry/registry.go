// Package registry — реестр постов: единственный источник конфигурации
// (IP шлюза, адрес MR6C, наличие K4, режим, тайминги, эталон настроек MR6C).
package registry

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kmlebedev/EVcomm/internal/domain"
)

// Timing — тайминги контура управления поста. Нулевые поля в посте берутся из defaults.
type Timing struct {
	LeaseTTL          time.Duration `yaml:"lease_ttl"`           // аренда, продлеваемая циклом управления
	TickPeriod        time.Duration `yaml:"tick_period"`         // такт цикла управления
	PollPeriod        time.Duration `yaml:"poll_period"`         // период опроса MR6C
	ModbusTimeout     time.Duration `yaml:"modbus_timeout"`      // таймаут одной транзакции
	StaleAfter        time.Duration `yaml:"stale_after"`         // возраст снимка, после которого он stale
	FeedbackTimeout   time.Duration `yaml:"feedback_timeout"`    // ожидание НЗ после записи
	MinSwitchInterval time.Duration `yaml:"min_switch_interval"` // минимум между переключениями линии
	ModeSwitchPause   time.Duration `yaml:"mode_switch_pause"`   // пауза после OFF группы перед сменой режима
	ReconnectMin      time.Duration `yaml:"reconnect_min"`
	ReconnectMax      time.Duration `yaml:"reconnect_max"`
}

// Safety — эталон настроек безопасного режима MR6C (раздел 5 дорожной карты).
type Safety struct {
	PollTimeoutS uint16       `yaml:"poll_timeout_s"` // регистр 8
	Source       SafetySource `yaml:"source"`         // регистр 19
}

// SafetySource — источник безопасного режима MR6C (регистр 19). Значение 1
// («только питание шины») отключает watchdog по таймауту опроса и не допускается.
type SafetySource string

const (
	SafetySourcePollTimeout    SafetySource = "poll_timeout"          // 2
	SafetySourcePollOrBusPower SafetySource = "poll_timeout_or_power" // 0
)

// Register возвращает значение регистра 19.
func (s SafetySource) Register() (uint16, bool) {
	switch s {
	case SafetySourcePollTimeout:
		return 2, true
	case SafetySourcePollOrBusPower:
		return 0, true
	}
	return 0, false
}

// Limits — токовые лимиты поста.
type Limits struct {
	PhaseMaxA float64 `yaml:"phase_max_a"` // меньшее из 60 А счётчика и номинала вводного автомата
	EVSEMaxA  float64 `yaml:"evse_max_a"`  // резерв на разрешённую линию
}

type Defaults struct {
	Timing `yaml:",inline"`
	Safety Safety `yaml:"safety"`
	Limits Limits `yaml:"limits"`
}

// InputRead — способ чтения входов MR6C. Сплошное чтение 0…7 захватывает адрес 6,
// которого может не быть у модуля; выбор по результатам этапа 1.
type InputRead string

const (
	InputReadContiguous InputRead = "contiguous" // FC02 0…7 одним запросом
	InputReadSplit      InputRead = "split"      // FC02 0…5 и 7
)

type Post struct {
	ID          domain.PostID `yaml:"id"`
	Gateway     string        `yaml:"gateway"`      // host:port порта RS485-1 шлюза (Modbus TCP, 502)
	MR6CAddress uint8         `yaml:"mr6c_address"` // адрес модуля на шине
	Mode        domain.Mode   `yaml:"mode"`
	HasK4       bool          `yaml:"has_k4"`
	InputRead   InputRead     `yaml:"input_read"`
	// RealState: прошивка MR6C ≥ 1.24.0 отдаёт фактическое состояние выходов (discrete 96…101).
	RealState *bool  `yaml:"real_state"`
	Timing    Timing `yaml:"timing"`
	Safety    Safety `yaml:"safety"`
	Limits    Limits `yaml:"limits"`
}

// Line — линия поста и её привязка к ресурсам MR6C.
type Line struct {
	ID       domain.LineID
	Output   int      // номер выхода K1…K6
	Feedback int      // номер входа MR6C с НЗ-контактом контактора
	Phases   []string // фазы ввода, которые нагружает линия
	Group    domain.Group
}

// Lines — фиксированная раскладка поста (раздел 3 дорожной карты, НЗ №1 → входы 1…4).
func (p Post) Lines() []Line {
	lines := []Line{
		{ID: domain.LineL1, Output: 1, Feedback: 1, Phases: []string{"L1"}, Group: domain.GroupSingle},
		{ID: domain.LineL2, Output: 2, Feedback: 2, Phases: []string{"L2"}, Group: domain.GroupSingle},
		{ID: domain.LineL3, Output: 3, Feedback: 3, Phases: []string{"L3"}, Group: domain.GroupSingle},
	}
	if p.HasK4 {
		lines = append(lines, Line{ID: domain.Line3P, Output: 4, Feedback: 4, Phases: []string{"L1", "L2", "L3"}, Group: domain.GroupThree})
	}
	return lines
}

func (p Post) RealStateSupported() bool { return p.RealState == nil || *p.RealState }

type Registry struct {
	Defaults Defaults `yaml:"defaults"`
	Posts    []Post   `yaml:"posts"`
}

// BuiltinDefaults — исходные предложения дорожной карты; окончательные значения утверждаются на этапе 0.
func BuiltinDefaults() Defaults {
	return Defaults{
		Timing: Timing{
			LeaseTTL:          time.Second,
			TickPeriod:        100 * time.Millisecond,
			PollPeriod:        100 * time.Millisecond,
			ModbusTimeout:     300 * time.Millisecond,
			StaleAfter:        time.Second,
			FeedbackTimeout:   500 * time.Millisecond,
			MinSwitchInterval: 2 * time.Second,
			ModeSwitchPause:   5 * time.Second,
			ReconnectMin:      200 * time.Millisecond,
			ReconnectMax:      10 * time.Second,
		},
		Safety: Safety{PollTimeoutS: 3, Source: SafetySourcePollTimeout},
		Limits: Limits{PhaseMaxA: 60, EVSEMaxA: 32},
	}
}

func Load(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func Parse(data []byte) (*Registry, error) {
	var r Registry
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	r.applyDefaults()
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *Registry) Post(id domain.PostID) (Post, bool) {
	for _, p := range r.Posts {
		if p.ID == id {
			return p, true
		}
	}
	return Post{}, false
}

func (r *Registry) applyDefaults() {
	r.Defaults.Timing = mergeTiming(r.Defaults.Timing, BuiltinDefaults().Timing)
	r.Defaults.Safety = mergeSafety(r.Defaults.Safety, BuiltinDefaults().Safety)
	r.Defaults.Limits = mergeLimits(r.Defaults.Limits, BuiltinDefaults().Limits)
	for i := range r.Posts {
		p := &r.Posts[i]
		p.Timing = mergeTiming(p.Timing, r.Defaults.Timing)
		p.Safety = mergeSafety(p.Safety, r.Defaults.Safety)
		p.Limits = mergeLimits(p.Limits, r.Defaults.Limits)
		if p.InputRead == "" {
			p.InputRead = InputReadContiguous
		}
		if p.Mode == "" {
			p.Mode = domain.Mode3x1
		}
	}
}

func (r *Registry) Validate() error {
	var errs []error
	seen := map[domain.PostID]bool{}
	for i, p := range r.Posts {
		fail := func(format string, a ...any) {
			errs = append(errs, fmt.Errorf("posts[%d] %q: %s", i, p.ID, fmt.Sprintf(format, a...)))
		}
		if p.ID == "" {
			fail("empty id")
		}
		if seen[p.ID] {
			fail("duplicate id")
		}
		seen[p.ID] = true
		if _, _, err := net.SplitHostPort(p.Gateway); err != nil {
			fail("gateway must be host:port: %v", err)
		}
		if p.MR6CAddress < 1 || p.MR6CAddress > 247 {
			fail("mr6c_address must be 1..247")
		}
		if !p.Mode.Valid() {
			fail("mode must be 3x1 or 1x3")
		}
		if p.Mode == domain.Mode1x3 && !p.HasK4 {
			fail("mode 1x3 requires has_k4")
		}
		if p.InputRead != InputReadContiguous && p.InputRead != InputReadSplit {
			fail("input_read must be contiguous or split")
		}
		if err := validateTiming(p.Timing, p.Safety); err != nil {
			fail("%v", err)
		}
		if _, ok := p.Safety.Source.Register(); !ok {
			fail("safety.source must be %q or %q", SafetySourcePollTimeout, SafetySourcePollOrBusPower)
		}
		if p.Limits.EVSEMaxA <= 0 || p.Limits.PhaseMaxA < p.Limits.EVSEMaxA {
			fail("limits: need 0 < evse_max_a <= phase_max_a")
		}
	}
	return errors.Join(errs...)
}

func validateTiming(t Timing, s Safety) error {
	pollTimeout := time.Duration(s.PollTimeoutS) * time.Second
	switch {
	case s.PollTimeoutS < 1:
		return errors.New("poll_timeout_s must be >= 1")
	case t.LeaseTTL >= pollTimeout:
		// Иначе цикл может «зависнуть» дольше таймаута модуля, а адаптер ещё будет слать пакеты.
		return fmt.Errorf("lease_ttl (%s) must be < poll_timeout_s (%s)", t.LeaseTTL, pollTimeout)
	case t.TickPeriod >= t.LeaseTTL || t.PollPeriod >= t.LeaseTTL:
		return errors.New("tick_period and poll_period must be < lease_ttl")
	case t.ModbusTimeout >= t.LeaseTTL:
		return errors.New("modbus_timeout must be < lease_ttl")
	case t.StaleAfter <= t.PollPeriod:
		return errors.New("stale_after must be > poll_period")
	case t.ReconnectMin <= 0 || t.ReconnectMax < t.ReconnectMin:
		return errors.New("need 0 < reconnect_min <= reconnect_max")
	}
	return nil
}

func mergeTiming(t, d Timing) Timing {
	def := func(v *time.Duration, dv time.Duration) {
		if *v == 0 {
			*v = dv
		}
	}
	def(&t.LeaseTTL, d.LeaseTTL)
	def(&t.TickPeriod, d.TickPeriod)
	def(&t.PollPeriod, d.PollPeriod)
	def(&t.ModbusTimeout, d.ModbusTimeout)
	def(&t.StaleAfter, d.StaleAfter)
	def(&t.FeedbackTimeout, d.FeedbackTimeout)
	def(&t.MinSwitchInterval, d.MinSwitchInterval)
	def(&t.ModeSwitchPause, d.ModeSwitchPause)
	def(&t.ReconnectMin, d.ReconnectMin)
	def(&t.ReconnectMax, d.ReconnectMax)
	return t
}

func mergeSafety(s, d Safety) Safety {
	if s.PollTimeoutS == 0 {
		s.PollTimeoutS = d.PollTimeoutS
	}
	if s.Source == "" {
		s.Source = d.Source
	}
	return s
}

func mergeLimits(l, d Limits) Limits {
	if l.PhaseMaxA == 0 {
		l.PhaseMaxA = d.PhaseMaxA
	}
	if l.EVSEMaxA == 0 {
		l.EVSEMaxA = d.EVSEMaxA
	}
	return l
}
