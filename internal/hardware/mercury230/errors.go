package mercury230

import (
	"context"
	"errors"
	"fmt"

	"github.com/kmlebedev/EVcomm/internal/rtu"
)

// Коды младшей тетрады байта состояния (протокол, раздел «Байт состояния»).
const (
	StatusOK             uint8 = 0
	StatusInvalidCommand uint8 = 1 // недопустимая команда или параметр
	StatusInternal       uint8 = 2 // внутренняя ошибка счётчика
	StatusAccessDenied   uint8 = 3 // недостаточен уровень доступа
	StatusClockCorrected uint8 = 4 // часы уже корректировались сегодня
	StatusChannelClosed  uint8 = 5 // канал связи не открыт
)

// StatusError — счётчик ответил кадром-состоянием с ненулевым кодом.
type StatusError struct {
	Code uint8 // младшая тетрада
	Raw  byte  // байт состояния целиком
}

func (e *StatusError) Error() string {
	var s string
	switch e.Code {
	case StatusInvalidCommand:
		s = "invalid command or parameter"
	case StatusInternal:
		s = "internal meter error"
	case StatusAccessDenied:
		s = "access level insufficient"
	case StatusClockCorrected:
		s = "clock already corrected today"
	case StatusChannelClosed:
		s = "channel not open"
	default:
		s = "unknown status"
	}
	return fmt.Sprintf("mercury230: status %d (%02X): %s", e.Code, e.Raw, s)
}

// IsStatus сообщает, что err — ответ-состояние с кодом code.
func IsStatus(err error, code uint8) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == code
}

var (
	// ErrMasked — значение FF FF FF FF: вид энергии не поддерживается исполнением.
	// Это не ноль и не ошибка связи.
	ErrMasked = errors.New("mercury230: value masked (not supported by this meter variant)")
	ErrCRC    = errors.New("mercury230: bad CRC")
	// ErrAddress — ответ с чужим адресом (второй счётчик на шине или сдвиг кадра).
	ErrAddress = errors.New("mercury230: response address mismatch")
	ErrLength  = errors.New("mercury230: unexpected response length")
	// ErrUnexpectedStatus — кадр-состояние «норма» вместо данных.
	ErrUnexpectedStatus = errors.New("mercury230: status frame instead of data")
	// ErrForbiddenCommand — кодек не формирует команды, кроме чтения (инвариант 6.1).
	ErrForbiddenCommand = errors.New("mercury230: command not allowed (read-only codec)")
	// ErrTimeout — счётчик не ответил. На ошибку адреса или CRC запроса он молчит,
	// поэтому таймаут может означать и тишину на линии, и испорченный запрос.
	ErrTimeout = rtu.ErrTimeout
)

// Kind — класс ошибки для реакции адаптера (раздел 6.3).
type Kind int

const (
	KindNone      Kind = iota
	KindTransport      // TCP: переподключение
	KindLink           // таймаут, CRC, адрес, длина: счётчик ошибок подряд
	KindStatus         // счётчик ответил кодом состояния
	KindMasked         // значение не поддерживается исполнением
	KindCancelled      // отменено вызывающим
)

func (k Kind) String() string {
	return [...]string{"none", "transport", "link", "status", "masked", "cancelled"}[k]
}

// Classify относит ошибку к классу.
func Classify(err error) Kind {
	var se *StatusError
	switch {
	case err == nil:
		return KindNone
	case errors.As(err, &se):
		return KindStatus
	case errors.Is(err, ErrMasked):
		return KindMasked
	case errors.Is(err, rtu.ErrTimeout), errors.Is(err, ErrCRC), errors.Is(err, ErrAddress),
		errors.Is(err, ErrLength), errors.Is(err, ErrUnexpectedStatus):
		return KindLink
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return KindCancelled
	}
	return KindTransport
}
