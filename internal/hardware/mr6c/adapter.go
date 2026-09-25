package mr6c

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

var (
	// ErrLeaseExpired — аренда поста истекла, адаптер молчит, чтобы сработал безопасный режим MR6C.
	ErrLeaseExpired = errors.New("mr6c: lease expired, traffic suspended")
	ErrNotConnected = errors.New("mr6c: not connected")
	// ErrOutcomeUnknown — запрос мог дойти до модуля; исход выяснять сверкой по снимку.
	ErrOutcomeUnknown = errors.New("mr6c: write outcome unknown")
	// ErrRejected — модуль или шлюз ответили исключением; запись не выполнена.
	ErrRejected = errors.New("mr6c: write rejected")
)

// Gate разрешает обмен с модулем. Реализация — supervision.Lease.
type Gate interface {
	Valid(now time.Time) bool
}

type Config struct {
	Name         string // для логов, обычно ID поста
	Dial         Dialer
	Gate         Gate
	PollPeriod   time.Duration
	SplitInputs  bool
	RealState    bool
	Safety       SafetyConfig
	ReconnectMin time.Duration
	ReconnectMax time.Duration
	// StartJitter — случайная задержка первого подключения, чтобы 100+ постов не подключались разом.
	StartJitter time.Duration
	Logger      *slog.Logger
}

// Confirmation — уровень подтверждения записи. Возврат из SetOutput не означает
// физического исполнения: контактор подтверждает только НЗ-вход (уровень контроллера).
type Confirmation int

const (
	ConfirmNone      Confirmation = iota // исход неизвестен
	ConfirmWriteAck                      // модуль ответил на FC05
	ConfirmOutput                        // чтение coil совпало с командой
	ConfirmRealState                     // совпало и фактическое состояние выхода
)

func (c Confirmation) String() string {
	return [...]string{"none", "write-ack", "output", "real-state"}[c]
}

type WriteResult struct {
	Level    Confirmation
	Started  time.Time     // начало записи
	WriteAck time.Duration // от начала записи до ответа
	Readback time.Duration // от начала записи до завершения контрольного чтения
	Snapshot Snapshot      // контрольное чтение после записи (если Level ≥ ConfirmOutput или прочитано)
}

// Status — состояние связи адаптера.
type Status struct {
	Connected     bool         // TCP-сессия со шлюзом
	ModuleOK      bool         // последний обмен с модулем успешен
	Silenced      bool         // аренда истекла, трафик прекращён
	LastError     string       `json:",omitempty"`
	LastErrorAt   time.Time    `json:",omitzero"`
	Reconnects    int          // число успешных подключений
	Safety        SafetyReport // сверка эталона в текущей сессии; сбрасывается при разрыве
	LastSuccessAt time.Time    `json:",omitzero"`
}

type opKind int

const (
	opSetOutput opKind = iota
	opSnapshot
	opVerify
	opApply
)

type request struct {
	ctx   context.Context
	kind  opKind
	k     int
	on    bool
	reply chan response
}

type response struct {
	write  WriteResult
	snap   Snapshot
	safety SafetyReport
	err    error
}

// Adapter владеет TCP-соединением с одним постом. Все Modbus-транзакции
// выполняет одна goroutine (Run), и только при действующей аренде.
type Adapter struct {
	cfg   Config
	log   *slog.Logger
	reqs  chan request
	snaps chan Snapshot

	mu     sync.Mutex
	status Status
	latest Snapshot
	seq    uint64
}

func New(cfg Config) *Adapter {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Adapter{
		cfg:   cfg,
		log:   cfg.Logger.With("post", cfg.Name, "component", "mr6c"),
		reqs:  make(chan request),
		snaps: make(chan Snapshot, 1),
	}
}

// Watch — поток снимков для единственного потребителя (контроллера поста).
// При медленном потребителе старый снимок вытесняется новым.
func (a *Adapter) Watch() <-chan Snapshot { return a.snaps }

// Latest возвращает последний снимок (Seq == 0 — снимков ещё не было).
func (a *Adapter) Latest() Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latest
}

func (a *Adapter) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

// SetOutput — идемпотентная запись FC05 на coil Kk и контрольное чтение.
func (a *Adapter) SetOutput(ctx context.Context, k int, on bool) (WriteResult, error) {
	if k < 1 || k > NumOutputs {
		return WriteResult{}, fmt.Errorf("mr6c: output K%d out of range", k)
	}
	r, err := a.call(ctx, request{kind: opSetOutput, k: k, on: on})
	return r.write, err
}

// ReadSnapshot — внеочередное свежее чтение.
func (a *Adapter) ReadSnapshot(ctx context.Context) (Snapshot, error) {
	r, err := a.call(ctx, request{kind: opSnapshot})
	return r.snap, err
}

// VerifySafetyConfig перечитывает эталон настроек.
func (a *Adapter) VerifySafetyConfig(ctx context.Context) (SafetyReport, error) {
	r, err := a.call(ctx, request{kind: opVerify})
	return r.safety, err
}

// ApplySafetyConfig записывает эталон в модуль и перечитывает его. Только для стенда.
func (a *Adapter) ApplySafetyConfig(ctx context.Context) (SafetyReport, error) {
	r, err := a.call(ctx, request{kind: opApply})
	return r.safety, err
}

func (a *Adapter) call(ctx context.Context, req request) (response, error) {
	req.ctx = ctx
	req.reply = make(chan response, 1)
	select {
	case a.reqs <- req:
	case <-ctx.Done():
		// Запрос не принят адаптером — к модулю он не уходил.
		return response{}, fmt.Errorf("%w: %w", ErrNotConnected, ctx.Err())
	}
	select {
	case r := <-req.reply:
		return r, r.err
	case <-ctx.Done():
		if req.kind == opSetOutput {
			return response{}, fmt.Errorf("%w: %w", ErrOutcomeUnknown, ctx.Err())
		}
		return response{}, ctx.Err()
	}
}

// Run — цикл подключения и опроса. Возвращается при отмене ctx.
func (a *Adapter) Run(ctx context.Context) error {
	if a.cfg.StartJitter > 0 {
		a.idle(ctx, rand.N(a.cfg.StartJitter), ErrNotConnected)
	}
	backoff := a.cfg.ReconnectMin
	for ctx.Err() == nil {
		bus, err := a.cfg.Dial(ctx)
		if err != nil {
			a.recordError(err)
			a.idle(ctx, jitter(backoff), ErrNotConnected)
			backoff = min(2*backoff, a.cfg.ReconnectMax)
			continue
		}
		a.mu.Lock()
		a.status.Connected = true
		a.status.Reconnects++
		a.status.Safety = SafetyReport{}
		a.mu.Unlock()
		a.log.Info("connected")

		exchanged, err := a.session(ctx, bus)
		_ = bus.Close()
		a.mu.Lock()
		a.status.Connected, a.status.ModuleOK = false, false
		a.status.Safety = SafetyReport{} // после переподключения — полная сверка эталона
		a.mu.Unlock()
		if ctx.Err() != nil {
			break
		}
		a.log.Warn("disconnected", "err", err)
		if exchanged {
			backoff = a.cfg.ReconnectMin
		}
		a.idle(ctx, jitter(backoff), ErrNotConnected)
		backoff = min(2*backoff, a.cfg.ReconnectMax)
	}
	return ctx.Err()
}

// session обслуживает одно TCP-соединение. Возвращает ошибку транспорта; exchanged —
// был ли хотя бы один успешный обмен (для сброса backoff).
func (a *Adapter) session(ctx context.Context, bus Bus) (exchanged bool, err error) {
	ticker := time.NewTicker(a.cfg.PollPeriod)
	defer ticker.Stop()
	opt := readOptions{splitInputs: a.cfg.SplitInputs, realState: a.cfg.RealState}

	for {
		select {
		case <-ctx.Done():
			return exchanged, nil

		case req := <-a.reqs:
			if !a.leaseOK() {
				req.reply <- response{err: ErrLeaseExpired}
				continue
			}
			if req.ctx.Err() != nil {
				// Вызывающий уже сдался: не выполнять устаревшую запись.
				req.reply <- response{err: req.ctx.Err()}
				continue
			}
			resp, terr := a.handle(bus, req, opt)
			req.reply <- resp
			if terr != nil {
				return exchanged, terr
			}
			exchanged = exchanged || resp.err == nil

		case <-ticker.C:
			if !a.leaseOK() {
				continue
			}
			// Эталон сверяется при каждом подключении, до первого разрешения включений.
			if a.Status().Safety.CheckedAt.IsZero() {
				rep := verifySafety(bus, a.cfg.Safety, time.Now())
				a.setSafety(rep)
				if rep.Err != nil {
					if terr := a.transportErr(rep.Err); terr != nil {
						return exchanged, terr
					}
					continue
				}
			}
			snap, err := readSnapshot(bus, opt)
			if err != nil {
				if terr := a.transportErr(err); terr != nil {
					return exchanged, terr
				}
				continue
			}
			a.publish(snap)
			exchanged = true
		}
	}
}

// handle выполняет запрос. Второй результат — ошибка транспорта (нужно переподключение).
func (a *Adapter) handle(bus Bus, req request, opt readOptions) (response, error) {
	switch req.kind {
	case opSetOutput:
		started := time.Now()
		res := WriteResult{Started: started}
		if err := bus.WriteCoil(CoilOutputBase+uint16(req.k-1), req.on); err != nil {
			if IsException(err) {
				a.recordError(err)
				return response{write: res, err: fmt.Errorf("%w: K%d=%v: %w", ErrRejected, req.k, req.on, err)}, nil
			}
			return response{write: res, err: fmt.Errorf("%w: K%d=%v: %w", ErrOutcomeUnknown, req.k, req.on, err)}, a.transportErr(err)
		}
		res.Level = ConfirmWriteAck
		res.WriteAck = time.Since(started)
		a.log.Debug("output written", "k", req.k, "on", req.on, "ack", res.WriteAck)
		snap, err := readSnapshot(bus, opt)
		if err != nil {
			// Запись подтверждена, чтение — нет; сверку выполнит следующий опрос.
			return response{write: res}, a.transportErr(err)
		}
		res.Readback = time.Since(started)
		snap = a.publish(snap)
		res.Snapshot = snap
		if snap.Output(req.k) == req.on {
			res.Level = ConfirmOutput
			if snap.Real(req.k) == req.on {
				res.Level = ConfirmRealState
			}
		}
		return response{write: res}, nil

	case opSnapshot:
		snap, err := readSnapshot(bus, opt)
		if err != nil {
			return response{err: err}, a.transportErr(err)
		}
		return response{snap: a.publish(snap)}, nil

	case opVerify, opApply:
		if req.kind == opApply {
			a.log.Warn("writing safety reference config to module")
			if err := applySafety(bus, a.cfg.Safety); err != nil {
				return response{err: err}, a.transportErr(err)
			}
		}
		rep := verifySafety(bus, a.cfg.Safety, time.Now())
		a.setSafety(rep)
		return response{safety: rep, err: rep.Err}, a.transportErr(rep.Err)
	}
	return response{err: errors.New("mr6c: unknown request")}, nil
}

// idle ждёт d, отвечая на запросы ошибкой reason (соединения нет — обмен невозможен).
func (a *Adapter) idle(ctx context.Context, d time.Duration, reason error) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			return
		case req := <-a.reqs:
			req.reply <- response{err: reason}
		}
	}
}

func (a *Adapter) leaseOK() bool {
	ok := a.cfg.Gate.Valid(time.Now())
	a.mu.Lock()
	changed := a.status.Silenced == ok
	a.status.Silenced = !ok
	a.mu.Unlock()
	if changed {
		if ok {
			a.log.Info("lease valid, traffic resumed")
		} else {
			a.log.Warn("lease expired, traffic suspended (MR6C safe mode will trigger)")
		}
	}
	return ok
}

func (a *Adapter) publish(s Snapshot) Snapshot {
	a.mu.Lock()
	a.seq++
	s.Seq = a.seq
	s.ReadAt = time.Now()
	a.latest = s
	a.status.ModuleOK = true
	a.status.LastSuccessAt = s.ReadAt
	a.mu.Unlock()
	select {
	case <-a.snaps:
	default:
	}
	a.snaps <- s
	return s
}

func (a *Adapter) setSafety(rep SafetyReport) {
	a.mu.Lock()
	a.status.Safety = rep
	a.mu.Unlock()
	switch {
	case rep.Err != nil:
		a.log.Warn("safety config read failed", "err", rep.Err)
	case !rep.OK():
		a.log.Error("safety config mismatch, enabling forbidden", "report", rep.String())
	default:
		a.log.Info("safety config verified", "firmware", rep.Firmware)
	}
}

// transportErr фиксирует ошибку и возвращает её, если нужно переподключение.
func (a *Adapter) transportErr(err error) error {
	if err == nil {
		return nil
	}
	a.recordError(err)
	if IsException(err) {
		return nil
	}
	return err
}

func (a *Adapter) recordError(err error) {
	a.mu.Lock()
	a.status.LastError = err.Error()
	a.status.LastErrorAt = time.Now()
	a.status.ModuleOK = false
	a.mu.Unlock()
}

// jitter — равномерно в [d/2, d].
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + rand.N(d/2+1)
}
