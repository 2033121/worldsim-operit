package narrative

import (
	"context"
	"fmt"
)

// ---------- 剧情引擎主入口 ----------

// Engine 剧情引擎：规划全书（分章→节拍→剧本）。
type Engine struct {
	Input *Input
}

// NewEngine 创建剧情引擎（输入已组装）。
func NewEngine(in *Input) *Engine {
	return &Engine{Input: in}
}

// Run 执行完整规划：分章 → 每章节拍 → 每章剧本。
// 返回完整 Plan（章节方案 + 每章剧本）。任何一步失败都降级（规则兜底），不中断。
func (e *Engine) Run(ctx context.Context) (*Plan, error) {
	in := e.Input
	units, err := PlanUnits(ctx, in)
	if err != nil {
		// 分章失败已有兜底，继续
		units = fallbackUnits(in)
	}
	if len(units) == 0 {
		return &Plan{Direction: in.Direction}, nil
	}
	plan := &Plan{Direction: in.Direction, Units: units}
	for i := range units {
		u := &units[i]
		// 每章：节拍 → 剧本
		beats, berr := PlanBeats(ctx, in, *u)
		if berr != nil {
			beats = fallbackBeats(in, *u)
		}
		scripts, serr := PlanScripts(ctx, in, *u, beats)
		if serr != nil {
			scripts = fallbackScripts(in, *u, beats)
		}
		plan.Scripts = append(plan.Scripts, scripts)
	}
	return plan, nil
}

// RunPlanOnly 只做分章规划（不细化剧本）——供 /api/world/novel/plan 预览。
func (e *Engine) RunPlanOnly(ctx context.Context) (*Plan, error) {
	in := e.Input
	units, err := PlanUnits(ctx, in)
	if err != nil {
		units = fallbackUnits(in)
	}
	if len(units) == 0 {
		return &Plan{Direction: in.Direction}, nil
	}
	return &Plan{Direction: in.Direction, Units: units}, nil
}

// PreviewText 分章方案的人类可读预览（API 返回用）。
func PreviewText(p *Plan) string {
	var s string
	for _, u := range p.Units {
		s += fmt.Sprintf("第%d章 %s（Day%d～Day%d", u.Num, u.Title, u.DayStart, u.DayEnd)
		if u.ArcRef > 0 {
			s += fmt.Sprintf("，段落#%d", u.ArcRef)
		}
		s += "）"
		if u.Goal != "" {
			s += " 目标：" + u.Goal
		}
		if u.Payoff != "" {
			s += " 爽点：" + u.Payoff
		}
		if u.Hook != "" {
			s += " 钩子：" + u.Hook
		}
		s += "\n"
	}
	return s
}
