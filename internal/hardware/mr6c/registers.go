// Package mr6c — адаптер WB-MR6C v.3 через WB-MGE v.3 (Modbus TCP, порт 502):
// карта регистров, опрос, запись выходов и эталон настроек безопасного режима.
//
// Карта регистров взята из шаблона wb-mqtt-serial templates/config-wb-mr6cv3.json.jinja
// (адреса с 0).
package mr6c

const (
	NumOutputs = 6 // K1…K6
	NumInputs  = 7 // вход 0…6

	// Coils.
	CoilOutputBase uint16 = 0 // K1…K6, FC01/FC05. Команды Off/On/Toggle (100+/108+/116+) не используем.

	// Discrete inputs.
	DiscreteInputBase     uint16 = 0  // вход 1…6 → 0…5
	DiscreteInput0        uint16 = 7  // вход 0
	DiscreteRealStateBase uint16 = 96 // фактическое состояние K1…K6, прошивка ≥ 1.24.0

	// Holding registers.
	RegLegacyInputMode    uint16 = 5   // 0 — устаревшее управление режимами входов выключено (setup шаблона)
	RegPowerOnState       uint16 = 6   // 0 — безопасное состояние, 1 — восстановить, 2 — по входам
	RegPollTimeout        uint16 = 8   // таймаут опроса, с (1…65534)
	RegInputModeBase      uint16 = 9   // режим входа 1…6 → 9…14
	RegInput0Mode         uint16 = 16  // режим входа 0
	RegSafetySource       uint16 = 19  // 0 — таймаут или питание шины, 1 — питание шины, 2 — таймаут
	RegSafeStateBase      uint16 = 930 // безопасное состояние K1…K6: 0 — Off
	RegSafetyBehaviorBase uint16 = 938 // 1 — перевести выход в безопасное состояние
	RegSafetyInputCtlBase uint16 = 946 // 1 — запретить управление от входов в безопасном режиме
	RegFirmwareVersion    uint16 = 250 // строка, по символу на регистр
	FirmwareVersionLen    uint16 = 16
)

// Значения регистров эталона.
const (
	ValuePowerOnSafeState   uint16 = 0
	ValueSafeStateOff       uint16 = 0
	ValueSafetyBehaviorSafe uint16 = 1
	ValueSafetyInputDisable uint16 = 1
	// ValueInputModeNoControl — «управление отключено / измерение частоты»:
	// вход не управляет выходами, НЗ «отпущенного» контактора не может включить катушку.
	ValueInputModeNoControl uint16 = 3
)

// inputModeReg возвращает регистр режима входа n (0…6).
func inputModeReg(n int) uint16 {
	if n == 0 {
		return RegInput0Mode
	}
	return RegInputModeBase + uint16(n-1)
}
