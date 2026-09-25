package supervision

import (
	"testing"
	"time"
)

func TestLeaseValidAndExpiry(t *testing.T) {
	var l Lease
	now := time.Now()
	if l.Valid(now) || !l.Deadline(now).IsZero() {
		t.Fatal("zero Lease is valid")
	}
	l.Extend(now, time.Second)
	if !l.Valid(now) || !l.Valid(now.Add(time.Second-time.Nanosecond)) {
		t.Fatal("lease not valid before deadline")
	}
	if l.Valid(now.Add(time.Second)) {
		t.Fatal("lease valid at deadline")
	}
	if d, want := l.Deadline(now.Add(300*time.Millisecond)), now.Add(time.Second); !d.Equal(want) {
		t.Fatalf("Deadline = %v, want %v", d, want)
	}
	l.Extend(now.Add(time.Second), time.Second)
	if !l.Valid(now.Add(1500 * time.Millisecond)) {
		t.Fatal("Extend did not move the deadline")
	}
}

func TestLeaseExpiresInRealTime(t *testing.T) {
	var l Lease
	l.Extend(time.Now(), 20*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if l.Valid(time.Now()) {
		t.Fatal("lease valid after ttl")
	}
}

func TestLeaseRevoke(t *testing.T) {
	var l Lease
	now := time.Now()
	l.Extend(now, time.Second)
	l.Revoke()
	if l.Valid(now) || !l.Deadline(now).IsZero() {
		t.Fatal("revoked lease is valid")
	}
	l.Extend(now, time.Second)
	if !l.Valid(now) {
		t.Fatal("lease not renewed after Revoke")
	}
}

// Время без монотонных показаний — это только системные часы, которые могут
// шагнуть. Такое время не продлевает аренду и не делает её действующей.
func TestLeaseIgnoresWallClock(t *testing.T) {
	var l Lease
	now := time.Now()
	l.Extend(now.Round(0), time.Hour)
	if l.Valid(now) {
		t.Fatal("lease extended by time without monotonic reading")
	}

	l.Extend(now, time.Second)
	// Часы отведены назад на час: по системному времени срок далеко впереди.
	for _, wall := range []time.Time{now.Round(0), now.Add(-time.Hour).Round(0), time.Unix(0, 0), {}} {
		if l.Valid(wall) {
			t.Errorf("Valid(%v) without monotonic reading", wall)
		}
	}
	if !l.Deadline(now.Round(0)).IsZero() {
		t.Error("Deadline without monotonic reading")
	}
	// Часы переведены вперёд: продление по ним не сдвигает монотонный срок.
	l.Extend(now.Add(time.Hour).Round(0), time.Hour)
	if !l.Valid(now) || l.Valid(now.Add(time.Second)) {
		t.Fatal("Extend without monotonic reading changed the lease")
	}
}
