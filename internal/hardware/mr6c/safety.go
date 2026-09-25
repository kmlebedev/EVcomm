package mr6c

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// SafetyConfig — утверждённые параметры безопасного режима.
type SafetyConfig struct {
	PollTimeoutS uint16 // регистр 8
	Source       uint16 // регистр 19: 2 или 0
}

// ExpectedRegister — один регистр эталона.
type ExpectedRegister struct {
	Addr  uint16
	Value uint16
	Name  string
}

// Expected — эталон настроек MR6C (раздел 5 дорожной карты). Применяется ко всем
// шести выходам и семи входам, включая неиспользуемые.
func (c SafetyConfig) Expected() []ExpectedRegister {
	regs := []ExpectedRegister{
		{RegLegacyInputMode, 0, "legacy input mode control"},
		{RegPowerOnState, ValuePowerOnSafeState, "outputs state after power on"},
		{RegPollTimeout, c.PollTimeoutS, "safety poll timeout, s"},
		{RegSafetySource, c.Source, "safety mode source"},
	}
	for n := range NumInputs {
		regs = append(regs, ExpectedRegister{inputModeReg(n), ValueInputModeNoControl, fmt.Sprintf("input %d mode", n)})
	}
	for k := range uint16(NumOutputs) {
		regs = append(regs,
			ExpectedRegister{RegSafeStateBase + k, ValueSafeStateOff, fmt.Sprintf("K%d safe state", k+1)},
			ExpectedRegister{RegSafetyBehaviorBase + k, ValueSafetyBehaviorSafe, fmt.Sprintf("K%d safety behaviour", k+1)},
			ExpectedRegister{RegSafetyInputCtlBase + k, ValueSafetyInputDisable, fmt.Sprintf("K%d inputs control in safety mode", k+1)},
		)
	}
	slices.SortFunc(regs, func(a, b ExpectedRegister) int { return int(a.Addr) - int(b.Addr) })
	return regs
}

type Mismatch struct {
	ExpectedRegister
	Actual uint16
}

func (m Mismatch) String() string {
	return fmt.Sprintf("%s (reg %d): %d, want %d", m.Name, m.Addr, m.Actual, m.Value)
}

// SafetyReport — результат сверки с эталоном. Включение поста разрешено только при OK().
type SafetyReport struct {
	CheckedAt  time.Time
	Firmware   string
	Mismatches []Mismatch
	Err        error // эталон не удалось прочитать
}

func (r SafetyReport) OK() bool {
	return r.Err == nil && len(r.Mismatches) == 0 && !r.CheckedAt.IsZero()
}

func (r SafetyReport) String() string {
	switch {
	case r.CheckedAt.IsZero():
		return "not checked"
	case r.Err != nil:
		return "read error: " + r.Err.Error()
	case len(r.Mismatches) == 0:
		return "ok"
	}
	parts := make([]string, len(r.Mismatches))
	for i, m := range r.Mismatches {
		parts[i] = m.String()
	}
	return fmt.Sprintf("%d mismatches: %s", len(parts), strings.Join(parts, "; "))
}

// verifySafety читает эталонные регистры блоками подряд идущих адресов (FC03).
func verifySafety(bus Bus, cfg SafetyConfig, now time.Time) SafetyReport {
	rep := SafetyReport{CheckedAt: now, Firmware: readFirmware(bus)}
	regs := cfg.Expected()
	for start := 0; start < len(regs); {
		end := start + 1
		for end < len(regs) && regs[end].Addr == regs[end-1].Addr+1 {
			end++
		}
		vals, err := bus.ReadHoldingRegisters(regs[start].Addr, uint16(end-start))
		if err != nil {
			rep.Err = fmt.Errorf("read holding %d..%d: %w", regs[start].Addr, regs[end-1].Addr, err)
			return rep
		}
		for i, v := range vals {
			if exp := regs[start+i]; v != exp.Value {
				rep.Mismatches = append(rep.Mismatches, Mismatch{exp, v})
			}
		}
		start = end
	}
	return rep
}

// applySafety записывает эталон (FC06). Только для стенда и ввода модуля в работу.
func applySafety(bus Bus, cfg SafetyConfig) error {
	for _, r := range cfg.Expected() {
		if err := bus.WriteRegister(r.Addr, r.Value); err != nil {
			return fmt.Errorf("write %s (reg %d): %w", r.Name, r.Addr, err)
		}
	}
	return nil
}

func readFirmware(bus Bus) string {
	vals, err := bus.ReadHoldingRegisters(RegFirmwareVersion, FirmwareVersionLen)
	if err != nil {
		return ""
	}
	var sb strings.Builder
	for _, v := range vals {
		if v == 0 {
			break
		}
		sb.WriteByte(byte(v))
	}
	return sb.String()
}
