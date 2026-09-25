package mercury230

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEncodeRequests(t *testing.T) {
	pw1 := [6]byte{1, 1, 1, 1, 1, 1}
	// Раздел 4.2 архитектуры (адрес 91h) и тесты wb-mqtt-serial (адрес 0).
	for _, tc := range []struct {
		addr uint8
		req  Request
		want string
		resp int
	}{
		{0x91, TestLink(), "91 00 6C 20", 4},
		{0x91, OpenChannel(1, pw1), "91 01 01 01 01 01 01 01 01 D6 17", 4},
		{0x91, CloseChannel(), "91 02 ED E1", 4},
		{0x91, ReadSerial(), "91 08 00 27 ED", 10},
		{0x91, ReadPassport(), "91 08 01 00 AC 8A", 27},
		{0x91, ReadVariant(), "91 08 12 A7 E0", 9},
		{0x91, ReadTotalEnergy(), "91 05 00 00 3C D9", 19},
		{0x91, ReadPhaseEnergy(), "91 05 60 00 14 D9", 15},
		{0x91, ReadActivePower(0x00), "91 08 16 00 A3 7A", 15},
		{0x91, ReadActivePowerPhase(1), "91 08 11 01 60 8A", 6},
		{0x91, ReadActivePowerPhase(2), "91 08 11 02 20 8B", 6},
		{0x91, ReadActivePowerPhase(3), "91 08 11 03 E1 4B", 6},
		{0x00, OpenChannel(1, pw1), "00 01 01 01 01 01 01 01 01 77 81", 4},
		{0x00, OpenChannel(2, [6]byte{0x12, 0x13, 0x14, 0x15, 0x16, 0x17}), "00 01 02 12 13 14 15 16 17 34 17", 4},
		{0x00, ReadTotalEnergy(), "00 05 00 00 10 25", 19},
		{0x00, ReadPhaseEnergy(), "00 05 60 00 38 25", 15},
		{0x00, ReadActivePowerPhase(1), "00 08 11 01 4C 76", 6},
	} {
		got, err := Encode(tc.addr, tc.req.PDU)
		if err != nil {
			t.Fatalf("%s: %v", tc.req.Name, err)
		}
		if want := unhex(t, tc.want); !bytes.Equal(got, want) {
			t.Errorf("%s: % X, want %s", tc.req.Name, got, tc.want)
		}
		if tc.req.RespLen != tc.resp {
			t.Errorf("%s: RespLen %d, want %d", tc.req.Name, tc.req.RespLen, tc.resp)
		}
	}
}

func TestEncodeReadOnlyInvariant(t *testing.T) {
	for _, code := range []byte{0x03, 0x04, 0x06, 0x07, 0x0A, 0xFF} {
		if _, err := Encode(0x91, []byte{code, 0x00}); !errors.Is(err, ErrForbiddenCommand) {
			t.Errorf("code %02X: err = %v, want ErrForbiddenCommand", code, err)
		}
	}
	if _, err := Encode(AddrBroadcast, []byte{CmdTestLink}); err == nil {
		t.Error("broadcast address accepted")
	}
	if _, err := Encode(0x91, nil); err == nil {
		t.Error("empty pdu accepted")
	}
}

func TestDecodeFrames(t *testing.T) {
	// Пофазная энергия из тестов wb-mqtt-serial (адрес 0).
	p, err := Decode(0, unhex(t, "00 30 00 29 C5 30 00 29 00 04 00 9D 95 50 99"), 15)
	if err != nil {
		t.Fatal(err)
	}
	e, err := DecodePhaseEnergy(p)
	if err != nil || e != (PhaseEnergy{3196201, 3145769, 300445}) {
		t.Fatalf("phase energy = %v, %v", e, err)
	}

	// Энергия от сброса: A− и R− замаскированы у однонаправленного исполнения.
	p, err = Decode(0, unhex(t, "00 30 00 28 C5 FF FF FF FF 04 00 9C 95 FF FF FF FF 44 AB"), 19)
	if err != nil {
		t.Fatal(err)
	}
	te, err := DecodeTotalEnergy(p)
	if err != nil || te.APlus != 3196200 || te.RPlus != 300444 || te.Masked != [4]bool{false, true, false, true} {
		t.Fatalf("total energy = %+v, %v", te, err)
	}

	// Синтетический ответ 08 16 из раздела 4.4: P = L1 = 7360,00 Вт, L2 = 0, L3 = −12,34 Вт.
	p, err = Decode(0x91, unhex(t, "91 0B 00 3B 0B 00 3B 00 00 00 80 D2 04 F1 82"), 15)
	if err != nil {
		t.Fatal(err)
	}
	pw, err := DecodeGroupPower(p)
	if err != nil || pw != (Power{TotalCW: 736000, PhaseCW: [3]int32{736000, 0, -1234}}) {
		t.Fatalf("power = %+v, %v", pw, err)
	}

	// Мгновенные из wb-mqtt-serial: U1 = 241,28 В, P > 0 при бите реактивной мощности, P1 < 0.
	for _, tc := range []struct {
		frame string
		want  int32
	}{
		{"00 00 40 5E B0 1C", 24128},
		{"00 48 87 70 E2 26", 553095},
		{"00 C8 87 70 E3 CE", -553095},
	} {
		p, err := Decode(0, unhex(t, tc.frame), 6)
		if err != nil {
			t.Fatalf("%s: %v", tc.frame, err)
		}
		if got := DecodePower3(p); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.frame, got, tc.want)
		}
	}
}

func TestDecodeStatusAndErrors(t *testing.T) {
	// Ответ на открытие канала — «норма».
	if p, err := Decode(0, unhex(t, "00 00 01 B0"), StatusLen); err != nil || len(p) != 1 {
		t.Fatalf("ok status: % X, %v", p, err)
	}
	// Коды 5 и 2 из тестов wb-mqtt-serial.
	for frame, code := range map[string]uint8{"00 05 C1 B3": StatusChannelClosed, "00 02 80 71": StatusInternal} {
		_, err := Decode(0, unhex(t, frame), 6)
		if !IsStatus(err, code) || Classify(err) != KindStatus {
			t.Errorf("%s: err = %v, want status %d", frame, err, code)
		}
	}
	for name, tc := range map[string]struct {
		frame  string
		expect int
		want   error
	}{
		"bad crc":            {"00 30 00 29 C5 30 00 29 00 04 00 9D 95 50 98", 15, ErrCRC},
		"foreign address":    {"00 00 01 B0", StatusLen, ErrAddress},
		"short":              {"00 00", StatusLen, ErrLength},
		"length":             {"00 00 40 5E B0 1C", 15, ErrLength},
		"ok instead of data": {"00 00 01 B0", 15, ErrUnexpectedStatus},
	} {
		addr := uint8(0)
		if name == "foreign address" {
			addr = 0x91
		}
		if _, err := Decode(addr, unhex(t, tc.frame), tc.expect); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
		if Classify(tc.want) != KindLink {
			t.Errorf("%s: class %s, want link", name, Classify(tc.want))
		}
	}
}

func TestMaskedPhaseEnergy(t *testing.T) {
	p := unhex(t, "FF FF FF FF 30 00 29 00 04 00 9D 95")
	if _, err := DecodePhaseEnergy(p); !errors.Is(err, ErrMasked) || Classify(err) != KindMasked {
		t.Fatalf("err = %v, want ErrMasked", err)
	}
}

func TestPasswordMasked(t *testing.T) {
	r := OpenChannel(2, [6]byte{0x32, 0x32, 0x32, 0x32, 0x32, 0x32})
	f, _ := Encode(0x91, r.PDU)
	m := r.Mask(f)
	if bytes.Contains(m, []byte{0x32, 0x32}) || m[2] != 2 || !bytes.Equal(m[9:], f[9:]) {
		t.Fatalf("masked = % X", m)
	}
	if f[3] != 0x32 {
		t.Fatal("Mask modified the original frame")
	}
	if got := ReadSerial().Mask([]byte{1, 2, 3}); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatal("non-secret request masked")
	}
}

func TestParsePassword(t *testing.T) {
	pw, err := ParsePassword("313131313131")
	if err != nil || pw != [6]byte{0x31, 0x31, 0x31, 0x31, 0x31, 0x31} {
		t.Fatalf("pw = % X, %v", pw, err)
	}
	for _, bad := range []string{"", "01010101010", "0101010101zz", "01010101010101"} {
		if _, err := ParsePassword(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDecodeIdentity(t *testing.T) {
	id, err := DecodeSerial([]byte{32, 87, 47, 66, 14, 3, 24})
	if err != nil || id.Serial != "32874766" || !id.Manufactured.Equal(time.Date(2024, 3, 14, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("serial = %+v, %v", id, err)
	}
	if id, _ := DecodeSerial([]byte{1, 2, 3, 4, 31, 2, 24}); !id.Manufactured.IsZero() {
		t.Fatal("31 February accepted")
	}
	p := append([]byte{32, 87, 47, 66, 14, 3, 24, 9, 0, 1}, 0xB4, 0xE3, 0x97, 0x04, 0x01, 0x00)
	p = append(p, make([]byte, 8)...)
	id, err = DecodePassport(p)
	if err != nil || id.Serial != "32874766" || id.Firmware != "9.0.1" || !id.HasVariant {
		t.Fatalf("passport = %+v, %v", id, err)
	}
	if !id.Variant.PhaseEnergy() || id.Variant.Interface() != 1 || !id.Variant.SumModeBit() {
		t.Fatalf("variant bits: % X", id.Variant[:])
	}
	if _, err := DecodePassport(p[:20]); !errors.Is(err, ErrLength) {
		t.Fatal("short passport accepted")
	}
}
