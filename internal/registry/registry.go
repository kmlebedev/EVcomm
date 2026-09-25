// Package registry — реестр постов: единственный источник конфигурации
// (IP шлюза, адрес MR6C, наличие K4, режим, тайминги, эталон настроек MR6C,
// счётчик поста).
package registry

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
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
	Safety Safety      `yaml:"safety"`
	Limits Limits      `yaml:"limits"`
	Meter  MeterTiming `yaml:"meter"`
}

// MeterDriver — способ снятия показаний счётчика поста.
type MeterDriver string

const (
	// MeterMercury230 — нативный Go через прозрачный мост WB-MGE (RS485-2, TCP 503).
	// Порт моста занимает Go: в wb-mqtt-serial его включать нельзя (один мастер).
	MeterMercury230 MeterDriver = "mercury230"
	MeterWBMQTT     MeterDriver = "wbmqtt"
)

// MeterTiming — тайминги опроса счётчика. Нулевые поля в посте берутся из defaults.meter.
type MeterTiming struct {
	ConnectTimeout  time.Duration `yaml:"connect_timeout"`
	ResponseTimeout time.Duration `yaml:"response_timeout"` // 9600: 150 мс ответа + передача + запас
	FrameGap        time.Duration `yaml:"frame_gap"`        // системный таймаут счётчика, 5 мс при 9600
	StatusTail      time.Duration `yaml:"status_tail"`      // ожидание «хвоста» после 4 байт с верным CRC
	PowerPeriod     time.Duration `yaml:"power_period"`
	EnergyPeriod    time.Duration `yaml:"energy_period"`
	IdentityPeriod  time.Duration `yaml:"identity_period"`
	ReconnectMin    time.Duration `yaml:"reconnect_min"`
	ReconnectMax    time.Duration `yaml:"reconnect_max"`
}

// Meter — счётчик поста.
type Meter struct {
	Driver      MeterDriver `yaml:"driver"`
	Gateway     string      `yaml:"gateway"` // host:port порта RS485-2 шлюза (прозрачный мост, 503)
	Address     uint8       `yaml:"address"` // 1…240; 0 и FEh не используются
	Serial      string      `yaml:"serial"`  // из паспорта, сверяется при подключении
	AccessLevel uint8       `yaml:"access_level"`
	// PasswordEnv — переменная окружения с паролем: 12 hex-символов (6 сырых байт).
	// Пароль в git не хранится.
	PasswordEnv string `yaml:"password_env"`
	// GroupPowerBWRI — BWRI группового чтения мощности 08 16 (0 или 1, по итогам стенда).
	GroupPowerBWRI uint8 `yaml:"group_power_bwri"`
	// PhaseMap — фаза счётчика 1…3 → линия поста, по монтажной схеме.
	PhaseMap    []domain.LineID `yaml:"phase_map"`
	MeterTiming `yaml:",inline"`
}

// Password читает пароль из окружения.
func (m Meter) Password() ([6]byte, error) {
	var pw [6]byte
	v, ok := os.LookupEnv(m.PasswordEnv)
	if !ok {
		return pw, fmt.Errorf("meter password: environment variable %s is not set", m.PasswordEnv)
	}
	b, err := hex.DecodeString(strings.TrimSpace(v))
	if err != nil || len(b) != len(pw) {
		return pw, fmt.Errorf("meter password: %s must hold 12 hex chars (6 raw bytes)", m.PasswordEnv)
	}
	copy(pw[:], b)
	return pw, nil
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
	Meter     *Meter `yaml:"meter"` // nil — счётчик не опрашивается
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
		Meter: MeterTiming{
			ConnectTimeout:  3 * time.Second,
			ResponseTimeout: 300 * time.Millisecond,
			FrameGap:        5 * time.Millisecond,
			StatusTail:      20 * time.Millisecond,
			PowerPeriod:     time.Second,
			EnergyPeriod:    15 * time.Second,
			IdentityPeriod:  time.Hour,
			ReconnectMin:    500 * time.Millisecond,
			ReconnectMax:    30 * time.Second,
		},
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
	r.Defaults.Meter = mergeMeterTiming(r.Defaults.Meter, BuiltinDefaults().Meter)
	for i := range r.Posts {
		p := &r.Posts[i]
		if m := p.Meter; m != nil {
			m.MeterTiming = mergeMeterTiming(m.MeterTiming, r.Defaults.Meter)
			if m.AccessLevel == 0 {
				m.AccessLevel = 1
			}
			if len(m.PhaseMap) == 0 {
				m.PhaseMap = []domain.LineID{domain.LineL1, domain.LineL2, domain.LineL3}
			}
		}
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
		if p.Meter != nil {
			if err := validateMeter(*p.Meter, p.Gateway); err != nil {
				fail("meter: %v", err)
			}
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

func validateMeter(m Meter, mr6cGateway string) error {
	switch m.Driver {
	case MeterMercury230:
	case MeterWBMQTT:
		return nil // параметры wb-mqtt-serial — этап 2 дорожной карты
	default:
		return fmt.Errorf("driver must be %q or %q", MeterMercury230, MeterWBMQTT)
	}
	if _, _, err := net.SplitHostPort(m.Gateway); err != nil {
		return fmt.Errorf("gateway must be host:port: %v", err)
	}
	if m.Gateway == mr6cGateway {
		return errors.New("gateway must differ from the MR6C gateway port (RS485-2 transparent bridge, not RS485-1)")
	}
	if m.Address < 1 || m.Address > 240 {
		return errors.New("address must be 1..240")
	}
	if m.AccessLevel != 1 && m.AccessLevel != 2 {
		return errors.New("access_level must be 1 or 2")
	}
	if m.PasswordEnv == "" {
		return errors.New("password_env is required")
	}
	if m.GroupPowerBWRI > 1 {
		return errors.New("group_power_bwri must be 0 or 1")
	}
	if m.Serial != "" {
		if len(m.Serial) != 8 || strings.Trim(m.Serial, "0123456789") != "" {
			return errors.New("serial must be 8 digits")
		}
	}
	if len(m.PhaseMap) != 3 {
		return errors.New("phase_map must list 3 lines")
	}
	seen := map[domain.LineID]bool{}
	for _, l := range m.PhaseMap {
		if l != domain.LineL1 && l != domain.LineL2 && l != domain.LineL3 || seen[l] {
			return errors.New("phase_map must be a permutation of L1, L2, L3")
		}
		seen[l] = true
	}
	t := m.MeterTiming
	switch {
	case t.ResponseTimeout <= 0 || t.PowerPeriod <= 0 || t.EnergyPeriod <= 0 || t.IdentityPeriod <= 0:
		return errors.New("response_timeout and poll periods must be > 0")
	case t.ResponseTimeout >= t.PowerPeriod:
		return errors.New("response_timeout must be < power_period")
	case t.ReconnectMin <= 0 || t.ReconnectMax < t.ReconnectMin:
		return errors.New("need 0 < reconnect_min <= reconnect_max")
	}
	return nil
}

func mergeMeterTiming(t, d MeterTiming) MeterTiming {
	def := func(v *time.Duration, dv time.Duration) {
		if *v == 0 {
			*v = dv
		}
	}
	def(&t.ConnectTimeout, d.ConnectTimeout)
	def(&t.ResponseTimeout, d.ResponseTimeout)
	def(&t.FrameGap, d.FrameGap)
	def(&t.StatusTail, d.StatusTail)
	def(&t.PowerPeriod, d.PowerPeriod)
	def(&t.EnergyPeriod, d.EnergyPeriod)
	def(&t.IdentityPeriod, d.IdentityPeriod)
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
