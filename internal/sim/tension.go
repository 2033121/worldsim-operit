package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"worldsim/internal/engine"
	"worldsim/internal/llm"
)

// ---------- 张力引擎（§5：Scene/Summary/Skip 自适应粒度，省 token） ----------

// SetMode 手动设定模式：auto | scene | summary | skip
func (s *Simulator) SetMode(m string) {
	switch m {
	case "auto":
		s.modeAuto = true
		s.mode = "scene"
	case "scene", "summary", "skip":
		s.modeAuto = false
		s.mode = m
	}
	s.lowTensionDays = 0
}

// decideMode 根据张力自适应选择粒度（§5 表格）：
//   tension≥0.5 → Scene；0.3~0.5 → Summary；<0.3 连续3天 → Skip
func (s *Simulator) decideMode(ctx context.Context) {
	t := s.engine.State().WorldLevel.Tension
	old := s.mode
	switch {
	case t >= 0.5:
		s.mode = "scene"
		s.lowTensionDays = 0
	case t >= 0.3:
		s.mode = "summary"
		s.lowTensionDays = 0
	default: // t < 0.3
		s.lowTensionDays++
		if s.lowTensionDays >= 3 {
			s.mode = "skip"
		} else {
			s.mode = "summary" // <3天先 Summary（§5.2：Skip 最小长度阈值）
		}
	}
	if s.mode != old {
		s.chronicle = append(s.chronicle, ChronicleEntry{
			Day: s.day, Kind: "STATE", Time: now(),
			Content:    fmt.Sprintf("张力引擎：模式 %s → %s（张力 %.2f）", old, s.mode, t),
			Visibility: "public", Source: "系统",
		
			Weight: 0.15, Tags: []string{"张力"},})
	}
}

// ---------- Skip 快进（§5.2：沿主角默认策略批量推进，不侵犯自主性） ----------

// readProtagonistPlan 读取主角默认策略（entities.hero.extra.plan）
func (s *Simulator) readProtagonistPlan() *ProtagonistPlan {
	ent, ok := s.engine.State().Entities[s.heroName]
	if !ok {
		return nil
	}
	if raw, ok := ent.Extra["plan"].(map[string]any); ok {
		b, _ := json.Marshal(raw)
		var p ProtagonistPlan
		if json.Unmarshal(b, &p) == nil {
			return &p
		}
	}
	return nil
}

// runSkip 执行一个快进块（3~7天，1次LLM；无策略时降级 Summary）
func (s *Simulator) runSkip(ctx context.Context, res *DayResult) (*DayResult, error) {
	plan := s.readProtagonistPlan()
	if plan == nil || s.llm == nil {
		// 无策略：降级为 Summary 一天
		s.mode = "summary"
		res.Mode = "summary"
		return s.runSummaryDay(ctx, res)
	}

	n := 4 // 快进块天数（3-7 调整）
	if plan.Horizon > 0 && plan.Horizon < n {
		n = plan.Horizon
	}
	startDay := s.day
	endDay := startDay + n - 1

	// 世界 Agent 一次性生成快进摘要（1次 LLM）
	summary, changes, interrupted := s.skipSummaryLLM(ctx, plan, startDay, endDay)
	if summary == "" {
		// LLM 失败：dry-run 快进
		summary = fmt.Sprintf("Day%d-%d：风平浪静，%s按计划生活，无大事发生。", startDay, endDay, s.heroName)
	}

	// 沿策略推进状态（§5.2：只能按 default_routine 微调，不替主角做大决定）
	if len(changes) > 0 && !interrupted {
		prop := &engine.Proposal{
			CommandID:    s.nextCmd("skip"),
			ActorID:      "world_agent",
			BaseRevision: s.engine.State().Revision,
			Type:         "state_change",
			Changes:      changes,
			Reason:       fmt.Sprintf("Skip快进 %d-%d（按主角策略）", startDay, endDay),
		}
		if err := s.engine.Submit(ctx, prop); err == nil {
			res.Proposals = append(res.Proposals, *prop)
		}
	}

	// 推进天数（中断则只推进到中断点）
	advanceDays := n
	if interrupted {
		advanceDays = 1
	}
	s.day += advanceDays - 1
	// 世界侧照常演化：天气/张力微调
	s.engine.State().Day = s.day
	s.engine.State().WorldLevel.Tension += 0.05 // 快进期间世界仍暗中演进

	// 编年史摘要块（§9.9.4：必须交代变化）
	s.chronicle = append(s.chronicle, ChronicleEntry{
		Day: s.day, Kind: "FACT", Time: now(),
		Content:    summary,
		Visibility: "public", Source: "快进",
	
	Weight: 0.5, Tags: []string{"快进"},})
	res.Events = nil
	res.Chronicle = append(res.Chronicle, s.chronicle[len(s.chronicle)-1])

	// Skip 块结束检查（§5.4）：张力仍低 → 强制注入引导事件转 Scene
	if !interrupted && s.engine.State().WorldLevel.Tension < 0.3 {
		s.mode = "scene"
		res.PauseMsg = "快进结束，世界仍平淡——已注入引导事件（强制转 Scene）"
	}
	s.lowTensionDays = 0
	return res, nil
}

// skipSummaryLLM 世界 Agent 生成快进摘要 + 状态变化 + 中断判断（1次调用）
func (s *Simulator) skipSummaryLLM(ctx context.Context, plan *ProtagonistPlan, startDay, endDay int) (string, []engine.Change, bool) {
	ctx = llm.WithSpan(ctx, "快进摘要")
	planJSON, _ := json.Marshal(plan)
	stateJSON := compactState(s.engine.State()) // 精简版状态（省 token）
	system := `你是世界Agent。世界正在快进（Skip模式）：主角按"默认策略"生活，你不替主角做大决定。
规则：
1. 输出严格 JSON：{"summary":"DayX-Y 概况（60字内，交代必须的关键变化：天气/事件/势力动向）","state_changes":[{"path":"...","op":"add|set","value":<数值>}],"interrupt":false}
2. state_changes 只允许沿策略微调：主角金钱按日常作息小幅收支、地点按 routine 轮转；不改变主角的重大人生轨迹
3. interrupt=true 仅在"命中中断条件"时（主角遇到策略外的大事/危险），命中则 summary 只写到中断前
4. 主角默认策略：` + string(planJSON)
	user := fmt.Sprintf("世界状态：\n%s\n请生成 Day%d-%d 的快进摘要。", stateJSON, startDay, endDay)
	raw, err := s.llm.Complete(ctx, system, user)
	if err != nil {
		return "", nil, false
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return "", nil, false
	}
	var resp struct {
		Summary      string          `json:"summary"`
		StateChanges []engine.Change `json:"state_changes"`
		Interrupt    bool            `json:"interrupt"`
	}
	if json.Unmarshal([]byte(jsonStr), &resp) != nil {
		return "", nil, false
	}
	return strings.TrimSpace(resp.Summary), resp.StateChanges, resp.Interrupt
}

// runSummaryDay Summary 轻模拟一天（事件用轻事件、主角走简化决策，省 2-3 次 LLM）
func (s *Simulator) runSummaryDay(ctx context.Context, res *DayResult) (*DayResult, error) {
	// 轻事件（dry-run 池：1 个低severity事件，不调 LLM）
	s.events = s.dryRunEvents()
	if len(s.events) > 1 {
		s.events = s.events[:1]
	}
	// 主角简化决策：dry-run（不调 LLM）
	action := s.protagonistAct(s.events)
	// 世界推进照常（世界 Agent 或 dry-run）
	var advance *engine.Proposal
	if s.llm != nil {
		if a, err := WorldAdvanceLLM(ctx, s.llm, s.engine.State(), s.events, engine.Rules{}, s.wb, s.OpenForeshadows(), s.currentArc); err == nil {
			advance = a
		}
	}
	if advance == nil {
		advance = &engine.Proposal{
			CommandID: s.nextCmd("world"), ActorID: "world_agent",
			BaseRevision: s.engine.State().Revision, Type: "state_change",
			Changes: s.worldAdvanceChanges(), Reason: "世界推进（Summary）",
		}
	}
	if err := s.engine.Submit(ctx, advance); err == nil {
		res.Proposals = append(res.Proposals, *advance)
	}
	if action != nil {
		if err := s.engine.Submit(ctx, action); err == nil {
			res.Proposals = append(res.Proposals, *action)
		}
	}
	// 记录
	s.recordChronicle(s.buildObservation(s.events), res.Proposals, s.events, res)
	s.saveChronicle()
	return res, nil
}