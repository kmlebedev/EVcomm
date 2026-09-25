// Command mercury230-sim — симулятор «Меркурий 230 ART» за прозрачным мостом WB-MGE
// (TCP, один мастер на порт): энергия фаз растёт по заданной мощности, канал 240 с,
// коды ошибок, маска пофазного учёта, сегментация ответа.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kmlebedev/EVcomm/internal/hardware/mercury230"
	"github.com/kmlebedev/EVcomm/internal/sim"
)

func main() {
	def := sim.DefaultMercury230Config()
	listen := flag.String("listen", "127.0.0.1:5503", "TCP listen address (WB-MGE RS485-2 transparent bridge)")
	addr := flag.Uint("address", uint(def.Address), "meter network address 1..240")
	serial := flag.String("serial", def.Serial, "serial number, 8 digits")
	pw1 := flag.String("password1", "010101010101", "level 1 password, 12 hex chars (ASCII 111111 = 313131313131)")
	pw2 := flag.String("password2", "020202020202", "level 2 password, 12 hex chars")
	powerLevel := flag.Uint("power-level", 1, "minimal access level for instantaneous values (1 or 2)")
	bwri := flag.Int("group-power-bwri", 0, "BWRI accepted by 08 16; -1 — 08 16 not supported")
	noPhase := flag.Bool("no-phase-energy", false, "meter variant without per-phase A+ (FF FF FF FF)")
	noPassport := flag.Bool("no-passport", false, "08 01 00 not supported (fallback to 08 00 + 08 12)")
	load := flag.String("load", "3680,0,0", "active power of phases 1,2,3, W (negative — reverse)")
	energy := flag.String("energy", "1000000,2000000,3000000", "initial A+ of phases 1,2,3, Wh")
	delay := flag.Duration("delay", 40*time.Millisecond, "meter response delay")
	segment := flag.Int("segment", 0, "send responses in chunks of N bytes (0 — whole frame)")
	segDelay := flag.Duration("segment-delay", 2*time.Millisecond, "pause between chunks")
	ttl := flag.Duration("channel-ttl", 240*time.Second, "channel lifetime after the last valid request")
	flag.Parse()

	cfg := def
	cfg.Address, cfg.Serial = uint8(*addr), *serial
	cfg.PowerLevel, cfg.GroupPowerBWRI = uint8(*powerLevel), *bwri
	cfg.NoPhaseEnergy, cfg.NoPassport = *noPhase, *noPassport
	cfg.ResponseDelay, cfg.Segment, cfg.SegmentDelay, cfg.ChannelTTL = *delay, *segment, *segDelay, *ttl
	var err error
	if cfg.Passwords[0], err = mercury230.ParsePassword(*pw1); err != nil {
		fatal(err)
	}
	if cfg.Passwords[1], err = mercury230.ParsePassword(*pw2); err != nil {
		fatal(err)
	}
	loadW, err := triple(*load)
	if err != nil {
		fatal(fmt.Errorf("-load: %w", err))
	}
	energyWh, err := triple(*energy)
	if err != nil {
		fatal(fmt.Errorf("-energy: %w", err))
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	m, err := sim.NewMercury230(cfg, log)
	if err != nil {
		fatal(err)
	}
	m.SetEnergy(energyWh)
	m.SetLoad(loadW)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	bound, stop, err := m.Serve(*listen)
	if err != nil {
		fatal(err)
	}
	log.Info("Mercury 230 simulator started", "listen", bound, "address", cfg.Address, "serial", cfg.Serial,
		"load_w", loadW, "power_level", cfg.PowerLevel, "group_power_bwri", cfg.GroupPowerBWRI, "phase_energy", !cfg.NoPhaseEnergy)
	<-ctx.Done()
	if err := stop(); err != nil {
		log.Error("stop", "err", err)
	}
}

func triple(s string) ([3]float64, error) {
	var r [3]float64
	parts := strings.Split(s, ",")
	if len(parts) != 3 {
		return r, fmt.Errorf("need 3 comma-separated values, got %q", s)
	}
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return r, err
		}
		r[i] = v
	}
	return r, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "mercury230-sim:", err)
	os.Exit(1)
}
