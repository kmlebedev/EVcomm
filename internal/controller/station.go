package controller

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/kmlebedev/EVcomm/internal/hardware/mr6c"
	"github.com/kmlebedev/EVcomm/internal/registry"
	"github.com/kmlebedev/EVcomm/internal/supervision"
)

// Station — пост в сборе: аренда, адаптер MR6C и контроллер.
type Station struct {
	Post    registry.Post
	Lease   *supervision.Lease
	Adapter *mr6c.Adapter
	Ctl     *Post
}

// SafetyConfig переводит эталон реестра в значения регистров.
func SafetyConfig(p registry.Post) mr6c.SafetyConfig {
	src, _ := p.Safety.Source.Register() // проверено registry.Validate
	return mr6c.SafetyConfig{PollTimeoutS: p.Safety.PollTimeoutS, Source: src}
}

// NewStation собирает пост; dial == nil — Modbus TCP к шлюзу из реестра.
func NewStation(p registry.Post, dial mr6c.Dialer, startJitter time.Duration, logger *slog.Logger) *Station {
	if dial == nil {
		dial = mr6c.TCPDialer(p.Gateway, p.MR6CAddress, p.Timing.ModbusTimeout)
	}
	lease := &supervision.Lease{}
	ad := mr6c.New(mr6c.Config{
		Name:         string(p.ID),
		Dial:         dial,
		Gate:         lease,
		PollPeriod:   p.Timing.PollPeriod,
		SplitInputs:  p.InputRead == registry.InputReadSplit,
		RealState:    p.RealStateSupported(),
		Safety:       SafetyConfig(p),
		ReconnectMin: p.Timing.ReconnectMin,
		ReconnectMax: p.Timing.ReconnectMax,
		StartJitter:  startJitter,
		Logger:       logger,
	})
	return &Station{
		Post:    p,
		Lease:   lease,
		Adapter: ad,
		Ctl:     New(Config{Post: p, HW: ad, Lease: lease, Logger: logger}),
	}
}

// Run запускает адаптер и контроллер и ждёт их завершения.
func (s *Station) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { _ = s.Adapter.Run(ctx) })
	wg.Go(func() { _ = s.Ctl.Run(ctx) })
	wg.Wait()
}

// ShutdownOff отключает все линии и ждёт подтверждения (до timeout).
func (s *Station) ShutdownOff(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ids, err := s.Ctl.AllOff(ctx, "shutdown")
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.Ctl.Wait(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
