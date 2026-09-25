package mercury230

import (
	"testing"

	"github.com/kmlebedev/EVcomm/internal/rtu"
)

// FuzzDecode: никакой вход не вызывает panic; принятый кадр имеет верный CRC и адрес.
func FuzzDecode(f *testing.F) {
	for _, s := range []string{
		"00 30 00 29 C5 30 00 29 00 04 00 9D 95 50 99",
		"00 30 00 28 C5 FF FF FF FF 04 00 9C 95 FF FF FF FF 44 AB",
		"91 0B 00 3B 0B 00 3B 00 00 00 80 D2 04 F1 82",
		"00 05 C1 B3",
		"00 00 01 B0",
		"00 00 40 5E B0 1C",
	} {
		b := unhex(f, s)
		f.Add(b[0], b, len(b))
	}
	f.Fuzz(func(t *testing.T, addr uint8, frame []byte, expect int) {
		p, err := Decode(addr, frame, expect)
		if err != nil {
			return
		}
		if !rtu.CheckCRC(frame) || frame[0] != addr {
			t.Fatalf("accepted bad frame % X", frame)
		}
		_, _ = DecodePhaseEnergy(p)
		_, _ = DecodeTotalEnergy(p)
		_, _ = DecodeGroupPower(p)
		_, _ = DecodeSerial(p)
		_, _ = DecodePassport(p)
		_, _ = DecodeVariant(p)
		if len(p) >= 3 {
			_ = DecodePower3(p)
		}
		if len(p) >= 4 {
			_, _ = DecodeEnergy4(p)
		}
	})
}
