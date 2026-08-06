package narrative

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"worldsim/internal/llm"
	"worldsim/internal/sim"
)

// ---------- 剧情单元规划（LLM 分章，替代按天数硬切） ----------

// PlanUnits 规划全书章节方案。
// 核心原则：章节骨架 = arcBook 段落（GM 已规划的情节单元）；LLM 按事件密度/戏剧强度决定拆并，
// 绝不按固定天数切章。返回 StoryUnit[]（= 章节方案）。
func PlanUnits(ctx context.Context, in *Input) ([]StoryUnit, error) {
	// 无段落（旧世界/模拟初期）：走降级路径——按编年史事件密度聚类
	if len(in.ArcBook) == 0 {
		return planUnitsNoArcs(in), nil
	}
	return planUnitsWithArcs(ctx, in)
}

// planUnitsWithArcs 有段落：把 arcBook 交给 LLM 细化成章节。
func planUnitsWithArcs(ctx context.Context, in *Input) ([]StoryUnit, error) {
	groups, loose := GroupArcs(in)

	// 组装段落信息文本
	var sb strings.Builder
	sb.WriteString("【世界背景（时间尺度/设定，判断节奏用）】\n")
	if in.WB != nil {
		sb.WriteString(in.WB.ForNovelist() + "\n")
	}
	sb.WriteString("\n【剧情段落账本（GM 已规划的网文情节单元，按开段日排序）】\n")
	for _, g := range groups {
		a := g.Arc
		sb.WriteString(fmt.Sprintf("· 段落#%d【%s】开段Day%d", a.Num, a.ArcName, a.Day))
		if a.DoneDay > 0 {
			sb.WriteString(fmt.Sprintf("～Day%d", a.DoneDay))
		} else {
			sb.WriteString("（进行中）")
		}
		sb.WriteString("\n  目标：" + a.Goal)
		if a.Villain != "" {
			sb.WriteString("\n  反派：" + a.Villain)
		}
		if len(a.Milestones) > 0 {
			sb.WriteString("\n  关键节点：" + strings.Join(a.Milestones, " → "))
		}
		if a.Payoff != "" {
			sb.WriteString("\n  爽点：" + a.Payoff)
		}
		if a.TimeHint != "" {
			sb.WriteString("\n  时间跨度：" + a.TimeHint)
		}
		// 段落内编年史摘要（事件密度）
		if len(g.Entries) > 0 {
			sb.WriteString("\n  段内事件（" + itoa(len(g.Entries)) + "条）：" + summarizeEntries(g.Entries, 6))
		}
		sb.WriteString("\n")
	}
	// 松散条目（段落间隙）
	if len(loose) > 0 {
		sb.WriteString("\n【段落间隙/初期事件（不属于任何段落，可并入相邻章或独立成过渡章）】\n")
		sb.WriteString(summarizeEntries(loose, 10) + "\n")
	}
	// 剧情方向
	dirText := directionText(in.Direction)
	if dirText != "" {
		sb.WriteString("\n【用户剧情方向（必须遵守）】\n" + dirText + "\n")
	}

	system := `你是网文小说的"剧情规划师"。你拿到的是世界模拟器已经规划好的"剧情段落账本"——每个段落=一个网文情节单元（有目标/反派/关键节点/爽点/时间跨度），段落内还有真实的模拟事件。
你的任务：把段落账本细化为**最终的章节方案**。这是"分章"，不是"写正文"——你要决定"一章该讲哪个故事片段"。
输出严格 JSON，格式：
{"chapters":[{"title":"章节标题（2~8字，有悬念/画面感）","arc_ref":<段落编号,整数>,"day_start":<起始天>,"day_end":<结束天>,"goal":"本章目标（主角要达成什么）","villain":"本章反派动作（若有）","payoff":"本章爽点/收获（若有）","hook":"结尾悬念（具体到谁/什么/在哪）","chars":["登场角色名"],"note":"为什么这么切（一句话说明依据）"}]}
分章铁律（这是核心，违反=不合格）：
1. **绝对禁止按固定天数切章**（比如"每3天一章"）。切章的唯一依据是"叙事是否完整"：
   · 一个段落默认是一章（它本身就有目标→行动→收获→展示的完整弧线）
   · 段落太长（关键节点多、事件密集、时间跨度大、信息量大）→ 拆成 2~3 章：按关键节点/剧情转折切，每章是段落里一个相对完整的子冲突
   · 段落太短或相邻段落太碎（连续多个小段落）→ 合并成一章（合并后仍要有完整起承转合）
   · 段落间隙/初期事件（不属于任何段落）→ 并入相邻章的开头或结尾，或独立成过渡章（视内容是否值得一章）
2. **时间跨度是信息，不是切章依据**：时间只是世界的时间戳——不同世界的时间流速与叙事节奏无关，一切以本世界实际发生的事件为准。判断"该不该拆"看的是**事件密度与戏剧强度**：一天发生三个大事件可以拆两章，长期平静期可能一章带过（用"数年后"开场）。
3. 每章必须有"事"：目标/冲突/转折/收获/悬念至少占一样。纯过渡的松散日子最多独立成一章，且要短。
4. 章节按时间顺序排列，覆盖全部模拟天数（从第一段开段日到最后一天，不遗漏不跳天）。
5. 登场角色（chars）从段内事件里出现的人里取（主角必在）。
6. 标题要有网文感：悬念/冲突/画面，禁止"第X天""日常"这类流水账名。
7. 只输出 JSON，不要其他文字。`
	user := sb.String() + "\n请输出分章方案。"

	if in.LLM == nil {
		return fallbackUnits(in), nil
	}
	raw, err := llm.CallAPITierSync(ctx, in.LLM, "fast", system, user)
	if err != nil {
		return fallbackUnits(in), fmt.Errorf("分章规划失败(%v)，用段落骨架兜底", err)
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return fallbackUnits(in), fmt.Errorf("分章规划输出无JSON，用段落骨架兜底")
	}
	var resp struct {
		Chapters []struct {
			Title    string   `json:"title"`
			ArcRef   int      `json:"arc_ref"`
			DayStart int      `json:"day_start"`
			DayEnd   int      `json:"day_end"`
			Goal     string   `json:"goal"`
			Villain  string   `json:"villain"`
			Payoff   string   `json:"payoff"`
			Hook     string   `json:"hook"`
			Chars    []string `json:"chars"`
			Note     string   `json:"note"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return fallbackUnits(in), fmt.Errorf("分章方案JSON解析失败(%v)，用段落骨架兜底", err)
	}
	if len(resp.Chapters) == 0 {
		return fallbackUnits(in), nil
	}
	var units []StoryUnit
	for i, c := range resp.Chapters {
		u := StoryUnit{
			Num:      i + 1,
			ArcRef:   c.ArcRef,
			Title:    c.Title,
			DayStart: c.DayStart,
			DayEnd:   c.DayEnd,
			Goal:     c.Goal,
			Villain:  c.Villain,
			Payoff:   c.Payoff,
			Hook:     c.Hook,
			Chars:    c.Chars,
		}
		// 补 Days（从 DayStart~DayEnd 全部有记录的天）
		u.Days = daysInRange(in.Chronicle, c.DayStart, c.DayEnd)
		if u.DayEnd < u.DayStart {
			u.DayEnd = u.DayStart
		}
		units = append(units, u)
	}
	return units, nil
}

// fallbackUnits 无 LLM/LLM 失败时：段落骨架兜底（一段一章，松散日并入前段）。
func fallbackUnits(in *Input) []StoryUnit {
	groups, loose := GroupArcs(in)
	var units []StoryUnit
	num := 1
	for _, g := range groups {
		a := g.Arc
		u := StoryUnit{
			Num:      num,
			ArcRef:   a.Num,
			Title:    a.ArcName,
			DayStart: a.Day,
			DayEnd:   a.DoneDay,
			Goal:     a.Goal,
			Villain:  a.Villain,
			Payoff:   a.Payoff,
			Chars:    collectChars(g.Entries, in.Entities, in.Hero),
		}
		if u.DayEnd < u.DayStart {
			u.DayEnd = u.DayStart
		}
		u.Days = daysInRange(in.Chronicle, u.DayStart, u.DayEnd)
		units = append(units, u)
		num++
	}
	// 松散日：并入最后一章（若在最后段落之后）或作为过渡章
	if len(loose) > 0 {
		firstDay, lastDay := looseDays(loose)
		if len(units) > 0 && firstDay >= units[len(units)-1].DayEnd {
			units[len(units)-1].DayEnd = lastDay
			units[len(units)-1].Days = daysInRange(in.Chronicle, units[len(units)-1].DayStart, lastDay)
		} else {
			units = append(units, StoryUnit{
				Num: num, Title: "过渡", DayStart: firstDay, DayEnd: lastDay,
				Days: daysInRange(in.Chronicle, firstDay, lastDay),
			})
		}
	}
	return units
}

// planUnitsNoArcs 无段落（旧世界/初期）：按编年史事件密度聚类分章。
// 降级策略：戏剧日（Weight≥0.55 或含对话/抉择）聚集为章；跨度过大/无事件则 LLM 快进或并章。
func planUnitsNoArcs(in *Input) []StoryUnit {
	days := daysOf(in.Chronicle)
	for d := range in.Thinkings {
		days = appendUnique(days, d)
	}
	if len(days) == 0 {
		return nil
	}
	sortDays(days)
	dramaDay := func(d int) bool {
		if in.Thinkings[d] != "" {
			return true
		}
		for _, e := range in.Chronicle {
			if e.Day == d && (e.Kind == "SAID" || e.Weight >= 0.55) {
				return true
			}
		}
		return false
	}
	var units []StoryUnit
	num := 1
	var chunk []int
	drama := 0
	for _, d := range days {
		chunk = append(chunk, d)
		if dramaDay(d) {
			drama++
		}
		// 成章条件：≥2 个戏剧日 且覆盖 ≥3 个记录日；或记录日超 14 强制成章
		if (drama >= 2 && len(chunk) >= 3) || len(chunk) >= 14 {
			units = append(units, StoryUnit{
				Num: num, DayStart: chunk[0], DayEnd: chunk[len(chunk)-1],
				Days: chunk, Title: "第" + itoa(num) + "章",
				Goal: "推进剧情", Chars: collectChars(chronicleForDays(in.Chronicle, chunk), in.Entities, in.Hero),
			})
			num++
			chunk = nil
			drama = 0
		}
	}
	if len(chunk) > 0 {
		units = append(units, StoryUnit{
			Num: num, DayStart: chunk[0], DayEnd: chunk[len(chunk)-1],
			Days: chunk, Title: "第" + itoa(num) + "章",
			Goal: "推进剧情", Chars: collectChars(chronicleForDays(in.Chronicle, chunk), in.Entities, in.Hero),
		})
	}
	return units
}

// ---------- 辅助 ----------

// directionText 方向文本（空=不注入）。
func directionText(d Direction) string {
	var parts []string
	if d.MainLine != "" {
		parts = append(parts, "主线/核心冲突："+d.MainLine)
	}
	if d.PayoffDensity != "" {
		label := map[string]string{"low": "低（蓄力为主，爽点稀疏但爆发更狠）", "normal": "正常（张弛有度）", "high": "高（每章尽量有收获/打脸/突破）"}[d.PayoffDensity]
		if label == "" {
			label = d.PayoffDensity
		}
		parts = append(parts, "爽点密度："+label)
	}
	if d.StyleTilt != "" {
		parts = append(parts, "文风倾向："+d.StyleTilt)
	}
	if len(d.FocusArcs) > 0 {
		parts = append(parts, "优先展开段落编号："+joinInts(d.FocusArcs))
	}
	return strings.Join(parts, "\n")
}

func joinInts(ns []int) string {
	var s []string
	for _, n := range ns {
		s = append(s, itoa(n))
	}
	return strings.Join(s, ",")
}

// summarizeEntries 条目摘要（合并同天，取前 n 条）。
func summarizeEntries(entries []sim.ChronicleEntry, n int) string {
	byDay := map[int][]string{}
	var days []int
	for _, e := range entries {
		if _, ok := byDay[e.Day]; !ok {
			days = append(days, e.Day)
		}
		byDay[e.Day] = append(byDay[e.Day], e.Content)
	}
	sortDays(days)
	var sb strings.Builder
	count := 0
	for _, d := range days {
		if count >= n {
			sb.WriteString(fmt.Sprintf("…（其余 %d 天略）", len(days)-count))
			break
		}
		sb.WriteString(fmt.Sprintf("Day%d：%s\n", d, strings.Join(byDay[d], "；")))
		count++
	}
	return sb.String()
}

func daysInRange(chronicle []sim.ChronicleEntry, from, to int) []int {
	set := map[int]bool{}
	for _, e := range chronicle {
		if e.Day >= from && e.Day <= to {
			set[e.Day] = true
		}
	}
	var days []int
	for d := range set {
		days = append(days, d)
	}
	sortDays(days)
	return days
}

func chronicleForDays(chronicle []sim.ChronicleEntry, days []int) []sim.ChronicleEntry {
	set := map[int]bool{}
	for _, d := range days {
		set[d] = true
	}
	var out []sim.ChronicleEntry
	for _, e := range chronicle {
		if set[e.Day] {
			out = append(out, e)
		}
	}
	return out
}

func looseDays(entries []sim.ChronicleEntry) (int, int) {
	first, last := 0, 0
	for _, e := range entries {
		if first == 0 || e.Day < first {
			first = e.Day
		}
		if e.Day > last {
			last = e.Day
		}
	}
	return first, last
}

func appendUnique(days []int, d int) []int {
	for _, x := range days {
		if x == d {
			return days
		}
	}
	return append(days, d)
}

func sortDays(days []int) {
	for i := 0; i < len(days); i++ {
		for j := i + 1; j < len(days); j++ {
			if days[j] < days[i] {
				days[i], days[j] = days[j], days[i]
			}
		}
	}
}
