// Package measurements — общий интерфейс источника показаний счётчика поста.
// Реализации: hardware/mercury230.Adapter (нативный Go через прозрачный мост)
// и wbmqtt (wb-mqtt-serial через MQTT). Выбор делается конфигурацией поста.
package measurements

import (
	"context"
	"time"

	"github.com/kmlebedev/EVcomm/internal/domain"
)

// Source — показания счётчика по линиям поста.
type Source interface {
	// Power — последняя активная мощность линии, Вт. Не блокирует.
	Power(line domain.LineID) domain.Observation[float64]
	// Energy — последняя накопленная активная энергия A+ линии, Вт·ч. Не блокирует.
	Energy(line domain.LineID) domain.Observation[uint64]
	// EnergySnapshot — внеочередное чтение учётного регистра линии; блокирует до ответа или ctx.
	EnergySnapshot(ctx context.Context, line domain.LineID) (EnergyEvidence, error)
	Status() SourceStatus
}

// EnergyEvidence — показание учётного регистра для расчёта сессии: целые Вт·ч,
// серийный номер счётчика и сырые кадры обмена для неизменяемого журнала.
type EnergyEvidence struct {
	Line       domain.LineID  `json:"line"`
	Source     string         `json:"source"` // «mercury230:<serial>»
	Serial     string         `json:"serial"`
	Register   string         `json:"register"` // какой регистр прочитан: «A+ phase 2», «A+ total»
	Wh         uint64         `json:"wh"`
	ObservedAt time.Time      `json:"observed_at"` // время хоста по получении ответа
	Quality    domain.Quality `json:"quality"`
	Issues     []string       `json:"issues,omitempty"` // причины disputed
	RawRequest []byte         `json:"raw_request,omitempty"`
	RawResp    []byte         `json:"raw_response,omitempty"`
}

// SourceStatus — состояние связи со счётчиком.
type SourceStatus struct {
	Source        string    `json:"source"`
	State         string    `json:"state"` // connecting, online, gateway_busy, config_error, …
	Connected     bool      `json:"connected"`
	Healthy       bool      `json:"healthy"`  // последний обмен успешен
	Mismatch      bool      `json:"mismatch"` // серийный номер не совпал с реестром
	LastError     string    `json:"last_error,omitempty"`
	LastErrorAt   time.Time `json:"last_error_at,omitzero"`
	LastSuccessAt time.Time `json:"last_success_at,omitzero"`
	Alarms        []string  `json:"alarms,omitempty"`
}
