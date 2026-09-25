// Package rtu — низкий уровень обмена RTU-кадрами через прозрачный мост WB-MGE:
// CRC16/Modbus и транзакция «запрос-ответ» поверх одного TCP-соединения.
// Пакет не знает протоколов устройств: границы ответа задаёт вызывающий.
package rtu

// crcTable — CRC16/Modbus: init 0xFFFF, отражённый полином 0xA001.
var crcTable = func() (t [256]uint16) {
	for i := range t {
		c := uint16(i)
		for range 8 {
			if c&1 != 0 {
				c = c>>1 ^ 0xA001
			} else {
				c >>= 1
			}
		}
		t[i] = c
	}
	return t
}()

// CRC16 — контрольная сумма Modbus RTU.
func CRC16(b []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, v := range b {
		crc = crc>>8 ^ crcTable[byte(crc)^v]
	}
	return crc
}

// AppendCRC дописывает CRC к кадру, младший байт первым.
func AppendCRC(b []byte) []byte {
	crc := CRC16(b)
	return append(b, byte(crc), byte(crc>>8))
}

// CheckCRC проверяет два последних байта кадра. Кадр короче 3 байт неверен.
func CheckCRC(frame []byte) bool {
	n := len(frame)
	if n < 3 {
		return false
	}
	crc := CRC16(frame[:n-2])
	return frame[n-2] == byte(crc) && frame[n-1] == byte(crc>>8)
}
