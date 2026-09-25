// Package sim — симуляторы оборудования для тестов и стенда без железа.
package sim

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"sync"
	"time"

	"github.com/simonvetter/modbus"

	"github.com/kmlebedev/EVcomm/internal/hardware/mr6c"
)

// Fault — неисправность контактора на выходе.
type Fault int

const (
	FaultNone          Fault = iota
	FaultStuckReleased       // механизм не срабатывает: НЗ остаётся замкнутым
	FaultStuckEngaged        // механизм не отпускается: НЗ остаётся разомкнутым
	FaultNCOpen              // обрыв цепи обратной связи: НЗ всегда разомкнут
)

// MR6C — симулятор WB-MR6C v.3 за шлюзом WB-MGE в режиме Modbus TCP.
// Контакторы Kk подключены НЗ-контактом ко входу k (k = 1…4).
type MR6C struct {
	UnitID         uint8
	ContactorDelay time.Duration
	Firmware       string

	mu         sync.Mutex
	coils      [mr6c.NumOutputs]bool
	contactors [mr6c.NumOutputs]contactor
	faults     [mr6c.NumOutputs]Fault
	inputs     [mr6c.NumInputs]bool // входы без контакторов (5, 6, 0)
	holding    map[uint16]uint16

	lastPacket   time.Time
	safeMode     bool
	packets      uint64
	safeEntries  int
	lastSafeMode time.Time

	log *slog.Logger
}

type contactor struct {
	engaged   bool // положение механизма до последнего изменения катушки
	coil      bool
	changedAt time.Time
}

func (c contactor) at(now time.Time, delay time.Duration) bool {
	if c.coil != c.engaged && now.Sub(c.changedAt) >= delay {
		return c.coil
	}
	return c.engaged
}

// NewMR6C создаёт модуль. configured=true — регистры заранее соответствуют эталону
// safety, иначе — заводские значения (таймаут 10 с, безопасный режим ничего не меняет).
func NewMR6C(unitID uint8, safety mr6c.SafetyConfig, configured bool, logger *slog.Logger) *MR6C {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	m := &MR6C{
		UnitID:         unitID,
		ContactorDelay: 30 * time.Millisecond,
		Firmware:       "1.30.0-sim",
		holding:        map[uint16]uint16{},
		lastPacket:     time.Now(),
		log:            logger,
	}
	for _, r := range safety.Expected() {
		m.holding[r.Addr] = 0
	}
	m.holding[mr6c.RegPollTimeout] = 10
	if configured {
		for _, r := range safety.Expected() {
			m.holding[r.Addr] = r.Value
		}
	}
	for i := range mr6c.FirmwareVersionLen {
		m.holding[mr6c.RegFirmwareVersion+i] = 0
		if int(i) < len(m.Firmware) {
			m.holding[mr6c.RegFirmwareVersion+i] = uint16(m.Firmware[i])
		}
	}
	return m
}

// Run следит за таймаутом опроса и переводит модуль в безопасный режим.
func (m *MR6C) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.checkSafety(now)
		}
	}
}

func (m *MR6C) checkSafety(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.holding[mr6c.RegSafetySource]
	timeout := time.Duration(m.holding[mr6c.RegPollTimeout]) * time.Second
	if m.safeMode || src == 1 || now.Sub(m.lastPacket) < timeout {
		return
	}
	m.safeMode = true
	m.safeEntries++
	m.lastSafeMode = now
	for k := range uint16(mr6c.NumOutputs) {
		if m.holding[mr6c.RegSafetyBehaviorBase+k] == mr6c.ValueSafetyBehaviorSafe {
			m.setCoil(int(k), m.holding[mr6c.RegSafeStateBase+k] == 1, now)
		}
	}
	m.log.Warn("sim: safe mode entered", "silence", now.Sub(m.lastPacket).Round(time.Millisecond), "coils", m.coils)
}

// packet — любой принятый пакет сбрасывает таймер. Выход из безопасного режима не восстанавливает выходы.
func (m *MR6C) packet(now time.Time) {
	m.packets++
	m.lastPacket = now
	if m.safeMode {
		m.safeMode = false
		m.log.Info("sim: safe mode left, outputs unchanged")
	}
}

func (m *MR6C) setCoil(k int, on bool, now time.Time) {
	c := &m.contactors[k]
	c.engaged = c.at(now, m.ContactorDelay)
	if c.coil != on {
		c.coil, c.changedAt = on, now
	}
	m.coils[k] = on
}

// nc — состояние НЗ-контакта контактора на выходе k (0-based).
func (m *MR6C) nc(k int, now time.Time) bool {
	switch m.faults[k] {
	case FaultStuckReleased:
		return true
	case FaultStuckEngaged, FaultNCOpen:
		return false
	}
	return !m.contactors[k].at(now, m.ContactorDelay)
}

func (m *MR6C) input(n int, now time.Time) bool {
	if n >= 1 && n <= 4 {
		return m.nc(n-1, now)
	}
	return m.inputs[n]
}

// SetFault задаёт неисправность контактора на выходе Kk.
func (m *MR6C) SetFault(k int, f Fault) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.faults[k-1] = f
}

type Stats struct {
	Packets      uint64
	SafeMode     bool
	SafeEntries  int
	LastSafeMode time.Time
	Coils        [mr6c.NumOutputs]bool
}

func (m *MR6C) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Stats{m.packets, m.safeMode, m.safeEntries, m.lastSafeMode, m.coils}
}

// --- modbus.RequestHandler ---

func (m *MR6C) HandleCoils(req *modbus.CoilsRequest) ([]bool, error) {
	return m.coilsOp(req.UnitId, req.Addr, req.Quantity, req.IsWrite, req.Args)
}

func (m *MR6C) coilsOp(unit uint8, addr, qty uint16, write bool, args []bool) ([]bool, error) {
	if unit != m.UnitID {
		return nil, modbus.ErrGWTargetFailedToRespond
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	m.packet(now)
	if int(addr)+int(qty) > mr6c.NumOutputs {
		return nil, modbus.ErrIllegalDataAddress
	}
	if write {
		for i, v := range args {
			if m.coils[int(addr)+i] != v {
				m.log.Info("sim: coil", "k", int(addr)+i+1, "on", v)
			}
			m.setCoil(int(addr)+i, v, now)
		}
		return nil, nil
	}
	return append([]bool(nil), m.coils[addr:addr+qty]...), nil
}

func (m *MR6C) HandleDiscreteInputs(req *modbus.DiscreteInputsRequest) ([]bool, error) {
	return m.discrete(req.UnitId, req.Addr, req.Quantity)
}

func (m *MR6C) discrete(unit uint8, addr, qty uint16) ([]bool, error) {
	if unit != m.UnitID {
		return nil, modbus.ErrGWTargetFailedToRespond
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	m.packet(now)
	res := make([]bool, 0, qty)
	for a := addr; a < addr+qty; a++ {
		switch {
		case a < 6:
			res = append(res, m.input(int(a)+1, now))
		case a == 6:
			res = append(res, false)
		case a == mr6c.DiscreteInput0:
			res = append(res, m.input(0, now))
		case a >= mr6c.DiscreteRealStateBase && a < mr6c.DiscreteRealStateBase+mr6c.NumOutputs:
			res = append(res, m.coils[a-mr6c.DiscreteRealStateBase])
		default:
			return nil, modbus.ErrIllegalDataAddress
		}
	}
	return res, nil
}

func (m *MR6C) HandleHoldingRegisters(req *modbus.HoldingRegistersRequest) ([]uint16, error) {
	return m.holdingOp(req.UnitId, req.Addr, req.Quantity, req.IsWrite, req.Args)
}

func (m *MR6C) holdingOp(unit uint8, addr, qty uint16, write bool, args []uint16) ([]uint16, error) {
	if unit != m.UnitID {
		return nil, modbus.ErrGWTargetFailedToRespond
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.packet(time.Now())
	res := make([]uint16, 0, qty)
	for i := range qty {
		v, ok := m.holding[addr+i]
		if !ok {
			return nil, modbus.ErrIllegalDataAddress
		}
		res = append(res, v)
	}
	if write {
		for i, v := range args {
			m.holding[addr+uint16(i)] = v
		}
		return nil, nil
	}
	return res, nil
}

func (m *MR6C) HandleInputRegisters(req *modbus.InputRegistersRequest) ([]uint16, error) {
	return nil, modbus.ErrIllegalFunction
}

// Serve запускает Modbus TCP сервер, например на "tcp://127.0.0.1:5502".
func (m *MR6C) Serve(url string) (stop func() error, err error) {
	srv, err := modbus.NewServer(&modbus.ServerConfiguration{
		URL:        url,
		Timeout:    time.Minute,
		MaxClients: 8, // как у WB-MGE в режиме Modbus TCP
		Logger:     log.New(io.Discard, "", 0),
	}, m)
	if err != nil {
		return nil, err
	}
	if err := srv.Start(); err != nil {
		return nil, fmt.Errorf("sim: start %s: %w", url, err)
	}
	return srv.Stop, nil
}

// Dialer — соединение без сети, для тестов. Каждое открытие — новая «сессия».
func (m *MR6C) Dialer() mr6c.Dialer {
	return func(context.Context) (mr6c.Bus, error) { return memBus{m}, nil }
}

type memBus struct{ m *MR6C }

func (b memBus) ReadCoils(addr, qty uint16) ([]bool, error) {
	return b.m.coilsOp(b.m.UnitID, addr, qty, false, nil)
}
func (b memBus) ReadDiscreteInputs(addr, qty uint16) ([]bool, error) {
	return b.m.discrete(b.m.UnitID, addr, qty)
}
func (b memBus) WriteCoil(addr uint16, v bool) error {
	_, err := b.m.coilsOp(b.m.UnitID, addr, 1, true, []bool{v})
	return err
}
func (b memBus) ReadHoldingRegisters(addr, qty uint16) ([]uint16, error) {
	return b.m.holdingOp(b.m.UnitID, addr, qty, false, nil)
}
func (b memBus) WriteRegister(addr, v uint16) error {
	_, err := b.m.holdingOp(b.m.UnitID, addr, 1, true, []uint16{v})
	return err
}
func (b memBus) Close() error { return nil }
