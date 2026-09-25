package rtu

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCRC16Vectors(t *testing.T) {
	// Кадры из раздела 4.2 архитектуры и тестов wb-mqtt-serial.
	for _, f := range []string{
		"91 00 6C 20",
		"91 01 01 01 01 01 01 01 01 D6 17",
		"91 02 ED E1",
		"91 08 00 27 ED",
		"91 05 60 00 14 D9",
		"00 01 01 01 01 01 01 01 01 77 81",
		"00 00 01 B0",
		"00 30 00 29 C5 30 00 29 00 04 00 9D 95 50 99",
	} {
		b := unhex(t, f)
		if !CheckCRC(b) {
			t.Errorf("CheckCRC(%s) = false", f)
		}
		if got := AppendCRC(bytes.Clone(b[:len(b)-2])); !bytes.Equal(got, b) {
			t.Errorf("AppendCRC = % X, want %s", got, f)
		}
	}
	if CheckCRC([]byte{0x91, 0x00}) || CheckCRC(unhex(t, "91 00 6C 21")) {
		t.Error("bad frame accepted")
	}
}

// server — прозрачный мост в тесте: на каждый принятый запрос вызывает handle.
type server struct {
	ln net.Listener
}

func newServer(t *testing.T, handle func(c net.Conn)) *server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				handle(c)
			}()
		}
	}()
	return &server{ln: ln}
}

// respond читает запросы длиной reqLen и отвечает reply(req).
func respond(reqLen int, reply func(c net.Conn, req []byte)) func(net.Conn) {
	return func(c net.Conn) {
		for {
			req := make([]byte, reqLen)
			if _, err := readFull(c, req); err != nil {
				return
			}
			reply(c, req)
		}
	}
}

func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		k, err := c.Read(b[n:])
		n += k
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func dial(t *testing.T, s *server, tm Timing) Conn {
	t.Helper()
	c, err := TCPDialer(s.ln.Addr().String(), tm)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

var (
	req4    = []byte{0x91, 0x05, 0x60, 0x00, 0x14, 0xD9}
	resp15  = func(t testing.TB) []byte { return AppendCRC(unhex(t, "91 30 00 29 C5 30 00 29 00 04 00 9D 95")) }
	fastTim = Timing{Response: 200 * time.Millisecond, FrameGap: 2 * time.Millisecond, StatusTail: 20 * time.Millisecond}
)

func TestTransactSegmented(t *testing.T) {
	want := resp15(t)
	for _, seg := range []int{1, 2, 4, 7, 15} {
		t.Run(fmt.Sprint("seg", seg), func(t *testing.T) {
			s := newServer(t, respond(len(req4), func(c net.Conn, _ []byte) {
				for i := 0; i < len(want); i += seg {
					_, _ = c.Write(want[i:min(i+seg, len(want))])
					time.Sleep(3 * time.Millisecond)
				}
			}))
			c := dial(t, s, fastTim)
			got, err := c.Transact(context.Background(), req4, len(want))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("got % X, %v", got, err)
			}
			if st := c.(StatsReporter).LastStats(); st.Segments < 1 {
				t.Fatalf("segments = %d", st.Segments)
			}
		})
	}
}

func TestTransactStatusFrame(t *testing.T) {
	status := unhex(t, "91 05 00 00")
	status = AppendCRC(status[:2])
	s := newServer(t, respond(len(req4), func(c net.Conn, _ []byte) {
		_, _ = c.Write(status[:1])
		time.Sleep(2 * time.Millisecond)
		_, _ = c.Write(status[1:])
	}))
	c := dial(t, s, fastTim)
	start := time.Now()
	got, err := c.Transact(context.Background(), req4, 15)
	if err != nil || !bytes.Equal(got, status) {
		t.Fatalf("got % X, %v", got, err)
	}
	if d := time.Since(start); d >= fastTim.Response {
		t.Fatalf("status frame waited full response timeout: %s", d)
	}
}

// Первые 4 байта длинного ответа случайно образуют кадр с верным CRC; остаток
// приходит в пределах StatusTail — кадр собирается целиком.
func TestTransactLongFrameWithValidCRCPrefix(t *testing.T) {
	prefix := AppendCRC([]byte{0x91, 0x05})
	long := AppendCRC(append(bytes.Clone(prefix), 1, 2, 3, 4, 5, 6, 7, 8, 9))
	s := newServer(t, respond(len(req4), func(c net.Conn, _ []byte) {
		_, _ = c.Write(long[:4])
		time.Sleep(5 * time.Millisecond)
		_, _ = c.Write(long[4:])
	}))
	c := dial(t, s, fastTim)
	got, err := c.Transact(context.Background(), req4, len(long))
	if err != nil || !bytes.Equal(got, long) {
		t.Fatalf("got % X, %v", got, err)
	}
}

func TestTransactDrainsGarbageAndLateResponse(t *testing.T) {
	want := resp15(t)
	calls := 0
	s := newServer(t, respond(len(req4), func(c net.Conn, _ []byte) {
		calls++
		if calls == 1 {
			// Опоздавший ответ: приходит после таймаута клиента.
			time.Sleep(fastTim.Response + 30*time.Millisecond)
		}
		_, _ = c.Write(want)
	}))
	c := dial(t, s, fastTim)
	got, err := c.Transact(context.Background(), req4, len(want))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("first: % X, %v; want timeout", got, err)
	}
	time.Sleep(80 * time.Millisecond) // опоздавший ответ лёг во входной буфер
	got, err = c.Transact(context.Background(), req4, len(want))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("second: % X, %v", got, err)
	}
	if st := c.(StatsReporter).LastStats(); st.Discarded != len(want) {
		t.Fatalf("discarded = %d, want %d", st.Discarded, len(want))
	}
}

func TestTransactPartialTimeout(t *testing.T) {
	want := resp15(t)
	s := newServer(t, respond(len(req4), func(c net.Conn, _ []byte) { _, _ = c.Write(want[:9]) }))
	c := dial(t, s, fastTim)
	got, err := c.Transact(context.Background(), req4, len(want))
	if !errors.Is(err, ErrTimeout) || len(got) != 9 {
		t.Fatalf("got % X, %v", got, err)
	}
}

func TestTransactFrameGap(t *testing.T) {
	status := AppendCRC([]byte{0x91, 0x00})
	var (
		mu    sync.Mutex
		times []time.Time
	)
	s := newServer(t, respond(4, func(c net.Conn, _ []byte) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		_, _ = c.Write(status)
	}))
	tm := fastTim
	tm.FrameGap = 30 * time.Millisecond
	c := dial(t, s, tm)
	ping := AppendCRC([]byte{0x91, 0x00})
	for range 3 {
		if _, err := c.Transact(context.Background(), ping, 4); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(times); i++ {
		if d := times[i].Sub(times[i-1]); d < tm.FrameGap {
			t.Fatalf("gap %s < %s", d, tm.FrameGap)
		}
	}
}

func TestTransactGatewayBusyAndClosed(t *testing.T) {
	busy := newServer(t, func(net.Conn) {}) // принять и сразу закрыть, как WB-MGE для второго клиента
	c := dial(t, busy, fastTim)
	time.Sleep(10 * time.Millisecond)
	if _, err := c.Transact(context.Background(), req4, 15); !errors.Is(err, ErrGatewayBusy) {
		t.Fatalf("err = %v, want ErrGatewayBusy", err)
	}

	want := resp15(t)
	once := newServer(t, func(c net.Conn) {
		req := make([]byte, len(req4))
		if _, err := readFull(c, req); err == nil {
			_, _ = c.Write(want)
		}
	})
	c = dial(t, once, fastTim)
	if _, err := c.Transact(context.Background(), req4, 15); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := c.Transact(context.Background(), req4, 15); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

func TestTransactContextCancel(t *testing.T) {
	want := resp15(t)
	calls := 0
	s := newServer(t, respond(len(req4), func(c net.Conn, _ []byte) {
		if calls++; calls > 1 {
			_, _ = c.Write(want)
		}
	}))
	tm := fastTim
	tm.Response = 5 * time.Second
	c := dial(t, s, tm)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	start := time.Now()
	if _, err := c.Transact(ctx, req4, 15); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("cancel took %s", d)
	}
	// Соединение пригодно после отмены: дедлайны следующей транзакции не сбиты.
	if got, err := c.Transact(context.Background(), req4, 15); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("after cancel: % X, %v", got, err)
	}
}
