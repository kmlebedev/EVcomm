// Package supervision — контроль работоспособности: аренда цикла управления, метрики.
package supervision

import (
	"sync/atomic"
	"time"
)

// Lease — аренда поста. Продлевает её только цикл управления поста; адаптер
// MR6C обменивается с модулем, лишь пока аренда действует. Если цикл завис,
// трафик прекращается и MR6C по таймауту опроса переходит в безопасный режим.
// Продлевать аренду из отдельной heartbeat-goroutine нельзя (раздел 5 дорожной карты).
type Lease struct {
	deadline atomic.Int64 // UnixNano; 0 — аренды нет
}

func (l *Lease) Extend(now time.Time, ttl time.Duration) {
	l.deadline.Store(now.Add(ttl).UnixNano())
}

func (l *Lease) Revoke() { l.deadline.Store(0) }

func (l *Lease) Valid(now time.Time) bool {
	d := l.deadline.Load()
	return d != 0 && now.UnixNano() < d
}

func (l *Lease) Deadline() time.Time {
	d := l.deadline.Load()
	if d == 0 {
		return time.Time{}
	}
	return time.Unix(0, d)
}
