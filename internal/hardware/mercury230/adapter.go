package mercury230

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/kmlebedev/EVcomm/internal/domain"
	"github.com/kmlebedev/EVcomm/internal/measurements"
	"github.com/kmlebedev/EVcomm/internal/rtu"
)

var (
	ErrNotConnected = errors.New("mercury230: meter not connected")
	// ErrPhaseEnergyUnsupported — исполнение не ведёт пофазный учёт A+:
	// однофазную линию по нему оплачивать нельзя.
	ErrPhaseEnergyUnsupported = errors.New("mercury230: per-phase energy not supported by meter")
	ErrLineNotMetered         = errors.New("mercury230: line is not mapped to a meter phase")
)

type Config struct {
	Name        string // ID поста, для логов
	Dial        rtu.Dialer
	Address     uint8 // 1…240
	AccessLevel uint8 // 1 или 2
	Password    [6]byte
	// ExpectedSerial — серийный номер из реестра; пусто — не сверять.
	ExpectedSerial string
	// PhaseMap — фаза счётчика 1…3 → линия поста, по монтажной схеме.
	PhaseMap [3]domain.LineID
	// GroupPowerBWRI — BWRI для 08 16 (00h или 01h, определяется на стенде).
	GroupPowerBWRI byte

	PowerPeriod    time.Duration // P: 1 с
	EnergyPeriod   time.Duration // AP и Total: 15 с + внеочередно
	IdentityPeriod time.Duration // серийный номер: при подключении и раз в час
	// PowerStaleAfter, EnergyStaleAfter — возраст, после которого значение stale.
	// По умолчанию — три периода опроса.
	PowerStaleAfter  time.Duration
	EnergyStaleAfter time.Duration
	// MaxLinkErrors — таймаутов или испорченных кадров подряд до переподключения.
	MaxLinkErrors int

	ReconnectMin, ReconnectMax, StartJitter time.Duration
	Validation                              ValidationConfig
	Logger                                  *slog.Logger
	// Trace получает каждую транзакцию (стенд, отладка). Необязателен.
	Trace func(Exchange)
}

func (c *Config) setDefaults() {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&c.PowerPeriod, time.Second)
	def(&c.EnergyPeriod, 15*time.Second)
	def(&c.IdentityPeriod, time.Hour)
	def(&c.PowerStaleAfter, 3*c.PowerPeriod)
	def(&c.EnergyStaleAfter, 3*c.EnergyPeriod)
	def(&c.ReconnectMin, 500*time.Millisecond)
	def(&c.ReconnectMax, 30*time.Second)
	if c.MaxLinkErrors <= 0 {
		c.MaxLinkErrors = 3
	}
	if c.AccessLevel == 0 {
		c.AccessLevel = 1
	}
	if c.PhaseMap == ([3]domain.LineID{}) {
		c.PhaseMap = [3]domain.LineID{domain.LineL1, domain.LineL2, domain.LineL3}
	}
	if c.Validation == (ValidationConfig{}) {
		c.Validation = DefaultValidation()
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Capabilities — возможности исполнения, определяются при подключении.
type Capabilities struct {
	PhaseEnergy  bool // бит варианта исполнения и ответ 05 60 00 без маски
	GroupPower   bool // 08 16 поддержан; иначе 3 × 08 11
	PowerAllowed bool // мгновенные значения доступны на текущем уровне доступа
}

type State string

const (
	StateConnecting  State = "connecting"
	StateOnline      State = "online"
	StateGatewayBusy State = "gateway_busy" // порт моста занят другим мастером
	StateNoResponse  State = "no_response"  // TCP есть, счётчик молчит
	StateConfigError State = "config_error" // неверный пароль или уровень доступа
	StateStopped     State = "stopped"
)

// Status — состояние связи и счётчика.
type Status struct {
	State         State
	Connected     bool // TCP-сессия с мостом
	MeterOK       bool // последний обмен успешен
	Identity      Identity
	Capabilities  Capabilities
	Mismatch      bool // серийный номер не совпал с реестром
	Alarms        []string
	LastError     string    `json:",omitempty"`
	LastErrorAt   time.Time `json:",omitzero"`
	LastSuccessAt time.Time `json:",omitzero"`
	Reconnects    int
	LinkErrors    int    // подряд
	Transactions  uint64 // всего
	Errors        uint64 // всего
}

type request struct {
	ctx   context.Context
	reply chan energyResult
}

type energyResult struct {
	snap EnergySnapshot
	err  error
}

// Adapter владеет соединением со счётчиком поста. Весь обмен выполняет одна
// goroutine (Run); внеочередные снимки энергии идут через её очередь, поэтому
// не пересекаются с фоновым опросом на шине.
type Adapter struct {
	cfg  Config
	log  *slog.Logger
	reqs chan request

	mu     sync.Mutex
	status Status
	alarms map[string]string
	power  PowerSnapshot
	energy EnergySnapshot
	valid  *Validator

	// Поля ниже — только goroutine Run.
	caps Capabilities
}

var _ measurements.Source = (*Adapter)(nil)

func New(cfg Config) *Adapter {
	cfg.setDefaults()
	return &Adapter{
		cfg:    cfg,
		log:    cfg.Logger.With("post", cfg.Name, "component", "mercury230"),
		reqs:   make(chan request),
		alarms: map[string]string{},
		valid:  NewValidator(cfg.Validation),
		status: Status{State: StateConnecting},
	}
}

// Run — цикл подключения и опроса. Возвращается при отмене ctx.
func (a *Adapter) Run(ctx context.Context) error {
	defer a.setState(StateStopped)
	if a.cfg.StartJitter > 0 {
		a.idle(ctx, rand.N(a.cfg.StartJitter))
	}
	backoff := a.cfg.ReconnectMin
	for ctx.Err() == nil {
		a.setState(StateConnecting)
		conn, err := a.cfg.Dial(ctx)
		if err != nil {
			a.recordError(err)
			a.idle(ctx, jitter(backoff))
			backoff = min(2*backoff, a.cfg.ReconnectMax)
			continue
		}
		a.mu.Lock()
		a.status.Connected = true
		a.status.Reconnects++
		a.mu.Unlock()
		a.log.Info("connected")

		exchanged, err := a.session(ctx, conn)
		_ = conn.Close()
		a.mu.Lock()
		a.status.Connected, a.status.MeterOK = false, false
		a.mu.Unlock()
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			a.recordError(err)
		}
		switch {
		case errors.Is(err, rtu.ErrGatewayBusy):
			a.setState(StateGatewayBusy)
			a.log.Warn("gateway port busy (another master connected)", "err", err)
		case IsStatus(err, StatusAccessDenied):
			a.setState(StateConfigError)
			a.log.Error("access denied: wrong password or access level", "level", a.cfg.AccessLevel, "err", err)
			backoff = a.cfg.ReconnectMax
		case Classify(err) == KindLink:
			a.setState(StateNoResponse)
			a.log.Warn("meter not responding", "err", err)
		default:
			a.log.Warn("disconnected", "err", err)
		}
		if exchanged && !IsStatus(err, StatusAccessDenied) {
			backoff = a.cfg.ReconnectMin
		}
		a.idle(ctx, jitter(backoff))
		backoff = min(2*backoff, a.cfg.ReconnectMax)
	}
	return ctx.Err()
}

// session обслуживает одно соединение. Разрыв TCP сбрасывает «канал открыт»,
// поэтому каждая сессия начинается с 08 00 и 01h.
func (a *Adapter) session(ctx context.Context, conn rtu.Conn) (exchanged bool, err error) {
	m := NewMeter(conn, MeterConfig{Address: a.cfg.Address, AccessLevel: a.cfg.AccessLevel, Password: a.cfg.Password, Trace: a.trace})
	linkErrs := 0
	// fail фиксирует ошибку и возвращает её, если сессию нужно завершить.
	fail := func(err error) error {
		a.recordError(err)
		switch Classify(err) {
		case KindTransport:
			return err
		case KindLink:
			linkErrs++
			a.mu.Lock()
			a.status.LinkErrors = linkErrs
			a.mu.Unlock()
			if linkErrs >= a.cfg.MaxLinkErrors {
				return fmt.Errorf("%d consecutive link errors: %w", linkErrs, err)
			}
		case KindStatus:
			if IsStatus(err, StatusInternal) {
				a.setAlarm("internal", "meter reports internal error (status 2)")
			}
		}
		return nil
	}
	ok := func() {
		linkErrs = 0
		exchanged = true
		a.recordSuccess()
	}

	id, err := m.ReadSerial(ctx)
	if err != nil {
		return false, err
	}
	ok()
	if err := m.Open(ctx); err != nil {
		return exchanged, err
	}
	if full, err := m.ReadIdentity(ctx); err == nil {
		id = full
	} else if Classify(err) == KindTransport {
		return exchanged, err
	} else {
		a.log.Warn("passport not read", "err", err)
	}
	a.setIdentity(id)
	a.log.Info("meter identified", "serial", id.Serial, "firmware", id.Firmware, "variant", fmt.Sprintf("% X", id.Variant[:]))

	if err := a.probe(ctx, m, id); err != nil {
		return exchanged, err
	}
	ok()
	a.setState(StateOnline)

	if _, err := a.readEnergy(ctx, m); err != nil {
		if ferr := fail(err); ferr != nil {
			return exchanged, ferr
		}
	} else {
		ok()
	}

	powerT := time.NewTicker(a.cfg.PowerPeriod)
	defer powerT.Stop()
	energyT := time.NewTicker(a.cfg.EnergyPeriod)
	defer energyT.Stop()
	identityT := time.NewTicker(a.cfg.IdentityPeriod)
	defer identityT.Stop()

	serve := func(req request) error {
		if req.ctx.Err() != nil {
			req.reply <- energyResult{err: req.ctx.Err()}
			return nil
		}
		// Контекст вызывающего ограничивает транзакцию, но отмена сессии важнее.
		rctx, cancel := context.WithCancel(req.ctx)
		stop := context.AfterFunc(ctx, cancel)
		snap, err := a.readEnergy(rctx, m)
		stop()
		cancel()
		req.reply <- energyResult{snap: snap, err: err}
		if err != nil {
			return fail(err)
		}
		ok()
		return nil
	}

	for {
		// Внеочередной снимок — приоритетнее фонового опроса.
		select {
		case req := <-a.reqs:
			if err := serve(req); err != nil {
				return exchanged, err
			}
			continue
		default:
		}
		var err error
		select {
		case <-ctx.Done():
			cctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			_ = m.CloseChannel(cctx) // освободить канал; слот моста освободит Close
			cancel()
			return exchanged, nil
		case req := <-a.reqs:
			err = serve(req)
			if err != nil {
				return exchanged, err
			}
			continue
		case <-powerT.C:
			if !a.caps.PowerAllowed {
				continue
			}
			err = a.readPower(ctx, m)
		case <-energyT.C:
			_, err = a.readEnergy(ctx, m)
		case <-identityT.C:
			var cur Identity
			if cur, err = m.ReadSerial(ctx); err == nil {
				a.checkSerial(cur.Serial)
			}
		}
		if err != nil {
			if ferr := fail(err); ferr != nil {
				return exchanged, ferr
			}
			continue
		}
		ok()
	}
}

// probe определяет возможности исполнения. Возвращает только ошибку транспорта.
func (a *Adapter) probe(ctx context.Context, m *Meter, id Identity) error {
	a.clearAlarm("phase_energy")
	a.clearAlarm("power_access")
	caps := Capabilities{PhaseEnergy: !id.HasVariant || id.Variant.PhaseEnergy()}
	if !caps.PhaseEnergy {
		a.setAlarm("phase_energy", "variant: per-phase A+ accounting not supported")
	}
	if caps.PhaseEnergy {
		_, _, err := m.ReadPhaseEnergy(ctx)
		switch {
		case err == nil:
		case errors.Is(err, ErrMasked), IsStatus(err, StatusInvalidCommand):
			caps.PhaseEnergy = false
			a.setAlarm("phase_energy", "05 60 00: per-phase A+ not supported ("+err.Error()+")")
		case Classify(err) == KindTransport:
			return err
		default:
			a.log.Warn("phase energy probe failed, will retry on poll", "err", err)
		}
	}

	pw, err := m.ReadGroupPower(ctx, a.cfg.GroupPowerBWRI)
	switch {
	case err == nil:
		caps.GroupPower, caps.PowerAllowed = true, true
	case IsStatus(err, StatusAccessDenied):
		a.setAlarm("power_access", "instantaneous power needs higher access level (status 3)")
	case Classify(err) == KindTransport:
		return err
	default:
		if pw, err = m.ReadPhasePower(ctx); err == nil {
			caps.PowerAllowed = true
		} else if IsStatus(err, StatusAccessDenied) {
			a.setAlarm("power_access", "instantaneous power needs higher access level (status 3)")
		} else if Classify(err) == KindTransport {
			return err
		} else if !IsStatus(err, StatusInvalidCommand) {
			caps.PowerAllowed = true // временная ошибка: попробовать при опросе
		}
	}
	a.caps = caps
	a.mu.Lock()
	a.status.Capabilities = caps
	a.mu.Unlock()
	a.log.Info("capabilities", "phase_energy", caps.PhaseEnergy, "group_power", caps.GroupPower, "power_allowed", caps.PowerAllowed)
	if err == nil && caps.PowerAllowed {
		a.publishPower(pw)
	}
	return nil
}

func (a *Adapter) readPower(ctx context.Context, m *Meter) error {
	var (
		pw  Power
		err error
	)
	if a.caps.GroupPower {
		pw, err = m.ReadGroupPower(ctx, a.cfg.GroupPowerBWRI)
		if IsStatus(err, StatusInvalidCommand) {
			a.log.Warn("group power not supported, falling back to per-phase reads", "err", err)
			a.setCaps(func(c *Capabilities) { c.GroupPower = false })
			pw, err = m.ReadPhasePower(ctx)
		}
	} else {
		pw, err = m.ReadPhasePower(ctx)
	}
	switch {
	case err == nil:
		a.publishPower(pw)
	case IsStatus(err, StatusAccessDenied), IsStatus(err, StatusInvalidCommand):
		// Постоянная ошибка: опрос мощности остановить до переподключения.
		a.setCaps(func(c *Capabilities) { c.PowerAllowed = false })
		a.setAlarm("power_access", "power polling stopped: "+err.Error())
	}
	return err
}

// readEnergy читает 05 60 00 (если поддержан) и 05 00 00, проверяет и публикует снимок.
func (a *Adapter) readEnergy(ctx context.Context, m *Meter) (EnergySnapshot, error) {
	s := EnergySnapshot{Serial: a.MeterStatus().Identity.Serial}
	if a.caps.PhaseEnergy {
		pe, ex, err := m.ReadPhaseEnergy(ctx)
		switch {
		case err == nil:
			s.HasPhase, s.PhaseWh, s.ObservedAt = true, pe, ex.Started.Add(ex.Duration)
			s.PhaseReq, s.PhaseResp = ex.Req, ex.Resp
		case errors.Is(err, ErrMasked), IsStatus(err, StatusInvalidCommand):
			a.setCaps(func(c *Capabilities) { c.PhaseEnergy = false })
			a.setAlarm("phase_energy", "05 60 00: per-phase A+ not supported ("+err.Error()+")")
		default:
			return s, err
		}
	}
	te, ex, err := m.ReadTotalEnergy(ctx)
	if err != nil {
		return s, err
	}
	s.TotalWh = te.APlus
	s.TotalReq, s.TotalResp = ex.Req, ex.Resp
	if !s.HasPhase {
		s.ObservedAt = ex.Started.Add(ex.Duration)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status.Mismatch {
		s.Issues = append(s.Issues, fmt.Sprintf("serial %q differs from registry %q", s.Serial, a.cfg.ExpectedSerial))
	}
	a.valid.Check(&s)
	if s.Quality == domain.QualityDisputed {
		a.log.Warn("energy snapshot disputed", "issues", s.Issues)
	}
	a.energy = s
	return s, nil
}

func (a *Adapter) publishPower(pw Power) {
	s := PowerSnapshot{TotalCW: pw.TotalCW, PhaseCW: pw.PhaseCW, ObservedAt: time.Now()}
	for i, v := range pw.PhaseCW {
		key := fmt.Sprintf("negative_power_%d", i+1)
		if v < 0 {
			a.setAlarm(key, fmt.Sprintf("negative active power on meter phase %d (%s): %.2f W — check wiring", i+1, a.cfg.PhaseMap[i], float64(v)/100))
		} else {
			a.clearAlarm(key)
		}
	}
	a.mu.Lock()
	a.power = s
	a.mu.Unlock()
}

func (a *Adapter) setIdentity(id Identity) {
	a.mu.Lock()
	a.status.Identity = id
	a.mu.Unlock()
	a.checkSerial(id.Serial)
}

func (a *Adapter) checkSerial(serial string) {
	a.mu.Lock()
	prev := a.status.Identity.Serial
	a.status.Identity.Serial = serial
	mismatch := a.cfg.ExpectedSerial != "" && serial != a.cfg.ExpectedSerial
	a.status.Mismatch = mismatch
	a.mu.Unlock()
	if prev != "" && prev != serial {
		a.log.Error("meter serial changed", "was", prev, "now", serial)
	}
	if mismatch {
		a.setAlarm("serial", fmt.Sprintf("serial %s differs from registry %s: energy is disputed", serial, a.cfg.ExpectedSerial))
	} else {
		a.clearAlarm("serial")
	}
}

// --- доступ извне ---

// ReadEnergy — внеочередной снимок энергии всех фаз; блокирует до ответа или ctx.
func (a *Adapter) ReadEnergy(ctx context.Context) (EnergySnapshot, error) {
	req := request{ctx: ctx, reply: make(chan energyResult, 1)}
	select {
	case a.reqs <- req:
	case <-ctx.Done():
		return EnergySnapshot{}, fmt.Errorf("%w: %w", ErrNotConnected, ctx.Err())
	}
	select {
	case r := <-req.reply:
		return r.snap, r.err
	case <-ctx.Done():
		return EnergySnapshot{}, ctx.Err()
	}
}

// EnergySnapshot — внеочередное чтение учётного регистра линии. Для однофазной
// линии — A+ её фазы (05 60 00), для 3P — A+ суммарно (05 00 00).
func (a *Adapter) EnergySnapshot(ctx context.Context, line domain.LineID) (measurements.EnergyEvidence, error) {
	idx, total := a.phaseOf(line)
	if idx < 0 && !total {
		return measurements.EnergyEvidence{}, fmt.Errorf("%w: %s", ErrLineNotMetered, line)
	}
	s, err := a.ReadEnergy(ctx)
	if err != nil {
		return measurements.EnergyEvidence{}, err
	}
	return a.evidence(s, line)
}

func (a *Adapter) evidence(s EnergySnapshot, line domain.LineID) (measurements.EnergyEvidence, error) {
	idx, _ := a.phaseOf(line)
	ev := measurements.EnergyEvidence{
		Line: line, Source: sourceName(s.Serial), Serial: s.Serial, ObservedAt: s.ObservedAt,
		Quality: s.Quality, Issues: s.Issues,
	}
	if idx < 0 {
		ev.Register, ev.Wh = "A+ total", uint64(s.TotalWh)
		ev.RawRequest, ev.RawResp = s.TotalReq, s.TotalResp
		return ev, nil
	}
	if !s.HasPhase {
		return ev, ErrPhaseEnergyUnsupported
	}
	ev.Register, ev.Wh = fmt.Sprintf("A+ phase %d", idx+1), uint64(s.PhaseWh[idx])
	ev.RawRequest, ev.RawResp = s.PhaseReq, s.PhaseResp
	return ev, nil
}

// phaseOf — индекс фазы счётчика для линии; total — линия 3P (сумма фаз).
func (a *Adapter) phaseOf(line domain.LineID) (idx int, total bool) {
	if line == domain.Line3P {
		return -1, true
	}
	return slices.Index(a.cfg.PhaseMap[:], line), false
}

// Power — последняя активная мощность линии, Вт.
func (a *Adapter) Power(line domain.LineID) domain.Observation[float64] {
	a.mu.Lock()
	defer a.mu.Unlock()
	obs := domain.Observation[float64]{Source: sourceName(a.status.Identity.Serial), Quality: domain.QualityUnknown}
	idx, total := a.phaseOf(line)
	if a.power.ObservedAt.IsZero() || (idx < 0 && !total) {
		return obs
	}
	cw := a.power.TotalCW
	if idx >= 0 {
		cw = a.power.PhaseCW[idx]
	}
	obs.Value, obs.ObservedAt = float64(cw)/100, a.power.ObservedAt
	obs.Quality = a.freshness(a.power.ObservedAt, a.cfg.PowerStaleAfter, domain.QualityFresh)
	return obs
}

// Energy — последняя накопленная A+ линии, Вт·ч.
func (a *Adapter) Energy(line domain.LineID) domain.Observation[uint64] {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.energy
	obs := domain.Observation[uint64]{Source: sourceName(s.Serial), Quality: domain.QualityUnknown}
	idx, total := a.phaseOf(line)
	if s.ObservedAt.IsZero() || (idx < 0 && !total) || (idx >= 0 && !s.HasPhase) {
		return obs
	}
	obs.Value, obs.ObservedAt = uint64(s.TotalWh), s.ObservedAt
	if idx >= 0 {
		obs.Value = uint64(s.PhaseWh[idx])
	}
	obs.Quality = a.freshness(s.ObservedAt, a.cfg.EnergyStaleAfter, s.Quality)
	return obs
}

func (a *Adapter) freshness(at time.Time, staleAfter time.Duration, q domain.Quality) domain.Quality {
	if q == domain.QualityFresh && (!a.status.Connected || time.Since(at) > staleAfter) {
		return domain.QualityStale
	}
	return q
}

// LatestPower, LatestEnergy — последние снимки (ObservedAt нулевое — ещё не было).
func (a *Adapter) LatestPower() PowerSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.power
}

func (a *Adapter) LatestEnergy() EnergySnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.energy
}

// MeterStatus — подробное состояние адаптера.
func (a *Adapter) MeterStatus() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.status
	st.Alarms = slices.Sorted(maps.Values(a.alarms))
	return st
}

// Status — состояние в терминах measurements.Source.
func (a *Adapter) Status() measurements.SourceStatus {
	st := a.MeterStatus()
	return measurements.SourceStatus{
		Source: sourceName(st.Identity.Serial), State: string(st.State),
		Connected: st.Connected, Healthy: st.MeterOK, Mismatch: st.Mismatch,
		LastError: st.LastError, LastErrorAt: st.LastErrorAt, LastSuccessAt: st.LastSuccessAt,
		Alarms: st.Alarms,
	}
}

func sourceName(serial string) string {
	if serial == "" {
		serial = "?"
	}
	return "mercury230:" + serial
}

// --- служебное ---

func (a *Adapter) trace(ex Exchange) {
	a.mu.Lock()
	a.status.Transactions++
	a.mu.Unlock()
	a.log.Debug("exchange", "req", ex.Request, "tx", fmt.Sprintf("% X", ex.Req), "rx", fmt.Sprintf("% X", ex.Resp),
		"dur", ex.Duration, "err", ex.Err)
	if a.cfg.Trace != nil {
		a.cfg.Trace(ex)
	}
}

func (a *Adapter) setCaps(f func(*Capabilities)) {
	f(&a.caps)
	a.mu.Lock()
	a.status.Capabilities = a.caps
	a.mu.Unlock()
}

func (a *Adapter) setState(s State) {
	a.mu.Lock()
	a.status.State = s
	a.mu.Unlock()
}

func (a *Adapter) setAlarm(key, msg string) {
	a.mu.Lock()
	prev, had := a.alarms[key]
	a.alarms[key] = msg
	a.mu.Unlock()
	if !had || prev != msg {
		a.log.Warn("alarm", "key", key, "msg", msg)
	}
}

func (a *Adapter) clearAlarm(key string) {
	a.mu.Lock()
	_, had := a.alarms[key]
	delete(a.alarms, key)
	a.mu.Unlock()
	if had {
		a.log.Info("alarm cleared", "key", key)
	}
}

func (a *Adapter) recordError(err error) {
	a.mu.Lock()
	a.status.LastError = err.Error()
	a.status.LastErrorAt = time.Now()
	a.status.MeterOK = false
	a.status.Errors++
	a.mu.Unlock()
}

func (a *Adapter) recordSuccess() {
	a.mu.Lock()
	a.status.MeterOK = true
	a.status.LinkErrors = 0
	a.status.LastSuccessAt = time.Now()
	a.mu.Unlock()
	a.clearAlarm("internal")
}

// idle ждёт d, отвечая на запросы ErrNotConnected.
func (a *Adapter) idle(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			return
		case req := <-a.reqs:
			req.reply <- energyResult{err: ErrNotConnected}
		}
	}
}

// jitter — равномерно в [d/2, d].
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + rand.N(d/2+1)
}
