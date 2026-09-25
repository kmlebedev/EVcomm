// Package policy — правила допуска включения: режимы 3x1/1x3, программная
// блокировка групп и бюджет фаз поста.
package policy

import (
	"fmt"

	"github.com/kmlebedev/EVcomm/internal/domain"
	"github.com/kmlebedev/EVcomm/internal/registry"
)

// LineView — то, что политике нужно знать о линии.
type LineView struct {
	Line  registry.Line
	State domain.LineState
}

// CheckEnable проверяет, можно ли включить линию target на посту.
func CheckEnable(post registry.Post, mode domain.Mode, target registry.Line, lines []LineView) error {
	if target.Group != mode.Group() {
		return fmt.Errorf("line %s is not allowed in mode %s", target.ID, mode)
	}
	reserved := map[string]float64{}
	for _, lv := range lines {
		if lv.Line.ID == target.ID {
			continue
		}
		// Линии другой группы должны быть подтверждённо OFF, а не «вероятно выключены».
		if lv.Line.Group != target.Group && lv.State != domain.LineOff {
			return fmt.Errorf("conflicting line %s is %s, must be confirmed OFF", lv.Line.ID, lv.State)
		}
		// Бюджет резервируется по максимальному току любой линии, кроме подтверждённо выключенной.
		if lv.State != domain.LineOff {
			for _, ph := range lv.Line.Phases {
				reserved[ph] += post.Limits.EVSEMaxA
			}
		}
	}
	for _, ph := range target.Phases {
		if need := reserved[ph] + post.Limits.EVSEMaxA; need > post.Limits.PhaseMaxA {
			return fmt.Errorf("phase %s budget exceeded: %.0f A > %.0f A", ph, need, post.Limits.PhaseMaxA)
		}
	}
	return nil
}

// CheckModeSwitch: смена режима допускается только после подтверждённого OFF всех линий поста.
func CheckModeSwitch(lines []LineView) error {
	for _, lv := range lines {
		if lv.State != domain.LineOff {
			return fmt.Errorf("line %s is %s, all lines must be confirmed OFF", lv.Line.ID, lv.State)
		}
	}
	return nil
}
