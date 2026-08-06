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
func (s *Simulator) SeedLocations(ctx context.Context) []engine.Change {
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
		if senses := s.seedLocationSenses(ctx, defaults); len(senses) > 0 {
			for name, sn := range senses {
				changes = append(changes, engine.Change{Path: "world_level.locations." + name + ".senses", Op: "set", Value: sn})
			}
		}
	}
	return changes
}

// EnsureLocationSenses 为缺少感官档案的地点补生成（旧世界升级/新地点登记后用，按本世界规则）
func (s *Simulator) EnsureLocationSenses(ctx context.Context) []engine.Change {
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
	if senses := s.seedLocationSenses(ctx, missing); len(senses) > 0 {
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
func (s *Simulator) seedLocationSenses(ctx context.Context, defaults []struct{ name, typ, note string }) map[string]string {
	ctx = llm.WithSpan(ctx, "地点感官")
	var locList strings.Builder
	for _, d := range defaults {
		locList.WriteString(fmt.Sprintf("- %s【%s】%s\n", d.name, d.typ, d.note))
	}
	system := "你是场景感官设计师。为下面这些地点各写一段'感官档案'（100字内）：必须覆盖五维感官——视觉（看到什么，含反常细节）、听觉（环境声）、触觉（温度/质感/风）、嗅觉（气味，气味即情绪）、第六感（氛围直觉）。必须贴合本世界的时代与设定，具体可感，禁止空泛（如'环境舒适'）。输出严格 JSON：{\"地点名\":\"感官描述\"}"
	user := fmt.Sprintf("世界背景：%s\n地点列表：\n%s", s.wb.ForWorldBrief(), locList.String())
	raw, err := s.llm.CompleteTier(ctx, "fast:low", system, user)
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
	stateJSON := compactState(st, s.heroName) // 精简版状态（省 token：不传 extra 大档案）
	system := `你是世界反应引擎。主角刚刚做了行动，评估它对世界的影响（蝴蝶效应）。
规则：
1. 输出严格 JSON：{"changes":[{"path":"world_level.global_events","op":"add","value":"..."},{"path":"world_level.locations.<地点>.state","op":"set","value":"..."},{"path":"world_level.factions.<势力>.power","op":"set","value":<0~1数值>}],"impact":"一句话总结影响（20字内）"}
2. 允许路径：world_level.global_events(追加)、world_level.locations.{地点}.{state|note}、world_level.factions.{势力}.{stance|power}、world_level.tension
3. 影响要克制而真实：小行动有小涟漪，大行动才改势力格局；主角目前还是小人物，不会一夜改变世界
4. 变化要能体现在后续事件里（封禁的地点、增强的势力、新的传闻）`
	user := fmt.Sprintf("世界状态：\n%s\n主角行动：%s", stateJSON, heroAction)
	raw, err := s.llm.CompleteTier(ctx, "fast:low", system, user)
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
	Name     string  `json:"name"`
	Planted  int     `json:"planted"`  // 埋设日
	Status   string  `json:"status"`   // planted | progressing | resolved | abandoned
	Progress string  `json:"progress"` // 当前进展（每次推进更新）
	Resolved int     `json:"resolved"` // 回收日
	Maturity float64 `json:"maturity"` // 酝酿度 0~1：铺垫期慢慢攒，到阈值自然爆发成戏剧事件
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
// 上下文管理：只列近期更新过的负责人线（AgentActiveWindow 天内），沉睡线不注入防膨胀。
const agentActiveWindow = 20 // 20天内更新过的视为活跃

func (s *Simulator) DynamicAgentsState() string {
	if len(s.dynamicAgents) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, da := range s.dynamicAgents {
		if s.day-da.UpdatedDay > agentActiveWindow {
			continue // 沉睡线：跳过（不注入）
		}
		// 截断 focus/state 长文本（负责人线只需给事件 Agent 一个"方向感"，细节太长占 token）
		focus := truncateCN(da.Focus, 80)
		state := truncateCN(da.State, 60)
		sb.WriteString(fmt.Sprintf("· %s【%s】：%s（下一步：%s）\n", da.Name, da.Type, focus, state))
	}
	return strings.TrimSpace(sb.String())
}

// truncateCN 按中文字符截断（len 是字节数，中文 3 字节/字，按 rune 截断更准）
func truncateCN(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
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

// matchForeshadowKey 模糊匹配伏笔：Agent 填的 resolve 名（如"槐树的歌"）和注册名
// （如"短·槐树的歌：颜承宗望见..."）通常不一致——精确匹配会漏标，导致剧情已回收
// 但伏笔还挂在清单里占 token。这里去掉前缀（短·/中·/长·/短线·）+ 只比核心词：
// 注册名的核心词被 Agent 名包含，或 Agent 名被注册名包含，即命中。
func matchForeshadowKey(registered, agent string) bool {
	if registered == agent {
		return true
	}
	trim := func(s string) string {
		s = strings.TrimSpace(s)
		for _, p := range []string{"短·", "中·", "长·", "短线·", "中线·", "长线·"} {
			s = strings.TrimPrefix(s, p)
		}
		// 去掉冒号后的详细描述（注册名常带长描述）
		if i := strings.Index(s, "："); i > 0 {
			s = s[:i]
		}
		if i := strings.Index(s, ":"); i > 0 {
			s = s[:i]
		}
		return strings.TrimSpace(s)
	}
	rt, at := trim(registered), trim(agent)
	if rt == "" || at == "" {
		return false
	}
	return strings.Contains(rt, at) || strings.Contains(at, rt)
}

func (s *Simulator) ResolveForeshadow(name, progress string) {
	if s.foreshadows == nil || name == "" {
		return
	}
	// 精确匹配优先；失败则模糊匹配（Agent 填的名字和注册名通常不完全一致）
	// 注意：同名不同描述的变体（如"短·仓底心音"和"短·仓底心音：脉搏与槐歌错开..."）
	// 剧情上是一回事——Agent 回收"仓底心音"时应把同核心词的所有变体都标记回收，
	// 避免旧版本永远挂在清单里占 token（用户发现：剧情已兑现但未标记的伏笔）。
	resolved := false
	for registered, f := range s.foreshadows {
		if f.Status == "resolved" || f.Status == "abandoned" {
			continue
		}
		if matchForeshadowKey(registered, name) {
			f.Status = "resolved"
			f.Progress = fmt.Sprintf("Day%d·%s", s.day, progress)
			f.Resolved = s.day
			s.foreshadows[registered] = f
			resolved = true
			// 继续遍历：同核心词的其他变体一起回收
		}
	}
	_ = resolved
}

// shortNames 伏笔短名：只保留前缀+冒号前的核心名（如"短·槐树的歌"），
// 去掉"：颜承宗望见..."的长描述——清单只提示"有哪些坑"，细节由记忆/编年史承载，
// 避免伏笔越多名字越长、user 无限膨胀（30个长描述伏笔 ≈ 1.5KB，短名能砍掉一大半）。
func shortNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		s := n
		if i := strings.Index(s, "："); i > 0 {
			s = s[:i]
		}
		if i := strings.Index(s, ":"); i > 0 {
			s = s[:i]
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

// OpenForeshadows 未回收伏笔清单（注入事件生成/小说写手，避免忘坑）
// 上下文管理原则：长短线伏笔都要全程记住（长线贯穿全书，不能因"近期没动静"被遗忘），
// 所以这里只做两件事：
//  1. 排除已回收的（resolved/abandoned）——回收了就不再占上下文
//  2. 按长线/中线/短线分组展示，让事件 Agent 清楚回收优先级（短线该尽快收、长线慢慢铺）
//
// 不按活跃度裁剪——那会导致长线伏笔被遗忘、烂尾。
func (s *Simulator) OpenForeshadows() string {
	if len(s.foreshadows) == 0 {
		return ""
	}
	var long, mid, short, other []string
	for n, f := range s.foreshadows {
		if f.Status == "resolved" || f.Status == "abandoned" {
			continue // 已回收/已弃：不注入
		}
		switch {
		case strings.HasPrefix(n, "长") || strings.HasPrefix(n, "长线"):
			long = append(long, n)
		case strings.HasPrefix(n, "中") || strings.HasPrefix(n, "中线"):
			mid = append(mid, n)
		case strings.HasPrefix(n, "短") || strings.HasPrefix(n, "短线"):
			short = append(short, n)
		default:
			other = append(other, n)
		}
	}
	sort.Strings(long)
	sort.Strings(mid)
	sort.Strings(short)
	sort.Strings(other)
	var sb strings.Builder
	if len(long) > 0 {
		sb.WriteString("长线（跨度大，持续推进，大谜底浮出就收）：" + strings.Join(shortNames(long), "、") + "\n")
	}
	if len(mid) > 0 {
		sb.WriteString("中线（段落内展开，谜底全浮出再收）：" + strings.Join(shortNames(mid), "、") + "\n")
	}
	if len(short) > 0 {
		sb.WriteString("短线（近期该回收，优先主动收）：" + strings.Join(shortNames(short), "、") + "\n")
	}
	if len(other) > 0 {
		sb.WriteString("其他（未分类）：" + strings.Join(shortNames(other), "、") + "\n")
	}
	res := strings.TrimSpace(sb.String())
	if res == "" {
		return "（暂无未回收伏笔，可埋新坑）"
	}
	return res
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
// 上下文管理：只列未来 pendingWindow 天内的种子（更远的已编排好但暂不注入，
// 到日子再由 consumePendingEvents 兑现——不注入不丢，避免长跑 token 膨胀）。
const pendingWindow = 14

func formatPendingEvents(s *Simulator, day int) string {
	var sb strings.Builder
	if len(s.pending) == 0 {
		return "（无）"
	}
	days := make([]int, 0, len(s.pending))
	for d := range s.pending {
		if d > day && d <= day+pendingWindow {
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
