// Command mr6c-sim — симулятор WB-MR6C v.3 за WB-MGE (Modbus TCP) с контакторами,
// НЗ-обратной связью и безопасным режимом по таймауту опроса.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kmlebedev/EVcomm/internal/hardware/mr6c"
	"github.com/kmlebedev/EVcomm/internal/sim"
)

func main() {
	listen := flag.String("listen", "tcp://127.0.0.1:5502", "Modbus TCP listen URL")
	unit := flag.Uint("unit", 1, "MR6C Modbus address")
	pollTimeout := flag.Uint("poll-timeout", 3, "reference poll_timeout_s (register 8)")
	factory := flag.Bool("factory", false, "start with factory settings instead of the safety reference")
	delay := flag.Duration("contactor-delay", 30*time.Millisecond, "contactor operate/release time")
	stuck := flag.Int("stuck", 0, "simulate contactor Kn that does not operate (0 — none)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	m := sim.NewMR6C(uint8(*unit), mr6c.SafetyConfig{PollTimeoutS: uint16(*pollTimeout), Source: 2}, !*factory, log)
	m.ContactorDelay = *delay
	if *stuck > 0 {
		m.SetFault(*stuck, sim.FaultStuckReleased)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	stop, err := m.Serve(*listen)
	if err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := stop(); err != nil {
			log.Error("stop", "err", err)
		}
	}()
	log.Info("MR6C simulator started", "listen", *listen, "unit", *unit, "configured", !*factory)
	m.Run(ctx)
}
