package mercury230

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kmlebedev/EVcomm/internal/rtu"
)

// Exchange — одна транзакция на шине: сырые кадры для журнала и стенда.
// В кадре открытия канала пароль замаскирован.
type Exchange struct {
	Request  string
	Req      []byte
	Resp     []byte // как принят, с адресом и CRC; при таймауте — принятая часть
	Started  time.Time
	Duration time.Duration
	Err      error
}

type MeterConfig struct {
	Address     uint8 // 1…240
	AccessLevel uint8 // 1 или 2
	Password    [6]byte
	// Trace получает каждую транзакцию, включая повторы. Необязателен.
	Trace func(Exchange)
}

// Meter — сессия протокола поверх rtu.Conn: открытие канала, повтор при коде 5,
// повтор при испорченном кадре. Не потокобезопасен, как и Conn.
type Meter struct {
	conn rtu.Conn
	cfg  MeterConfig
}

func NewMeter(conn rtu.Conn, cfg MeterConfig) *Meter {
	return &Meter{conn: conn, cfg: cfg}
}

// Do выполняет запрос и возвращает поле данных ответа и последнюю транзакцию.
//   - Испорченный кадр (CRC, адрес, длина) — один повтор: транспорт перед ним
//     сбрасывает входной буфер. Таймаут не повторяется: его считает адаптер.
//   - Код 5 («канал не открыт») на запрос, которому нужен канал, — открыть
//     канал и повторить один раз.
func (m *Meter) Do(ctx context.Context, r Request) ([]byte, Exchange, error) {
	p, ex, err := m.once(ctx, r)
	if err != nil && isCorrupt(err) {
		p, ex, err = m.once(ctx, r)
	}
	if err != nil && r.NeedsChannel && IsStatus(err, StatusChannelClosed) {
		if oerr := m.Open(ctx); oerr != nil {
			return nil, ex, fmt.Errorf("%s: reopen channel: %w", r.Name, oerr)
		}
		p, ex, err = m.once(ctx, r)
	}
	if err != nil {
		return nil, ex, fmt.Errorf("%s: %w", r.Name, err)
	}
	return p, ex, nil
}

func isCorrupt(err error) bool {
	return errors.Is(err, ErrCRC) || errors.Is(err, ErrAddress) || errors.Is(err, ErrLength) || errors.Is(err, ErrUnexpectedStatus)
}

func (m *Meter) once(ctx context.Context, r Request) ([]byte, Exchange, error) {
	frame, err := Encode(m.cfg.Address, r.PDU)
	if err != nil {
		return nil, Exchange{Request: r.Name}, err
	}
	ex := Exchange{Request: r.Name, Req: r.Mask(frame), Started: time.Now()}
	resp, err := m.conn.Transact(ctx, frame, r.RespLen)
	ex.Duration = time.Since(ex.Started)
	ex.Resp = append([]byte(nil), resp...)
	var p []byte
	if err == nil {
		p, err = Decode(m.cfg.Address, resp, r.RespLen)
	}
	ex.Err = err
	if m.cfg.Trace != nil {
		m.cfg.Trace(ex)
	}
	return p, ex, err
}

// Ping — проверка связи 00h.
func (m *Meter) Ping(ctx context.Context) error {
	_, _, err := m.Do(ctx, TestLink())
	return err
}

// Open открывает канал на уровне и с паролем из конфигурации.
func (m *Meter) Open(ctx context.Context) error {
	return m.OpenWith(ctx, m.cfg.AccessLevel, m.cfg.Password)
}

// OpenWith открывает канал на заданном уровне (для стенда: подбор уровня и кодировки пароля).
func (m *Meter) OpenWith(ctx context.Context, level uint8, password [6]byte) error {
	_, _, err := m.Do(ctx, OpenChannel(level, password))
	return err
}

// CloseChannel — 02h.
func (m *Meter) CloseChannel(ctx context.Context) error {
	_, _, err := m.Do(ctx, CloseChannel())
	return err
}

// ReadSerial — 08 00: адрес и связь подтверждены без канала.
func (m *Meter) ReadSerial(ctx context.Context) (Identity, error) {
	p, _, err := m.Do(ctx, ReadSerial())
	if err != nil {
		return Identity{}, err
	}
	return DecodeSerial(p)
}

// ReadIdentity читает паспорт 08 01 00. Если исполнение его не знает (код 1),
// собирает сведения из 08 00 и 08 12; вариант исполнения тогда может остаться неизвестным.
func (m *Meter) ReadIdentity(ctx context.Context) (Identity, error) {
	p, _, err := m.Do(ctx, ReadPassport())
	if err == nil {
		return DecodePassport(p)
	}
	if !IsStatus(err, StatusInvalidCommand) {
		return Identity{}, err
	}
	id, err := m.ReadSerial(ctx)
	if err != nil {
		return Identity{}, err
	}
	p, _, verr := m.Do(ctx, ReadVariant())
	if verr == nil {
		if v, derr := DecodeVariant(p); derr == nil {
			id.Variant, id.HasVariant = v, true
		}
	} else if Classify(verr) == KindTransport || Classify(verr) == KindCancelled {
		return id, verr
	}
	return id, nil
}

// ReadPhaseEnergy — 05 60 00: A+ по фазам.
func (m *Meter) ReadPhaseEnergy(ctx context.Context) (PhaseEnergy, Exchange, error) {
	p, ex, err := m.Do(ctx, ReadPhaseEnergy())
	if err != nil {
		return PhaseEnergy{}, ex, err
	}
	e, err := DecodePhaseEnergy(p)
	return e, ex, err
}

// ReadTotalEnergy — 05 00 00: A+, A−, R+, R− по сумме тарифов.
func (m *Meter) ReadTotalEnergy(ctx context.Context) (TotalEnergy, Exchange, error) {
	p, ex, err := m.Do(ctx, ReadTotalEnergy())
	if err != nil {
		return TotalEnergy{}, ex, err
	}
	e, err := DecodeTotalEnergy(p)
	return e, ex, err
}

// ReadGroupPower — 08 16 bwri: сумма и три фазы одним запросом.
func (m *Meter) ReadGroupPower(ctx context.Context, bwri byte) (Power, error) {
	p, _, err := m.Do(ctx, ReadActivePower(bwri))
	if err != nil {
		return Power{}, err
	}
	return DecodeGroupPower(p)
}

// ReadPhasePower — запасной путь: 3 × 08 11 0n. Сумма — по фазам.
func (m *Meter) ReadPhasePower(ctx context.Context) (Power, error) {
	var pw Power
	for i := range pw.PhaseCW {
		p, _, err := m.Do(ctx, ReadActivePowerPhase(i+1))
		if err != nil {
			return Power{}, err
		}
		if len(p) != 3 {
			return Power{}, fmt.Errorf("%w: phase power %d bytes", ErrLength, len(p))
		}
		pw.PhaseCW[i] = DecodePower3(p)
		pw.TotalCW += pw.PhaseCW[i]
	}
	return pw, nil
}
