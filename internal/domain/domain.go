// Package domain содержит типы предметной области: посты, линии, команды,
// наблюдения и состояния. Пакет не зависит от Modbus и MQTT.
package domain

import "time"

type (
	PostID string
	LineID string
)

// Линии поста: K1→L1, K2→L2, K3→L3 (однофазные), K4→3P (трёхфазная, опционально).
const (
	LineL1 LineID = "L1"
	LineL2 LineID = "L2"
	LineL3 LineID = "L3"
	Line3P LineID = "3P"
)

// Mode — режим поста. Одновременная работа групп запрещена (до 64 А на фазу при счётчике 60 А).
type Mode string

const (
	Mode3x1 Mode = "3x1" // K1–K3
	Mode1x3 Mode = "1x3" // K4
)

func (m Mode) Valid() bool { return m == Mode3x1 || m == Mode1x3 }

// Group — группа линий, взаимоисключающая с другой группой.
type Group string

const (
	GroupSingle Group = "single" // L1, L2, L3
	GroupThree  Group = "three"  // 3P
)

// GroupFor возвращает группу, которую разрешает режим.
func (m Mode) Group() Group {
	if m == Mode1x3 {
		return GroupThree
	}
	return GroupSingle
}

// LineState — состояние линии. ON означает подтверждённое разрешение питания, а не «идёт зарядка».
type LineState string

const (
	LineUnknown   LineState = "UNKNOWN"
	LineOff       LineState = "OFF"
	LineEnabling  LineState = "ENABLING"
	LineOn        LineState = "ON"
	LineDisabling LineState = "DISABLING"
	LineFault     LineState = "FAULT"
	LineLocked    LineState = "LOCKED"
)

// Quality — качество наблюдения.
type Quality string

const (
	QualityFresh   Quality = "fresh"
	QualityStale   Quality = "stale"
	QualityUnknown Quality = "unknown"
	QualityError   Quality = "error"
	// QualityDisputed — значение получено, но не прошло проверку достоверности
	// (монотонность, правдоподобие прироста, сверка с суммой, серийный номер).
	QualityDisputed Quality = "disputed"
)

// Observation — значение с источником, временем наблюдения и качеством.
type Observation[T any] struct {
	Value      T         `json:"value"`
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
	Quality    Quality   `json:"quality"`
}

// CommandStatus — состояние команды, хранится отдельно от состояния линии.
type CommandStatus string

const (
	CommandAccepted   CommandStatus = "accepted"
	CommandInProgress CommandStatus = "in_progress"
	CommandConfirmed  CommandStatus = "confirmed"
	CommandFailed     CommandStatus = "failed"
	CommandUnknown    CommandStatus = "unknown"
)

func (s CommandStatus) Terminal() bool {
	return s == CommandConfirmed || s == CommandFailed || s == CommandUnknown
}

// Command — идемпотентное «установить ON/OFF». Повтор с тем же ID не создаёт нового переключения.
type Command struct {
	ID        string    `json:"command_id"`
	Line      LineID    `json:"line"`
	On        bool      `json:"on"`
	ExpiresAt time.Time `json:"expires_at"` // нулевое значение — без срока
}

// CommandTimings — задержки от начала записи в MR6C.
type CommandTimings struct {
	WriteAck  time.Duration `json:"write_ack"`  // ответ Modbus на FC05
	RealState time.Duration `json:"real_state"` // выход и фактическое состояние совпали с командой
	Feedback  time.Duration `json:"feedback"`   // НЗ-вход подтвердил положение контактора
}

type CommandResult struct {
	Command    Command        `json:"command"`
	Status     CommandStatus  `json:"status"`
	Reason     string         `json:"reason,omitempty"`
	AcceptedAt time.Time      `json:"accepted_at"`
	FinishedAt time.Time      `json:"finished_at"`
	Timings    CommandTimings `json:"timings"`
}
