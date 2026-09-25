package mr6c

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/simonvetter/modbus"
)

// Bus — транзакции Modbus к одному модулю. Используется только из goroutine адаптера.
type Bus interface {
	ReadCoils(addr, qty uint16) ([]bool, error)
	ReadDiscreteInputs(addr, qty uint16) ([]bool, error)
	WriteCoil(addr uint16, value bool) error
	ReadHoldingRegisters(addr, qty uint16) ([]uint16, error)
	WriteRegister(addr, value uint16) error
	Close() error
}

// Dialer открывает соединение с модулем. TCP-подключение само не создаёт Modbus-трафика,
// поэтому выполняется и без действующей аренды.
type Dialer func(ctx context.Context) (Bus, error)

// TCPDialer — Modbus TCP к порту шлюза WB-MGE; unitID — адрес MR6C на RS485-1.
func TCPDialer(addr string, unitID uint8, timeout time.Duration) Dialer {
	return func(ctx context.Context) (Bus, error) {
		c, err := modbus.NewClient(&modbus.ClientConfiguration{
			URL:     "tcp://" + addr,
			Timeout: timeout,
			Logger:  log.New(io.Discard, "", 0),
		})
		if err != nil {
			return nil, err
		}
		if err := c.Open(); err != nil {
			return nil, fmt.Errorf("connect %s: %w", addr, err)
		}
		if err := c.SetUnitId(unitID); err != nil {
			_ = c.Close()
			return nil, err
		}
		return tcpBus{c}, nil
	}
}

type tcpBus struct{ c *modbus.ModbusClient }

func (b tcpBus) ReadCoils(addr, qty uint16) ([]bool, error) { return b.c.ReadCoils(addr, qty) }
func (b tcpBus) ReadDiscreteInputs(addr, qty uint16) ([]bool, error) {
	return b.c.ReadDiscreteInputs(addr, qty)
}
func (b tcpBus) WriteCoil(addr uint16, v bool) error { return b.c.WriteCoil(addr, v) }
func (b tcpBus) ReadHoldingRegisters(addr, qty uint16) ([]uint16, error) {
	return b.c.ReadRegisters(addr, qty, modbus.HOLDING_REGISTER)
}
func (b tcpBus) WriteRegister(addr, v uint16) error { return b.c.WriteRegister(addr, v) }
func (b tcpBus) Close() error                       { return b.c.Close() }

// IsException сообщает, что устройство или шлюз ответили исключением Modbus:
// TCP-сессия со шлюзом исправна, переподключаться не нужно.
func IsException(err error) bool {
	for _, e := range []error{
		modbus.ErrIllegalFunction, modbus.ErrIllegalDataAddress, modbus.ErrIllegalDataValue,
		modbus.ErrServerDeviceFailure, modbus.ErrAcknowledge, modbus.ErrServerDeviceBusy,
		modbus.ErrMemoryParityError, modbus.ErrGWPathUnavailable, modbus.ErrGWTargetFailedToRespond,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// IsModuleSilent — шлюз жив, но модуль на RS485-1 не ответил.
func IsModuleSilent(err error) bool {
	return errors.Is(err, modbus.ErrGWTargetFailedToRespond) || errors.Is(err, modbus.ErrGWPathUnavailable)
}
