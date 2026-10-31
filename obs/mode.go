package obs

import (
	"fmt"
	"strings"
)

// Mode 是报告的判定语义：snapshot 回答「现在怎样」，acceptance 回答「这轮通过没」。
type Mode string

const (
	ModeSnapshot   Mode = "snapshot"
	ModeAcceptance Mode = "acceptance"
)

// Trigger 是报告的生成方式；auto 在所有已知触发方式下使用运行快照。
type Trigger string

const (
	TriggerPeriodic Trigger = "periodic" // obs.report.interval 定时
	TriggerExit     Trigger = "exit"     // obs.report.on_exit 退出
	TriggerCLI      Trigger = "cli"      // papa report generate
)

// ResolveMode 把配置值与触发方式解析成生效模式。
// configured: "auto"/空 = 运行快照；"snapshot"/"acceptance" = 强制。
func ResolveMode(configured string, trigger Trigger) (Mode, error) {
	mode := Mode(strings.ToLower(strings.TrimSpace(configured)))
	switch mode {
	case "", "auto":
		switch trigger {
		case TriggerPeriodic, TriggerExit, TriggerCLI:
			return ModeSnapshot, nil
		default:
			return "", fmt.Errorf("obs.report.mode: 未知触发方式 %q", trigger)
		}
	case ModeSnapshot, ModeAcceptance:
		return mode, nil
	default:
		return "", fmt.Errorf("obs.report.mode: 未知模式 %q（可选 auto/snapshot/acceptance）", configured)
	}
}
