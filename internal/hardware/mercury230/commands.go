package mercury230

import (
	"encoding/hex"
	"fmt"
	"time"
)

// Request — запрос протокола и ожидаемая длина ответа (кадр целиком, с адресом и CRC).
type Request struct {
	Name         string
	PDU          []byte // код запроса и параметры
	RespLen      int
	NeedsChannel bool // без открытого канала счётчик ответит кодом 5
	Secret       bool // PDU содержит пароль: в журналах маскируется
}

// Mask возвращает копию кадра запроса, в которой пароль заменён звёздочками-нулями.
// Кадр 01h: адрес, код, уровень, 6 байт пароля, CRC.
func (r Request) Mask(frame []byte) []byte {
	out := append([]byte(nil), frame...)
	if r.Secret && len(out) >= 9 {
		for i := 3; i < 9; i++ {
			out[i] = 0x2A // '*'
		}
	}
	return out
}

// TestLink — проверка связи (00h), ответ-состояние, канал не нужен.
func TestLink() Request {
	return Request{Name: "test", PDU: []byte{CmdTestLink}, RespLen: StatusLen}
}

// OpenChannel — открыть канал связи на уровне level (1 — потребитель, 2 — хозяин).
// Пароль — 6 сырых байт: кодировка «111111» у партий различается (01×6 или ASCII 31×6).
func OpenChannel(level uint8, password [6]byte) Request {
	pdu := append([]byte{CmdOpenChannel, level}, password[:]...)
	return Request{Name: "open", PDU: pdu, RespLen: StatusLen, Secret: true}
}

// CloseChannel — закрыть канал (02h).
func CloseChannel() Request {
	return Request{Name: "close", PDU: []byte{CmdCloseChannel}, RespLen: StatusLen}
}

// ReadSerial — серийный номер и дата выпуска (08 00), без канала. Ответ 1+7+2.
func ReadSerial() Request {
	return Request{Name: "serial", PDU: []byte{CmdReadParam, 0x00}, RespLen: 10}
}

// ReadPassport — серийный номер, дата, версия ПО, вариант исполнения (08 01 00). Ответ 1+24+2.
func ReadPassport() Request {
	return Request{Name: "passport", PDU: []byte{CmdReadParam, 0x01, 0x00}, RespLen: 27, NeedsChannel: true}
}

// ReadVariant — вариант исполнения, устаревшая форма (08 12), запасной путь. Ответ 1+6+2.
func ReadVariant() Request {
	return Request{Name: "variant", PDU: []byte{CmdReadParam, 0x12}, RespLen: 9, NeedsChannel: true}
}

// ReadTotalEnergy — энергия от сброса A+, A−, R+, R− по сумме тарифов (05 00 00). Ответ 1+16+2.
func ReadTotalEnergy() Request {
	return Request{Name: "energy_total", PDU: []byte{CmdReadArray, 0x00, 0x00}, RespLen: 19, NeedsChannel: true}
}

// ReadPhaseEnergy — пофазная A+ от сброса, сумма тарифов (05 60 00). Ответ 1+12+2.
func ReadPhaseEnergy() Request {
	return Request{Name: "energy_phase", PDU: []byte{CmdReadArray, 0x60, 0x00}, RespLen: 15, NeedsChannel: true}
}

// ReadActivePower — активная мощность, сумма и три фазы (08 16 bwri). Ответ 1+12+2.
// Какой BWRI (00h или 01h) работает у исполнения, определяется на стенде.
func ReadActivePower(bwri byte) Request {
	return Request{Name: fmt.Sprintf("power_group_%02x", bwri), PDU: []byte{CmdReadParam, 0x16, bwri}, RespLen: 15, NeedsChannel: true}
}

// ReadActivePowerPhase — активная мощность фазы 1…3 (08 11 0n), запасной путь. Ответ 1+3+2.
func ReadActivePowerPhase(phase int) Request {
	return Request{Name: fmt.Sprintf("power_p%d", phase), PDU: []byte{CmdReadParam, 0x11, byte(phase & 0x03)}, RespLen: 6, NeedsChannel: true}
}

// --- декодеры ---

// DecodeEnergy4 — 4 байта энергии в порядке 2-й, 1-й, 4-й, 3-й (1-й — старший), Вт·ч.
// masked — значение FF FF FF FF.
func DecodeEnergy4(p []byte) (wh uint32, masked bool) {
	if p[0] == 0xFF && p[1] == 0xFF && p[2] == 0xFF && p[3] == 0xFF {
		return 0, true
	}
	return uint32(p[1])<<24 | uint32(p[0])<<16 | uint32(p[3])<<8 | uint32(p[2]), false
}

// DecodePower3 — 3 байта мгновенной мощности, сотые доли Вт со знаком.
// Бит 7 первого байта — обратное направление активной мощности,
// бит 6 — направление реактивной, для P маскируется.
func DecodePower3(p []byte) int32 {
	n := int32(p[0]&0x3F)<<16 | int32(p[2])<<8 | int32(p[1])
	if p[0]&0x80 != 0 {
		n = -n
	}
	return n
}

// PhaseEnergy — A+ фаз 1…3 от сброса, сумма тарифов.
type PhaseEnergy [3]uint32

// DecodePhaseEnergy разбирает ответ 05 60 00. Маска хотя бы у одной фазы — ErrMasked:
// пофазный учёт не поддерживается исполнением.
func DecodePhaseEnergy(p []byte) (PhaseEnergy, error) {
	var e PhaseEnergy
	if len(p) != 12 {
		return e, fmt.Errorf("%w: phase energy %d bytes", ErrLength, len(p))
	}
	for i := range e {
		v, masked := DecodeEnergy4(p[4*i:])
		if masked {
			return PhaseEnergy{}, fmt.Errorf("phase %d: %w", i+1, ErrMasked)
		}
		e[i] = v
	}
	return e, nil
}

// TotalEnergy — энергия от сброса по сумме тарифов, Вт·ч (вар·ч для R).
type TotalEnergy struct {
	APlus, AMinus, RPlus, RMinus uint32
	// Masked — вид энергии не поддерживается (A+, A−, R+, R−).
	Masked [4]bool
}

// DecodeTotalEnergy разбирает ответ 05 00 00. Маска A+ — ErrMasked; маски
// остальных видов допустимы (однонаправленные исполнения).
func DecodeTotalEnergy(p []byte) (TotalEnergy, error) {
	var e TotalEnergy
	if len(p) != 16 {
		return e, fmt.Errorf("%w: total energy %d bytes", ErrLength, len(p))
	}
	dst := [4]*uint32{&e.APlus, &e.AMinus, &e.RPlus, &e.RMinus}
	for i, d := range dst {
		*d, e.Masked[i] = DecodeEnergy4(p[4*i:])
	}
	if e.Masked[0] {
		return e, fmt.Errorf("total energy A+: %w", ErrMasked)
	}
	return e, nil
}

// Power — активная мощность, сотые доли Вт со знаком.
type Power struct {
	TotalCW int32
	PhaseCW [3]int32
}

// DecodeGroupPower разбирает ответ 08 16: сумма, фаза 1, фаза 2, фаза 3.
func DecodeGroupPower(p []byte) (Power, error) {
	if len(p) != 12 {
		return Power{}, fmt.Errorf("%w: power %d bytes", ErrLength, len(p))
	}
	pw := Power{TotalCW: DecodePower3(p)}
	for i := range pw.PhaseCW {
		pw.PhaseCW[i] = DecodePower3(p[3*(i+1):])
	}
	return pw, nil
}

// Variant — вариант исполнения, 6 байт.
type Variant [6]byte

// PhaseEnergy — 5-й байт, бит 0: пофазный учёт энергии A+.
func (v Variant) PhaseEnergy() bool { return v[4]&0x01 != 0 }

// Interface — 4-й байт, биты 3–2: интерфейс (1 — RS-485).
func (v Variant) Interface() uint8 { return (v[3] >> 2) & 0x03 }

// SumModeBit — 3-й байт, бит 7: способ суммирования фаз («по модулю» или «со знаком»).
// Какое значение что означает у 230, сверить с описанием протокола на стенде.
func (v Variant) SumModeBit() bool { return v[2]&0x80 != 0 }

// Identity — паспортные данные счётчика.
type Identity struct {
	Serial       string    // 8 цифр
	SerialRaw    [4]byte   // сырые байты: формат номера подтверждается на стенде
	Manufactured time.Time // нулевое, если дата не разобрана
	Firmware     string    // «9.0.0»; пусто, если паспорт не прочитан
	Variant      Variant
	HasVariant   bool
}

// DecodeSerial разбирает ответ 08 00: 4 байта номера (каждый — две десятичные
// цифры в двоичном виде) и 3 байта даты выпуска (число, месяц, год).
func DecodeSerial(p []byte) (Identity, error) {
	if len(p) != 7 {
		return Identity{}, fmt.Errorf("%w: serial %d bytes", ErrLength, len(p))
	}
	var id Identity
	copy(id.SerialRaw[:], p[:4])
	id.Serial = fmt.Sprintf("%02d%02d%02d%02d", p[0], p[1], p[2], p[3])
	id.Manufactured = decodeDate(p[4], p[5], p[6])
	return id, nil
}

// DecodePassport разбирает ответ 08 01 00: номер (4), дата (3), версия ПО (3),
// вариант исполнения (6), далее служебные байты.
func DecodePassport(p []byte) (Identity, error) {
	if len(p) != 24 {
		return Identity{}, fmt.Errorf("%w: passport %d bytes", ErrLength, len(p))
	}
	id, _ := DecodeSerial(p[:7])
	id.Firmware = fmt.Sprintf("%d.%d.%d", p[7], p[8], p[9])
	copy(id.Variant[:], p[10:16])
	id.HasVariant = true
	return id, nil
}

// DecodeVariant разбирает ответ 08 12.
func DecodeVariant(p []byte) (Variant, error) {
	var v Variant
	if len(p) != len(v) {
		return v, fmt.Errorf("%w: variant %d bytes", ErrLength, len(p))
	}
	copy(v[:], p)
	return v, nil
}

func decodeDate(d, m, y byte) time.Time {
	if d < 1 || d > 31 || m < 1 || m > 12 || y > 99 {
		return time.Time{}
	}
	t := time.Date(2000+int(y), time.Month(m), int(d), 0, 0, 0, 0, time.UTC)
	if t.Day() != int(d) { // 31 февраля и т. п.
		return time.Time{}
	}
	return t
}

// ParsePassword — 12 hex-символов в 6 сырых байт.
func ParsePassword(s string) ([6]byte, error) {
	var pw [6]byte
	if len(s) != 12 {
		return pw, fmt.Errorf("mercury230: password must be 12 hex chars (6 raw bytes), got %d chars", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return pw, fmt.Errorf("mercury230: password: %w", err)
	}
	copy(pw[:], b)
	return pw, nil
}
