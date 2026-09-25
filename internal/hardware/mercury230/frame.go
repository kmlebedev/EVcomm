// Package mercury230 — снятие показаний «Меркурий 230 ART» по протоколу
// «Меркурий» через прозрачный мост WB-MGE (RTU-over-TCP).
//
// Кодек и декодеры — чистые функции без ввода-вывода. Meter — сессия уровня
// протокола поверх rtu.Conn, Adapter — goroutine поста. Пакет формирует только
// команды чтения: перепрограммировать счётчик он не может даже с паролем уровня 2.
package mercury230

import (
	"fmt"

	"github.com/kmlebedev/EVcomm/internal/rtu"
)

// Коды запросов. Других кодов (запись 03h, 07h и т. д.) в пакете нет.
const (
	CmdTestLink     byte = 0x00
	CmdOpenChannel  byte = 0x01
	CmdCloseChannel byte = 0x02
	CmdReadArray    byte = 0x05
	CmdReadParam    byte = 0x08
)

// AddrBroadcast — широковещательный адрес: счётчик не отвечает, модуль его не использует.
const AddrBroadcast = 0xFE

// StatusLen — длина ответа-состояния.
const StatusLen = rtu.StatusLen

func allowed(code byte) bool {
	switch code {
	case CmdTestLink, CmdOpenChannel, CmdCloseChannel, CmdReadArray, CmdReadParam:
		return true
	}
	return false
}

// Encode собирает кадр запроса: адрес, PDU (код и параметры), CRC.
// Коды, кроме чтения и управления каналом, отклоняются.
func Encode(addr uint8, pdu []byte) ([]byte, error) {
	if len(pdu) == 0 {
		return nil, fmt.Errorf("mercury230: empty pdu")
	}
	if !allowed(pdu[0]) {
		return nil, fmt.Errorf("%w: %02Xh", ErrForbiddenCommand, pdu[0])
	}
	if addr == AddrBroadcast {
		return nil, fmt.Errorf("mercury230: broadcast address %02Xh gets no response", addr)
	}
	f := make([]byte, 0, len(pdu)+3)
	f = append(f, addr)
	f = append(f, pdu...)
	return rtu.AppendCRC(f), nil
}

// Decode проверяет кадр ответа и возвращает поле данных (без адреса и CRC).
// Для запросов с ответом-состоянием (expectLen == StatusLen) «норма» даёт
// пустое поле данных. Ненулевой код состояния — *StatusError.
func Decode(addr uint8, frame []byte, expectLen int) ([]byte, error) {
	if len(frame) < StatusLen {
		return nil, fmt.Errorf("%w: %d bytes", ErrLength, len(frame))
	}
	if !rtu.CheckCRC(frame) {
		return nil, ErrCRC
	}
	if frame[0] != addr {
		return nil, fmt.Errorf("%w: got %02Xh, want %02Xh", ErrAddress, frame[0], addr)
	}
	if len(frame) == StatusLen {
		if code := frame[1] & 0x0F; code != StatusOK {
			return nil, &StatusError{Code: code, Raw: frame[1]}
		}
		if expectLen != StatusLen {
			return nil, ErrUnexpectedStatus
		}
		return frame[1:2], nil
	}
	if len(frame) != expectLen {
		return nil, fmt.Errorf("%w: %d bytes, want %d", ErrLength, len(frame), expectLen)
	}
	return frame[1 : len(frame)-2], nil
}
