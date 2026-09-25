package mr6c

import (
	"fmt"
	"time"
)

// Snapshot — одно достоверное чтение модуля: выходы, фактическое состояние и входы.
type Snapshot struct {
	Seq    uint64
	ReadAt time.Time // момент завершения чтения
	// Outputs — coil K1…K6 (индекс 0 = K1).
	Outputs [NumOutputs]bool
	// RealState — фактическое состояние K1…K6. Если прошивка его не отдаёт, равно Outputs.
	RealState [NumOutputs]bool
	// Inputs — входы 0…6 (индекс = номер входа). true — цепь замкнута.
	Inputs [NumInputs]bool
}

// Output возвращает состояние выхода Kk (k = 1…6).
func (s Snapshot) Output(k int) bool { return s.Outputs[k-1] }

// Real возвращает фактическое состояние выхода Kk (k = 1…6).
func (s Snapshot) Real(k int) bool { return s.RealState[k-1] }

// Input возвращает состояние входа n (n = 0…6).
func (s Snapshot) Input(n int) bool { return s.Inputs[n] }

func (s Snapshot) String() string {
	b := func(v bool) byte {
		if v {
			return '1'
		}
		return '0'
	}
	var out, real, in [7]byte
	for i := range NumOutputs {
		out[i], real[i] = b(s.Outputs[i]), b(s.RealState[i])
	}
	for i := range NumInputs {
		in[i] = b(s.Inputs[i])
	}
	return fmt.Sprintf("K1..6=%s real=%s in0..6=%s", out[:NumOutputs], real[:NumOutputs], in[:])
}

type readOptions struct {
	splitInputs bool
	realState   bool
}

// readSnapshot: FC02 входы, FC02 фактическое состояние 96…101, FC01 выходы 0…5.
func readSnapshot(bus Bus, opt readOptions) (Snapshot, error) {
	var s Snapshot
	if opt.splitInputs {
		in, err := bus.ReadDiscreteInputs(DiscreteInputBase, 6)
		if err != nil {
			return s, fmt.Errorf("read inputs 1..6: %w", err)
		}
		in0, err := bus.ReadDiscreteInputs(DiscreteInput0, 1)
		if err != nil {
			return s, fmt.Errorf("read input 0: %w", err)
		}
		copy(s.Inputs[1:], in)
		s.Inputs[0] = in0[0]
	} else {
		in, err := bus.ReadDiscreteInputs(DiscreteInputBase, 8)
		if err != nil {
			return s, fmt.Errorf("read inputs 0..7: %w", err)
		}
		copy(s.Inputs[1:], in[:6])
		s.Inputs[0] = in[DiscreteInput0]
	}
	if opt.realState {
		real, err := bus.ReadDiscreteInputs(DiscreteRealStateBase, NumOutputs)
		if err != nil {
			return s, fmt.Errorf("read real state: %w", err)
		}
		copy(s.RealState[:], real)
	}
	out, err := bus.ReadCoils(CoilOutputBase, NumOutputs)
	if err != nil {
		return s, fmt.Errorf("read outputs: %w", err)
	}
	copy(s.Outputs[:], out)
	if !opt.realState {
		s.RealState = s.Outputs
	}
	return s, nil
}
