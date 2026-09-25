package controller

import (
	"errors"
	"log/slog"
	"time"

	"github.com/kmlebedev/EVcomm/internal/domain"
	"github.com/kmlebedev/EVcomm/internal/hardware/mercury230"
	"github.com/kmlebedev/EVcomm/internal/registry"
	"github.com/kmlebedev/EVcomm/internal/rtu"
)

// MeterTiming переводит тайминги реестра в тайминги транзакции прозрачного моста.
func MeterTiming(m registry.Meter) rtu.Timing {
	return rtu.Timing{Connect: m.ConnectTimeout, Response: m.ResponseTimeout, FrameGap: m.FrameGap, StatusTail: m.StatusTail}
}

// MeterConfig собирает конфигурацию адаптера «Меркурий 230» поста; dial == nil —
// TCP к прозрачному мосту из реестра. Пароль читается из окружения.
func MeterConfig(p registry.Post, dial rtu.Dialer, startJitter time.Duration, logger *slog.Logger) (mercury230.Config, error) {
	m := p.Meter
	if m == nil || m.Driver != registry.MeterMercury230 {
		return mercury230.Config{}, errors.New("post has no mercury230 meter")
	}
	pw, err := m.Password()
	if err != nil {
		return mercury230.Config{}, err
	}
	if dial == nil {
		dial = rtu.TCPDialer(m.Gateway, MeterTiming(*m))
	}
	var phases [3]domain.LineID
	copy(phases[:], m.PhaseMap) // registry.Validate: ровно 3
	return mercury230.Config{
		Name:           string(p.ID),
		Dial:           dial,
		Address:        m.Address,
		AccessLevel:    m.AccessLevel,
		Password:       pw,
		ExpectedSerial: m.Serial,
		PhaseMap:       phases,
		GroupPowerBWRI: m.GroupPowerBWRI,
		PowerPeriod:    m.PowerPeriod,
		EnergyPeriod:   m.EnergyPeriod,
		IdentityPeriod: m.IdentityPeriod,
		ReconnectMin:   m.ReconnectMin,
		ReconnectMax:   m.ReconnectMax,
		StartJitter:    startJitter,
		Logger:         logger,
	}, nil
}
