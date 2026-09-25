// Package supervision — контроль работоспособности: аренда цикла управления, метрики.
package supervision

import (
	"sync/atomic"
	"time"
)

// base — точка отсчёта сроков аренды. Разность с ней берётся по монотонным часам,
// поэтому шаг системных часов (NTP, RTC, ручная смена даты) не продлевает аренду
// и не обрывает её раньше срока.
var base = time.Now()

// Lease — аренда поста. Продлевает её только цикл управления поста; адаптер
// MR6C обменивается с модулем, лишь пока аренда действует. Если цикл завис,
// трафик прекращается и MR6C по таймауту опроса переходит в безопасный режим.
// Продлевать аренду из отдельной heartbeat-goroutine нельзя (раздел 5 дорожной карты).
//
// now во всех методах — из time.Now(). Время без монотонных показаний
// (t.Round(0), time.Unix, после сериализации) аренду не продлевает, и по нему она недействительна.
type Lease struct {
	deadline atomic.Int64 // срок от base по монотонным часам, нс; 0 — аренды нет
}

func (l *Lease) Extend(now time.Time, ttl time.Duration) {
	if off, ok := sinceBase(now); ok {
		l.deadline.Store(int64(off + ttl))
	}
}

func (l *Lease) Revoke() { l.deadline.Store(0) }

func (l *Lease) Valid(now time.Time) bool {
	d := l.deadline.Load()
	off, ok := sinceBase(now)
	return d != 0 && ok && int64(off) < d
}

// Deadline — срок аренды в шкале now, для отображения. Нулевое время, если
// аренды нет или у now нет монотонных показаний.
func (l *Lease) Deadline(now time.Time) time.Time {
	d := l.deadline.Load()
	off, ok := sinceBase(now)
	if d == 0 || !ok {
		return time.Time{}
	}
	return now.Add(time.Duration(d) - off)
}

// sinceBase — смещение now от base. ok=false, если у now нет монотонных
// показаний: Sub тогда сравнивал бы системные часы.
func sinceBase(now time.Time) (off time.Duration, ok bool) {
	// == сравнивает и монотонные показания (Equal — нет), Round(0) их снимает.
	if now == now.Round(0) { //nolint:staticcheck // QF1009: нужен именно ==
		return 0, false
	}
	return now.Sub(base), true
}
