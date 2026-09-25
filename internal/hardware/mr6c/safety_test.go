package mr6c

import (
	"testing"
	"time"
)

// fakeBus — holding-регистры в памяти, считает запросы.
type fakeBus struct {
	regs  map[uint16]uint16
	reads int
}

func (b *fakeBus) ReadCoils(uint16, uint16) ([]bool, error)          { return nil, nil }
func (b *fakeBus) ReadDiscreteInputs(uint16, uint16) ([]bool, error) { return nil, nil }
func (b *fakeBus) WriteCoil(uint16, bool) error                      { return nil }
func (b *fakeBus) Close() error                                      { return nil }
func (b *fakeBus) WriteRegister(a, v uint16) error                   { b.regs[a] = v; return nil }
func (b *fakeBus) ReadHoldingRegisters(a, n uint16) ([]uint16, error) {
	b.reads++
	out := make([]uint16, n)
	for i := range n {
		out[i] = b.regs[a+i]
	}
	return out, nil
}

func TestVerifySafetyDetectsMismatchAndApplyFixesIt(t *testing.T) {
	cfg := SafetyConfig{PollTimeoutS: 3, Source: 2}
	bus := &fakeBus{regs: map[uint16]uint16{RegPollTimeout: 10}}

	rep := verifySafety(bus, cfg, time.Now())
	if rep.OK() || len(rep.Mismatches) == 0 {
		t.Fatalf("factory config accepted: %s", rep)
	}
	// Блоки: 5–6, 8–14, 16, 19, 930–935, 938–943, 946–951 и версия прошивки.
	if bus.reads != 8 {
		t.Fatalf("reads = %d, want 8", bus.reads)
	}
	if err := applySafety(bus, cfg); err != nil {
		t.Fatal(err)
	}
	if rep := verifySafety(bus, cfg, time.Now()); !rep.OK() {
		t.Fatalf("after apply: %s", rep)
	}
	if bus.regs[RegSafetyBehaviorBase+5] != ValueSafetyBehaviorSafe || bus.regs[RegInput0Mode] != ValueInputModeNoControl {
		t.Fatal("reference not applied to K6 / input 0")
	}
}
