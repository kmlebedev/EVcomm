package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kmlebedev/EVcomm/internal/sim"
)

func serveSim(t *testing.T, mod func(*sim.Mercury230Config)) string {
	t.Helper()
	cfg := sim.DefaultMercury230Config()
	cfg.ResponseDelay, cfg.Segment, cfg.SegmentDelay = 2*time.Millisecond, 5, time.Millisecond
	if mod != nil {
		mod(&cfg)
	}
	m, err := sim.NewMercury230(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.SetLoad([3]float64{3680, 0, 0})
	addr, stop, err := m.Serve("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop() })
	return addr.String()
}

func runBench(t *testing.T, gateway string, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	base := []string{"-registry", "", "-gateway", gateway, "-address", "145"}
	code := run(context.Background(), append(base, args...), &out, &out, nil)
	return out.String(), code
}

func TestCheckPassesOnSimulator(t *testing.T) {
	gw := serveSim(t, nil)
	out, code := runBench(t, gw, "-serial", "32874766", "check", "-n", "20")
	if code != 0 || strings.Contains(out, "FAIL  ") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{"1   08 00", "2   01h", "3   08 01 00", "4   05 60 00", "5   мгновенная P", "9   задержки", "10  сегментация TCP", "11  второй клиент", "PASS 8"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestCheckReportsFailures(t *testing.T) {
	gw := serveSim(t, func(c *sim.Mercury230Config) {
		c.Passwords[0] = [6]byte{0x31, 0x31, 0x31, 0x31, 0x31, 0x31}
	})
	out, code := runBench(t, gw, "-serial", "11111111", "-password", "010101010101", "check", "-n", "5")
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	for _, want := range []string{"≠ реестр 11111111", "отклонён: код 3", "принят 31×6"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestPowerMatrixRecommendsLevel2(t *testing.T) {
	gw := serveSim(t, func(c *sim.Mercury230Config) { c.PowerLevel, c.GroupPowerBWRI = 2, 1 })
	out, code := runBench(t, gw, "-password", "010101010101", "-password2", "020202020202", "power")
	if code != 0 || !strings.Contains(out, "access_level: 2") || !strings.Contains(out, "уровень 2  08 16 01  P = 3680.00") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestExpiryAndRaw(t *testing.T) {
	// Команды идут подряд, а мост освобождает слот с задержкой: утилита должна
	// переждать «порт занят», а не падать.
	gw := serveSim(t, func(c *sim.Mercury230Config) {
		c.ChannelTTL, c.SlotReleaseDelay = 100*time.Millisecond, 200*time.Millisecond
	})
	out, code := runBench(t, gw, "-password", "010101010101", "expiry", "-wait", "200ms")
	if code != 0 || !strings.Contains(out, "PASS  код 5 → 01h → повтор") {
		t.Fatalf("expiry exit %d:\n%s", code, out)
	}
	out, code = runBench(t, gw, "-password", "010101010101", "raw", "-len", "15", "08", "16", "00")
	if code != 0 || !strings.Contains(out, "данные: ") {
		t.Fatalf("raw exit %d:\n%s", code, out)
	}
	// Инвариант «только чтение»: код записи 03h кодек не формирует.
	out, code = runBench(t, gw, "-password", "010101010101", "raw", "-len", "4", "03", "00")
	if code != 1 || !strings.Contains(out, "read-only") {
		t.Fatalf("write command: exit %d:\n%s", code, out)
	}
}

// Если порт занят дольше busyWait, ошибка объясняет причину.
func TestBusyGatewayReported(t *testing.T) {
	gw := serveSim(t, nil)
	c, err := net.Dial("tcp", gw)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	time.Sleep(20 * time.Millisecond) // первый клиент занял слот
	start := time.Now()
	out, code := runBench(t, gw, "-password", "010101010101", "read")
	if code != 1 || !strings.Contains(out, "порт моста занят другим мастером") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if d := time.Since(start); d < busyWait {
		t.Fatalf("gave up after %s, want >= %s", d, busyWait)
	}
}

func TestWatchReconciles(t *testing.T) {
	gw := serveSim(t, nil)
	out, code := runBench(t, gw, "-password", "010101010101", "watch", "-interval", "150ms", "-duration", "700ms")
	if code != 0 || !strings.Contains(out, "PASS  за") || strings.Contains(out, "FAIL  ") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}
