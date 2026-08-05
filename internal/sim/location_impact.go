package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"worldsim/internal/engine"
	"worldsim/internal/llm"
)

// ---------- P0-2 地点系统：会变化的世界 ----------

// SeedLocations 初始化基础地点（从世界书/默认池），并为每个地点生成"感官档案"（按本世界规则）
func (s *Simulator) SeedLocations() []engine.Change {
	st := s.engine.State()
	if len(st.WorldLevel.Locations) > 0 {
		return nil
	}
	// 通用兜底地点池（任何世界都能用，不含都市专属设定；正常流程由 WorldInitPlan 按世界书生成地点）
	defaults := []struct{ name, typ, note string }{
		{"集镇", "城区", "主角生活圈的集镇，人来人往，消息灵通"},
		{"主街", "交通", "日常往返的必经之路，偶有怪事传闻"},
		{"常去的老店", "建筑", "老熟人的据点，消息的集散地"},
		{"郊野", "自然", "镇子外围，人少，气氛微妙"},
	}
	var changes []engine.Change
	for _, d := range defaults {
		changes = append(changes,
			engine.Change{Path: "world_level.locations." + d.name + ".type", Op: "set", Value: d.typ},
			engine.Change{Path: "world_level.locations." + d.name + ".state", Op: "set", Value: "正常"},
			engine.Change{Path: "world_level.locations." + d.name + ".note", Op: "set", Value: d.note},
		)
	}
	// 感官档案：按本世界规则生成（贴合该世界的环境质感）
	if s.llm != nil && s.wb != nil {
		if senses := s.seedLocationSenses(defaults); len(senses) > 0 {
			for name, sn := range senses {
				changes = append(changes, engine.Change{Path: "world_level.locations." + name + ".senses", Op: "set", Value: sn})
			}
		}
	}
	return changes
}

// EnsureLocationSenses 为缺少感官档案的地点补生成（旧世界升级/新地点登记后用，按本世界规则）
func (s *Simulator) EnsureLocationSenses() []engine.Change {
	st := s.engine.State()
	if len(st.WorldLevel.Locations) == 0 || s.llm == nil || s.wb == nil {
		return nil
	}
	var missing []struct{ name, typ, note string }
	for n, l := range st.WorldLevel.Locations {
		if l.Senses == "" {
			missing = append(missing, struct{ name, typ, note string }{n, l.Type, l.Note})
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if senses := s.seedLocationSenses(missing); len(senses) > 0 {
		var changes []engine.Change
		for name, sn := range senses {
			if sn != "" {
				changes = append(changes, engine.Change{Path: "world_level.locations." + name + ".senses", Op: "set", Value: sn})
			}
		}
		return changes
	}
	return nil
}

// seedLocationSenses 一次 LLM 调用为所有基础地点生成感官档案
func (s *Simulator) seedLocationSenses(defaults []struct{ name, typ, note string }) map[string]string {
	ctx := llm.WithSpan(context.Background(), "地点感官")
	var locList strings.Builder
	for _, d := range defaults {
		locList.WriteString(fmt.Sprintf("- %s【%s】%s\n", d.name, d.typ, d.note))
	}
	system := "你是场景感官设计师。为下面这些地点各写一段'感官档案'（100字内）：必须覆盖五维感官——视觉（看到什么，含反常细节）、听觉（环境声）、触觉（温度/质感/风）、嗅觉（气味，气味即情绪）、第六感（氛围直觉）。必须贴合本世界的时代与设定，具体可感，禁止空泛（如'环境舒适'）。输出严格 JSON：{\"地点名\":\"感官描述\"}"
	user := fmt.Sprintf("世界背景：%s\n地点列表：\n%s", s.wb.ForWorldBrief(), locList.String())
	raw, err := s.llm.CompleteTier(ctx, "fast", system, user)
	if err != nil {
		return nil
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return nil
	}
	var senses map[string]string
	if json.Unmarshal([]byte(jsonStr), &senses) != nil {
		return nil
	}
	return senses
}

// ApplyLocationChanges 提交地点变更提案
func (s *Simulator) ApplyLocationChanges(ctx context.Context, changes []engine.Change) *engine.Proposal {
	if len(changes) == 0 {
		return nil
	}
	prop := &engine.Proposal{
		CommandID:    s.nextCmd("loc"),
		ActorID:      "world_agent",
		BaseRevision: s.engine.State().Revision,
		Type:         "state_change",
		Changes:      changes,
		Reason:       "地点演化",
	}
	if err := s.engine.Submit(ctx, prop); err == nil {
		return prop
	}
	return nil
}

// ---------- P0-3 世界影响反馈（蝴蝶效应）：主角行动 → 世界变化 ----------

// WorldImpactLLM 评估主角行动对世界的影响（1次 fast 调用）
func (s *Simulator) WorldImpactLLM(ctx context.Context, heroAction string) ([]engine.Change, string) {
	ctx = llm.WithSpan(ctx, "世界影响")
	st := s.engine.State()
	stateJSON := compactState(st) // 精简版状态（省 token：不传 extra 大档案）
	system := `你是世界反应引擎。主角刚刚做了行动，评估它对世界的影响（蝴蝶效应）。
规则：
1. 输出严格 JSON：{"changes":[{"path":"world_level.global_events","op":"add","value":"..."},{"path":"world_level.locations.<地点>.state","op":"set","value":"..."},{"path":"world_level.factions.<势力>.power","op":"set","value":<0~1数值>}],"impact":"一句话总结影响（20字内）"}
2. 允许路径：world_level.global_events(追加)、world_level.locations.{地点}.{state|note}、world_level.factions.{势力}.{stance|power}、world_level.tension
3. 影响要克制而真实：小行动有小涟漪，大行动才改势力格局；主角目前还是小人物，不会一夜改变世界
4. 变化要能体现在后续事件里（封禁的地点、增强的势力、新的传闻）`
	user := fmt.Sprintf("世界状态：\n%s\n主角行动：%s", stateJSON, heroAction)
	raw, err := s.llm.CompleteTier(ctx, "fast", system, user)
	if err != nil {
		return nil, ""
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return nil, ""
	}
	var resp struct {
		Changes []engine.Change `json:"changes"`
		Impact  string          `json:"impact"`
	}
	if json.Unmarshal([]byte(jsonStr), &resp) != nil {
		return nil, ""
	}
	return resp.Changes, strings.TrimSpace(resp.Impact)
}

// ---------- P1 时间轴：天 → 年月（时间跨度"几年"） ----------

// DateLabel 把模拟日换算成年月标签（30天=1月，12月=1年）
func DateLabel(day int) string {
	if day <= 0 {
		return "第0天"
	}
	// 年份从1年起（第1年1月 = day 1-30）
	month := (day-1)/30 + 1
	year := (month-1)/12 + 1
	m := (month-1)%12 + 1
	return fmt.Sprintf("第%d年%d月（Day%d）", year, m, day)
}

// YearOf 当前是第几年（用于跨年Skip显示）
func (s *Simulator) YearOf() int { return (s.day-1)/360 + 1 }

// ---------- P1 遭遇链：事件种子 → 未来事件 ----------

// pendingEvents 是未来几天的"遭遇链种子"（事件A触发事件B）
func (s *Simulator) consumePendingEvents(day int) []EventCard {
	if s.pending == nil {
		return nil
	}
	var out []EventCard
	keep := map[int][]EventCard{}
	for d, evs := range s.pending {
		if d <= day {
			for _, ev := range evs {
				if ev.Type == "" {
					ev.Type = "daily" // 种子事件兜底类型
				}
				out = append(out, ev)
			}
		} else {
			keep[d] = evs
		}
	}
	s.pending = keep
	return out
}

func (s *Simulator) queuePendingEvents(day int, evs []EventCard) {
	if s.pending == nil {
		s.pending = map[int][]EventCard{}
	}
	s.pending[day] = append(s.pending[day], evs...)
}

// ---------- P1 伏笔账本（NeuroBook 理念：埋下/推进/酝酿/回收） ----------

type Foreshadow struct {
	Name      string  `json:"name"`
	Planted   int     `json:"planted"`   // 埋设日
	Status    string  `json:"status"`    // planted | progressing | resolved | abandoned
	Progress  string  `json:"progress"`  // 当前进展（每次推进更新）
	Resolved  int     `json:"resolved"`  // 回收日
	Maturity  float64 `json:"maturity"`  // 酝酿度 0~1：铺垫期慢慢攒，到阈值自然爆发成戏剧事件
}

// AdvanceForeshadowMaturity 酝酿度推进：铺垫 Agent 的"伏笔滋长"类铺垫调用
// 酝酿度每推进一步 +0.1~0.2，到 0.8 以上视为"即将爆发"（事件 Agent 会收到提示）
func (s *Simulator) AdvanceForeshadowMaturity(name string, amount float64) {
	if s.foreshadows == nil {
		return
	}
	f, ok := s.foreshadows[name]
	if !ok {
		// 铺垫 Agent 提到的"酝酿中线索"可能还没正式登记：自动登记
		s.RegisterForeshadow(name)
		f, _ = s.foreshadows[name]
	}
	if f.Status == "resolved" || f.Status == "abandoned" {
		return
	}
	f.Maturity += amount
	if f.Maturity > 1 {
		f.Maturity = 1
	}
	f.Progress = fmt.Sprintf("Day%d·酝酿度%.0f%%", s.day, f.Maturity*100)
	if f.Maturity >= 0.8 {
		f.Status = "progressing" // 即将爆发
	}
	s.foreshadows[name] = f
}

// RipeForeshadows 即将爆发的伏笔（酝酿度≥0.8，事件 Agent 该安排回收了）
func (s *Simulator) RipeForeshadows() string {
	if len(s.foreshadows) == 0 {
		return ""
	}
	var names []string
	for n, f := range s.foreshadows {
		if (f.Status == "planted" || f.Status == "progressing") && f.Maturity >= 0.8 {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return strings.Join(names, "、")
}

// ---------- P2 动态负责人 Agent 注册表（公司新岗位：随剧情需要"招聘"） ----------

// RegisterDynamicAgent 注册/更新一个动态负责人 Agent（同名更新，不重复创建）
func (s *Simulator) RegisterDynamicAgent(name, typ, focus, state string) string {
	for i := range s.dynamicAgents {
		if s.dynamicAgents[i].Name == name {
			s.dynamicAgents[i].Focus = focus
			if state != "" {
				s.dynamicAgents[i].State = state
			}
			s.dynamicAgents[i].UpdatedDay = s.day
			return s.dynamicAgents[i].ID
		}
	}
	da := DynamicAgent{
		ID: fmt.Sprintf("agent-%04d", len(s.dynamicAgents)+1), Name: name, Type: typ,
		Focus: focus, State: state, CreatedDay: s.day, UpdatedDay: s.day,
	}
	s.dynamicAgents = append(s.dynamicAgents, da)
	return da.ID
}

// DynamicAgentsState 活跃动态负责人状态（注入事件/铺垫 Agent，让"部门"协同）
func (s *Simulator) DynamicAgentsState() string {
	if len(s.dynamicAgents) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, da := range s.dynamicAgents {
		sb.WriteString(fmt.Sprintf("· %s【%s】：%s（下一步：%s）\n", da.Name, da.Type, da.Focus, da.State))
	}
	return strings.TrimSpace(sb.String())
}

// RegisterForeshadow 登记伏笔（事件 Agent 在事件里带 foreshadow 字段）
func (s *Simulator) RegisterForeshadow(name string) {
	if name == "" {
		return
	}
	if s.foreshadows == nil {
		s.foreshadows = map[string]Foreshadow{}
	}
	if _, ok := s.foreshadows[name]; ok {
		return
	}
	s.foreshadows[name] = Foreshadow{Name: name, Planted: s.day, Status: "planted", Progress: "埋下伏笔"}
}

func (s *Simulator) AdvanceForeshadow(name, progress string) {
	if s.foreshadows == nil {
		return
	}
	f, ok := s.foreshadows[name]
	if !ok {
		return
	}
	f.Status = "progressing"
	f.Progress = fmt.Sprintf("Day%d·%s", s.day, progress)
	s.foreshadows[name] = f
}

func (s *Simulator) ResolveForeshadow(name, progress string) {
	if s.foreshadows == nil {
		return
	}
	f, ok := s.foreshadows[name]
	if !ok {
		return
	}
	f.Status = "resolved"
	f.Progress = fmt.Sprintf("Day%d·%s", s.day, progress)
	f.Resolved = s.day
	s.foreshadows[name] = f
}

// OpenForeshadows 未回收伏笔清单（注入事件生成/小说写手，避免忘坑）
func (s *Simulator) OpenForeshadows() string {
	if len(s.foreshadows) == 0 {
		return ""
	}
	var names []string
	for n, f := range s.foreshadows {
		if f.Status == "planted" || f.Status == "progressing" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, "、")
}

// LocationSenses 所有地点的感官档案（供小说写手写场景用：先知道这个地点什么味/什么声/什么光）
func (s *Simulator) LocationSenses() string {
	st := s.engine.State()
	if len(st.WorldLevel.Locations) == 0 {
		return ""
	}
	var names []string
	for n := range st.WorldLevel.Locations {
		names = append(names, n)
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		l := st.WorldLevel.Locations[n]
		if l.Senses != "" {
			sb.WriteString(fmt.Sprintf("· %s：%s\n", n, l.Senses))
		}
	}
	return strings.TrimSpace(sb.String())
}

// ---------- 工具 ----------

func formatLocations(st *engine.WorldState) string {
	if len(st.WorldLevel.Locations) == 0 {
		return "（暂无地点记录）"
	}
	var names []string
	for n := range st.WorldLevel.Locations {
		names = append(names, n)
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		l := st.WorldLevel.Locations[n]
		sb.WriteString(fmt.Sprintf("· %s【%s】%s\n", n, l.State, l.Note))
	}
	return strings.TrimSpace(sb.String())
}

// formatRecentEvents 取编年史最近 N 条事件/对话（供时间过渡段/铺垫/总导演衔接）
func formatRecentEvents(chronicle []ChronicleEntry, n int) string {
	if len(chronicle) == 0 {
		return "（尚无）"
	}
	var sb strings.Builder
	cnt := 0
	for i := len(chronicle) - 1; i >= 0 && cnt < n; i-- {
		e := chronicle[i]
		if e.Kind == "FACT" || e.Kind == "SAID" {
			sb.WriteString(fmt.Sprintf("· Day%d %s：%s\n", e.Day, e.Kind, truncate(e.Content, 80)))
			cnt++
		}
	}
	if sb.Len() == 0 {
		return "（尚无）"
	}
	return strings.TrimSpace(sb.String())
}

// formatPendingEvents 把待触发的遭遇链种子格式化成提示
func formatPendingEvents(s *Simulator, day int) string {
	var sb strings.Builder
	if len(s.pending) == 0 {
		return "（无）"
	}
	days := make([]int, 0, len(s.pending))
	for d := range s.pending {
		if d > day {
			days = append(days, d)
		}
	}
	sort.Ints(days)
	for _, d := range days {
		for _, ev := range s.pending[d] {
			sb.WriteString(fmt.Sprintf("· Day%d 将发生：%s（%s）\n", d, ev.Title, ev.Frame))
		}
	}
	if sb.Len() == 0 {
		return "（无）"
	}
	return strings.TrimSpace(sb.String())
}