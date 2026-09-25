package sim

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/kmlebedev/EVcomm/internal/rtu"
)

// Mercury230Config — исполнение и поведение симулятора «Меркурий 230».
type Mercury230Config struct {
	Address  uint8
	Serial   string  // 8 цифр
	Made     [3]byte // число, месяц, год (двоично)
	Firmware [3]byte
	// Passwords — пароли уровней 1 и 2, сырые байты.
	Passwords [2][6]byte
	// PowerLevel — минимальный уровень доступа для мгновенных значений.
	PowerLevel uint8
	// GroupPowerBWRI — BWRI, на который отвечает 08 16; -1 — 08 16 не поддержан.
	GroupPowerBWRI int
	NoPhaseEnergy  bool // бит варианта 0 и маска FF FF FF FF в массиве 6h
	NoPassport     bool // 08 01 00 → код 1 (запасной путь 08 00 + 08 12)
	ChannelTTL     time.Duration
	// ResponseDelay — задержка ответа; Segment > 0 — ответ частями по Segment байт
	// с паузой SegmentDelay (как WB-MGE в прозрачном режиме).
	ResponseDelay time.Duration
	Segment       int
	SegmentDelay  time.Duration
	// SlotReleaseDelay — слот TCP остаётся занятым после закрытия соединения клиентом:
	// мост ещё не обработал FIN, и новое подключение отклоняется.
	SlotReleaseDelay time.Duration
}

// DefaultMercury230Config — ART-01 с пофазным учётом, пароли 01×6 / 02×6, мгновенные на уровне 1.
func DefaultMercury230Config() Mercury230Config {
	return Mercury230Config{
		Address:        145,
		Serial:         "32874766",
		Made:           [3]byte{14, 3, 24},
		Firmware:       [3]byte{9, 0, 0},
		Passwords:      [2][6]byte{{1, 1, 1, 1, 1, 1}, {2, 2, 2, 2, 2, 2}},
		PowerLevel:     1,
		GroupPowerBWRI: 0x00,
		ChannelTTL:     240 * time.Second,
		ResponseDelay:  5 * time.Millisecond,
	}
}

// Mercury230 — модель счётчика за прозрачным мостом: энергия фаз растёт по
// заданной мощности, канал связи с истечением, коды ошибок 1/3/5, маска,
// «молчание», смена серийного номера.
type Mercury230 struct {
	mu        sync.Mutex
	cfg       Mercury230Config
	serial    [4]byte
	loadW     [3]float64
	importWh  [3]float64 // A+
	exportWh  [3]float64 // A−
	updated   time.Time
	level     uint8
	openUntil time.Time
	silent    bool
	internal  bool
	requests  uint64
	busy      bool // TCP-слот занят
	log       *slog.Logger
}

func NewMercury230(cfg Mercury230Config, logger *slog.Logger) (*Mercury230, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.ChannelTTL <= 0 {
		cfg.ChannelTTL = 240 * time.Second
	}
	m := &Mercury230{cfg: cfg, updated: time.Now(), log: logger}
	if err := m.SetSerial(cfg.Serial); err != nil {
		return nil, err
	}
	return m, nil
}

// SetSerial меняет серийный номер (имитация замены счётчика).
func (m *Mercury230) SetSerial(s string) error {
	if len(s) != 8 {
		return fmt.Errorf("sim: serial must be 8 digits")
	}
	var b [4]byte
	for i := range b {
		v, err := strconv.Atoi(s[2*i : 2*i+2])
		if err != nil {
			return fmt.Errorf("sim: serial: %w", err)
		}
		b[i] = byte(v)
	}
	m.mu.Lock()
	m.serial = b
	m.mu.Unlock()
	return nil
}

// SetLoad задаёт активную мощность фаз, Вт (отрицательная — обратное направление).
func (m *Mercury230) SetLoad(w [3]float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.integrate(time.Now())
	m.loadW = w
}

// SetEnergy задаёт накопленную A+ фаз, Вт·ч (например, «сброс регистров»).
func (m *Mercury230) SetEnergy(wh [3]float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.integrate(time.Now())
	m.importWh = wh
}

// SetSilent — счётчик перестаёт отвечать (обрыв RS-485, нет питания).
func (m *Mercury230) SetSilent(v bool) {
	m.mu.Lock()
	m.silent = v
	m.mu.Unlock()
}

// SetInternalError — ответ кодом 2 на чтения.
func (m *Mercury230) SetInternalError(v bool) {
	m.mu.Lock()
	m.internal = v
	m.mu.Unlock()
}

// ExpireChannel закрывает канал, как по истечении 240 с.
func (m *Mercury230) ExpireChannel() {
	m.mu.Lock()
	m.openUntil = time.Time{}
	m.mu.Unlock()
}

func (m *Mercury230) Requests() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests
}

// EnergyWh — текущая A+ фаз (целые Вт·ч, как в регистрах).
func (m *Mercury230) EnergyWh() [3]uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.integrate(time.Now())
	var r [3]uint32
	for i, v := range m.importWh {
		r[i] = uint32(v)
	}
	return r
}

func (m *Mercury230) integrate(now time.Time) {
	h := now.Sub(m.updated).Hours()
	m.updated = now
	for i, w := range m.loadW {
		if w >= 0 {
			m.importWh[i] += w * h
		} else {
			m.exportWh[i] -= w * h
		}
	}
}

func (m *Mercury230) variant() [6]byte {
	v := [6]byte{0xB4, 0xE3, 0x97, 0x04, 0x01, 0x00} // 4-й байт: интерфейс RS-485, 5-й: пофазный учёт
	if m.cfg.NoPhaseEnergy {
		v[4] = 0x00
	}
	return v
}

// Handle обрабатывает кадр запроса и возвращает кадр ответа; nil — счётчик молчит
// (ошибка CRC, чужой или широковещательный адрес, «молчание»).
func (m *Mercury230) Handle(req []byte) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(req) < 4 || !rtu.CheckCRC(req) || m.silent {
		return nil
	}
	addr := req[0]
	if addr != m.cfg.Address && addr != 0 {
		return nil
	}
	pdu := req[1 : len(req)-2]
	now := time.Now()
	m.integrate(now)
	m.requests++

	data, code := m.dispatch(pdu, now)
	if code == 0 && data != nil && m.level > 0 {
		m.openUntil = now.Add(m.cfg.ChannelTTL) // корректный запрос продлевает канал
	}
	if data == nil {
		return rtu.AppendCRC([]byte{addr, code})
	}
	return rtu.AppendCRC(append([]byte{addr}, data...))
}

// dispatch возвращает поле данных ответа либо (nil, код состояния).
func (m *Mercury230) dispatch(pdu []byte, now time.Time) ([]byte, byte) {
	channel := func() byte {
		if m.level == 0 || now.After(m.openUntil) {
			m.level = 0
			return 5
		}
		return 0
	}
	switch pdu[0] {
	case 0x00:
		if len(pdu) != 1 {
			return nil, 1
		}
		return nil, 0
	case 0x01:
		if len(pdu) != 8 || pdu[1] < 1 || pdu[1] > 2 {
			return nil, 1
		}
		if [6]byte(pdu[2:8]) != m.cfg.Passwords[pdu[1]-1] {
			return nil, 3
		}
		m.level, m.openUntil = pdu[1], now.Add(m.cfg.ChannelTTL)
		return nil, 0
	case 0x02:
		m.level = 0
		return nil, 0
	case 0x05:
		if c := channel(); c != 0 {
			return nil, c
		}
		if m.internal {
			return nil, 2
		}
		if len(pdu) != 3 || pdu[2] != 0 {
			return nil, 1
		}
		switch pdu[1] {
		case 0x00:
			var ap, am float64
			for i := range m.importWh {
				ap += m.importWh[i]
				am += m.exportWh[i]
			}
			d := energy4(uint32(ap))
			d = append(d, energy4(uint32(am))...)
			d = append(d, energy4(0)...)
			return append(d, energy4(0)...), 0
		case 0x60:
			var d []byte
			for _, v := range m.importWh {
				if m.cfg.NoPhaseEnergy {
					d = append(d, 0xFF, 0xFF, 0xFF, 0xFF)
				} else {
					d = append(d, energy4(uint32(v))...)
				}
			}
			return d, 0
		}
		return nil, 1
	case 0x08:
		if len(pdu) < 2 {
			return nil, 1
		}
		if pdu[1] == 0x00 && len(pdu) == 2 {
			return append(m.serial[:], m.cfg.Made[:]...), 0
		}
		if c := channel(); c != 0 {
			return nil, c
		}
		if m.internal {
			return nil, 2
		}
		switch {
		case pdu[1] == 0x01 && len(pdu) == 3 && pdu[2] == 0x00:
			if m.cfg.NoPassport {
				return nil, 1
			}
			v := m.variant()
			d := append(m.serial[:], m.cfg.Made[:]...)
			d = append(d, m.cfg.Firmware[:]...)
			d = append(d, v[:]...)
			return append(d, make([]byte, 8)...), 0
		case pdu[1] == 0x12 && len(pdu) == 2:
			v := m.variant()
			return v[:], 0
		case pdu[1] == 0x11 && len(pdu) == 3:
			if pdu[2]&0xFC != 0 { // только P (параметр 0, вид 0)
				return nil, 1
			}
			if m.level < m.cfg.PowerLevel {
				return nil, 3
			}
			return power3(m.powerCW(int(pdu[2] & 0x03))), 0
		case pdu[1] == 0x16 && len(pdu) == 3:
			if m.cfg.GroupPowerBWRI < 0 || int(pdu[2]) != m.cfg.GroupPowerBWRI {
				return nil, 1
			}
			if m.level < m.cfg.PowerLevel {
				return nil, 3
			}
			var d []byte
			for ph := range 4 {
				d = append(d, power3(m.powerCW(ph))...)
			}
			return d, 0
		}
		return nil, 1
	}
	return nil, 1
}

// powerCW — мощность фазы 1…3 или сумма (0), сотые доли Вт.
func (m *Mercury230) powerCW(phase int) int32 {
	if phase == 0 {
		return int32(math.Round((m.loadW[0] + m.loadW[1] + m.loadW[2]) * 100))
	}
	return int32(math.Round(m.loadW[phase-1] * 100))
}

func energy4(v uint32) []byte {
	return []byte{byte(v >> 16), byte(v >> 24), byte(v), byte(v >> 8)}
}

func power3(cw int32) []byte {
	var sign byte
	if cw < 0 {
		sign, cw = 0x80, -cw
	}
	mag := uint32(cw) & 0x3FFFFF
	return []byte{byte(mag>>16)&0x3F | sign, byte(mag), byte(mag >> 8)}
}

// requestLen — длина кадра запроса по коду и параметру; 0 — не определена.
func requestLen(b []byte) int {
	if len(b) < 2 {
		return 0
	}
	switch b[1] {
	case 0x00, 0x02:
		return 4
	case 0x01:
		return 11
	case 0x05:
		return 6
	case 0x08:
		if len(b) < 3 {
			return 0
		}
		switch b[2] {
		case 0x00, 0x12:
			return 5
		default:
			return 6
		}
	}
	return 0
}

// Serve принимает TCP-подключения как WB-MGE в прозрачном режиме: один клиент;
// второй принимается и сразу закрывается, первый не вытесняется.
func (m *Mercury230) Serve(addr string) (net.Addr, func() error, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			m.mu.Lock()
			busy := m.busy
			m.busy = true
			m.mu.Unlock()
			if busy {
				m.log.Info("sim: second client rejected", "remote", c.RemoteAddr())
				_ = c.Close()
				continue
			}
			wg.Go(func() {
				defer func() {
					_ = c.Close()
					m.mu.Lock()
					delay := m.cfg.SlotReleaseDelay
					m.mu.Unlock()
					time.Sleep(delay)
					m.mu.Lock()
					m.busy = false
					m.mu.Unlock()
				}()
				m.log.Info("sim: client connected", "remote", c.RemoteAddr())
				m.serveConn(c)
			})
		}
	})
	stop := func() error {
		err := ln.Close()
		wg.Wait()
		return err
	}
	return ln.Addr(), stop, nil
}

// Dialer — соединение через net.Pipe без сети, для тестов.
func (m *Mercury230) Dialer(t rtu.Timing) rtu.Dialer {
	return func(ctx context.Context) (rtu.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer func() { _ = server.Close() }()
			m.serveConn(server)
		}()
		return rtu.NewConn(client, t), nil
	}
}

// serveConn выделяет кадры из потока байт: по длине, известной из кода запроса,
// иначе по первому префиксу с верным CRC.
func (m *Mercury230) serveConn(c net.Conn) {
	var buf []byte
	tmp := make([]byte, 64)
	for {
		n, err := c.Read(tmp)
		if err != nil {
			return
		}
		buf = append(buf, tmp[:n]...)
		for {
			l := requestLen(buf)
			if l == 0 {
				for k := 4; k <= len(buf); k++ {
					if rtu.CheckCRC(buf[:k]) {
						l = k
						break
					}
				}
			}
			if l == 0 || l > len(buf) {
				if len(buf) > 64 {
					buf = nil
				}
				break
			}
			frame := buf[:l]
			buf = append([]byte(nil), buf[l:]...)
			resp := m.Handle(frame)
			if resp == nil {
				continue
			}
			if !m.write(c, resp) {
				return
			}
		}
	}
}

func (m *Mercury230) write(c net.Conn, resp []byte) bool {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	time.Sleep(cfg.ResponseDelay)
	seg := cfg.Segment
	if seg <= 0 {
		seg = len(resp)
	}
	for i := 0; i < len(resp); i += seg {
		if _, err := c.Write(resp[i:min(i+seg, len(resp))]); err != nil {
			return false
		}
		if cfg.SegmentDelay > 0 && i+seg < len(resp) {
			time.Sleep(cfg.SegmentDelay)
		}
	}
	return true
}
