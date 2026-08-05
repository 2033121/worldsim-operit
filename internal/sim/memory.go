package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"worldsim/internal/llm"
)

// ---------- 记忆系统（§4.6：独立记忆流 + 重要性 + 反思 + 检索） ----------

// MemoryEntry 单条记忆（Smallville 范式：时间戳 + 重要性分数）
type MemoryEntry struct {
	Day        int     `json:"day"`
	Time       string  `json:"time"`
	Actor      string  `json:"actor"`   // 谁的记忆
	Content    string  `json:"content"` // 记忆内容（已转述为本人视角）
	Kind       string  `json:"kind"`    // event | dialogue | state | reflection | plan
	Importance float64 `json:"importance"` // 0~1
}

// MemoryStore 全角色记忆库（每个角色独立记忆流，互不串通）
// 三层记忆架构（防膨胀，参考 Mem0/MemGPT/Generative Agents）：
//   Working 工作记忆：近30天完整记忆（活跃期，全量保留）
//   Archive 存档记忆：更早记忆按月LLM摘要压缩（每月1-3条）
//   Core    核心记忆：人设/身份/长期目标（存在 entities.extra，不占记忆库）
// 压缩链：Working → 月度摘要 → 年度摘要 → 遗忘（超长期低重要）
type MemoryStore struct {
	memories map[string][]MemoryEntry // Working：近30天完整记忆
	archive  map[string][]MemoryEntry // Archive：历史月度摘要（day=该月最后一天）
	path     string
	llm      *LLMClient // 压缩用（nil 则规则式摘要）
}

// NewMemoryStore 创建记忆库（自动加载已有持久化文件）
func NewMemoryStore(path string) *MemoryStore {
	ms := &MemoryStore{memories: map[string][]MemoryEntry{}, archive: map[string][]MemoryEntry{}, path: path}
	if data, err := os.ReadFile(path); err == nil {
		var raw struct {
			Memories map[string][]MemoryEntry `json:"memories"`
			Archive  map[string][]MemoryEntry `json:"archive,omitempty"`
		}
		if json.Unmarshal(data, &raw) == nil {
			ms.memories = raw.Memories
			ms.archive = raw.Archive
		} else {
			// 兼容旧格式（只有 memories 顶层）
			_ = json.Unmarshal(data, &ms.memories)
		}
	}
	if ms.memories == nil {
		ms.memories = map[string][]MemoryEntry{}
	}
	if ms.archive == nil {
		ms.archive = map[string][]MemoryEntry{}
	}
	return ms
}

// SetCompressor 设置记忆压缩用的 LLM（月度摘要）
func (ms *MemoryStore) SetCompressor(c *LLMClient) { ms.llm = c }

// Save 持久化全部记忆
func (ms *MemoryStore) Save() {
	if ms == nil {
		return
	}
	payload := map[string]any{"memories": ms.memories, "archive": ms.archive}
	if b, err := json.MarshalIndent(payload, "", "  "); err == nil {
		if err := os.WriteFile(ms.path, b, 0644); err != nil {
			fmt.Printf(" [记忆] 保存失败: %v\n", err)
		}
	}
}

// Add 写入一条记忆（程序级校验：内容非空、importance 0~1）
func (ms *MemoryStore) Add(actor, content, kind string, importance float64) {
	if ms == nil || strings.TrimSpace(content) == "" {
		return
	}
	if importance < 0 {
		importance = 0
	}
	if importance > 1 {
		importance = 1
	}
	e := MemoryEntry{
		Day: 0, Time: time.Now().Format("01-02 15:04"),
		Actor: actor, Content: content, Kind: kind, Importance: importance,
	}
	ms.memories[actor] = append(ms.memories[actor], e)
}

// AddDay 写入一条带模拟日的记忆
func (ms *MemoryStore) AddDay(actor, content, kind string, importance float64, day int) {
	ms.Add(actor, content, kind, importance)
	entries := ms.memories[actor]
	if len(entries) > 0 {
		entries[len(entries)-1].Day = day
	}
}

// Count 返回某角色 Working 完整记忆条数（记忆量驱动压缩的触发依据）
func (ms *MemoryStore) Count(actor string) int {
	if ms == nil {
		return 0
	}
	return len(ms.memories[actor])
}
func (ms *MemoryStore) Retrieve(actor, query string, limit int) []MemoryEntry {
	if ms == nil {
		return nil
	}
	now := time.Now()
	qWords := splitKeywords(query)
	type scored struct {
		e     MemoryEntry
		score float64
	}
	var scoredList []scored
	// Working：完整记忆（近30天）
	for _, e := range ms.memories[actor] {
		score := scoreMemory(e, qWords, now)
		scoredList = append(scoredList, scored{e, score})
	}
	// Archive：月度摘要（历史关键信息，如10年前的约定）
	for _, e := range ms.archive[actor] {
		score := scoreMemory(e, qWords, now) + 0.8 // 摘要保底权重（防完全遗忘重要历史）
		scoredList = append(scoredList, scored{e, score})
	}
	if len(scoredList) == 0 {
		return nil
	}
	sort.Slice(scoredList, func(i, j int) bool { return scoredList[i].score > scoredList[j].score })
	if len(scoredList) > limit {
		scoredList = scoredList[:limit]
	}
	out := make([]MemoryEntry, 0, len(scoredList))
	for _, s := range scoredList {
		out = append(out, s.e)
	}
	return out
}

// scoreMemory 单条记忆相关性打分（关键词+重要性+时间衰减）
func scoreMemory(e MemoryEntry, qWords []string, now time.Time) float64 {
	score := 0.0
	for _, w := range qWords {
		if w != "" && strings.Contains(e.Content, w) {
			score += 2.0
		}
	}
	score += e.Importance * 1.5
	ageDays := now.Sub(parseMemTime(e.Time)).Hours() / 24
	score += 1.0 / (1.0 + ageDays*0.15)
	if e.Kind == "reflection" || e.Kind == "summary" {
		score += 1.0
	}
	return score
}

// Consolidate 月度记忆压缩（防膨胀核心）：把 Working 中早于 cutoffDay 的记忆
// 用 LLM 摘要成 1-3 条"第N月摘要"存入 Archive，并从 Working 移除。
// 无 LLM 时用规则式保留（保留 importance 高的前几条）。
func (ms *MemoryStore) Consolidate(actor string, cutoffDay int) {
	if ms == nil {
		return
	}
	old := ms.memories[actor]
	var keep, toCompress []MemoryEntry
	for _, e := range old {
		if e.Day > 0 && e.Day < cutoffDay {
			toCompress = append(toCompress, e)
		} else {
			keep = append(keep, e)
		}
	}
	if len(toCompress) == 0 {
		return
	}
	ms.memories[actor] = keep

	// 该批记忆的月份范围（用于摘要标注）
	minDay, maxDay := toCompress[0].Day, toCompress[0].Day
	for _, e := range toCompress {
		if e.Day < minDay {
			minDay = e.Day
		}
		if e.Day > maxDay {
			maxDay = e.Day
		}
	}
	label := fmt.Sprintf("第%d月记忆摘要", (minDay-1)/30+1)

	var summary string
	if ms.llm != nil {
		summary = ms.summarizeMemories(actor, toCompress)
	}
	if summary == "" {
		// 规则式：保留 importance 最高的前3条
		sort.Slice(toCompress, func(i, j int) bool { return toCompress[i].Importance > toCompress[j].Importance })
		var parts []string
		for i := 0; i < len(toCompress) && i < 3; i++ {
			parts = append(parts, toCompress[i].Content)
		}
		summary = strings.Join(parts, "；")
	}
	entry := MemoryEntry{
		Day: maxDay, Time: time.Now().Format("01-02 15:04"),
		Actor: actor, Content: fmt.Sprintf("【%s】%s", label, summary),
		Kind: "summary", Importance: 0.85,
	}
	ms.archive[actor] = append(ms.archive[actor], entry)

	// Archive 超过 24 条（≈2年）→ 把最老的合并成年度摘要
	if len(ms.archive[actor]) > 24 {
		ms.yearlyConsolidate(actor)
	}
	fmt.Printf(" [记忆] %s：%d 条旧记忆压缩为摘要（Day %d-%d）\n", actor, len(toCompress), minDay, maxDay)
}

// summarizeMemories 用 LLM 提炼记忆摘要（保留因果关键：关系/伏笔/事件/教训）
func (ms *MemoryStore) summarizeMemories(actor string, entries []MemoryEntry) string {
	ctx := llm.WithSpan(context.Background(), "记忆摘要")
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("- Day%d [%s] %s\n", e.Day, e.Kind, e.Content))
	}
	system := `你是{actor}的"记忆整理者"。回顾这段时间的经历，提炼成 1-3 条"对未来依然重要的记忆"：
- 重要的人际关系进展（和谁走近/疏远/决裂）
- 未解决的事、未兑现的约定、埋下的隐患
- 身份/工作/住处的重大变化
- 学到的教训、形成的习惯或判断
要求：用第一人称（"我"），每条不超过50字，输出严格 JSON：{"key_memories":["...","..."]}`
	system = strings.ReplaceAll(system, "{actor}", actor)
	raw, err := ms.llm.CompleteTier(ctx, "fast", system, sb.String())
	if err != nil {
		return ""
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return ""
	}
	var resp struct {
		KeyMemories []string `json:"key_memories"`
	}
	if json.Unmarshal([]byte(jsonStr), &resp) != nil {
		return ""
	}
	var out []string
	for _, m := range resp.KeyMemories {
		m = strings.TrimSpace(m)
		if m != "" {
			out = append(out, m)
		}
	}
	return strings.Join(out, "；")
}

// yearlyConsolidate 年度合并：24+条月度摘要 → 压缩成年度摘要
func (ms *MemoryStore) yearlyConsolidate(actor string) {
	arch := ms.archive[actor]
	if len(arch) <= 12 {
		return
	}
	// 取最早的12条合并成1条年度摘要
	old := arch[:12]
	rest := arch[12:]
	firstDay := old[0].Day
	lastDay := old[len(old)-1].Day
	var parts []string
	for _, e := range old {
		parts = append(parts, e.Content)
	}
	merged := MemoryEntry{
		Day: lastDay, Time: time.Now().Format("01-02 15:04"),
		Actor: actor, Content: fmt.Sprintf("【第%d-第%d月综合记忆】%s", (firstDay-1)/30+1, (lastDay-1)/30+1, strings.Join(parts, "；")),
		Kind: "summary", Importance: 0.9,
	}
	ms.archive[actor] = append([]MemoryEntry{merged}, rest...)
	// 年度摘要超过 8 条（≈8年）→ 最老的降级为低重要（接近遗忘）
	if len(ms.archive[actor]) > 8 {
		n := len(ms.archive[actor])
		ms.archive[actor][0].Importance = 0.3 // 最老一条几乎遗忘
		_ = n
	}
}

// Recent 取最近 N 条记忆（按时间）
func (ms *MemoryStore) Recent(actor string, n int) []MemoryEntry {
	if ms == nil {
		return nil
	}
	entries := ms.memories[actor]
	if len(entries) <= n {
		return entries
	}
	return entries[len(entries)-n:]
}

// All 返回某角色全部记忆（反思用）
func (ms *MemoryStore) All(actor string) []MemoryEntry {
	if ms == nil {
		return nil
	}
	return ms.memories[actor]
}

// Actors 返回有记忆的角色列表
func (ms *MemoryStore) Actors() []string {
	if ms == nil {
		return nil
	}
	out := make([]string, 0, len(ms.memories))
	for a := range ms.memories {
		out = append(out, a)
	}
	return out
}

// Reflect 周期性反思（Smallville 范式）：回顾记忆流，LLM 提炼高阶洞察，写回记忆
func (ms *MemoryStore) Reflect(ctx context.Context, c *LLMClient, actor string) string {
	ctx = llm.WithSpan(ctx, "记忆反思")
	entries := ms.Recent(actor, 30)
	if len(entries) < 5 {
		return ""
	}
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("- [Day%d] %s\n", e.Day, e.Content))
	}
	system := `你是{actor}。回顾最近经历，提炼 1~2 条"你学到的高阶认知"（对自己处境的判断、对他人的态度、对世界异常的直觉）。
要求：必须基于下面列出的亲身经历，用第一人称；输出严格 JSON：{"reflections":["...","..."]}（每条不超过50字）`
	system = strings.ReplaceAll(system, "{actor}", actor)
	raw, err := c.Complete(ctx, system, sb.String())
	if err != nil {
		return ""
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return ""
	}
	var resp struct {
		Reflections []string `json:"reflections"`
	}
	if json.Unmarshal([]byte(jsonStr), &resp) != nil {
		return ""
	}
	var out []string
	for _, r := range resp.Reflections {
		r = strings.TrimSpace(r)
		if r != "" {
			ms.AddDay(actor, r, "reflection", 0.9, 0)
			out = append(out, r)
		}
	}
	return strings.Join(out, "；")
}

// BatchConsolidate 批量记忆巩固：一次 LLM 调用整理多个角色的旧记忆，再分发到各自 Archive
// 省 token：n 个角色只需 1 次调用（原来 CapMemory 每人 1 次）。触发条件同 CapMemory：超过 maxN 条才巩固
func (ms *MemoryStore) BatchConsolidate(ctx context.Context, c *LLMClient, actors []string, maxN int) map[string]int {
	ctx = llm.WithSpan(ctx, "记忆巩固")
	consolidated := map[string]int{}
	if ms == nil || c == nil || len(actors) == 0 {
		return consolidated
	}
	type batchItem struct {
		actor string
		old   []MemoryEntry
		keep  []MemoryEntry
	}
	var items []batchItem
	for _, a := range actors {
		entries := ms.memories[a]
		if len(entries) <= maxN {
			continue
		}
		keep := len(entries) / 3 // 保留最近 1/3
		old := entries[:len(entries)-keep]
		if len(old) < 10 {
			continue // 太少不值得合并
		}
		items = append(items, batchItem{a, old, entries[len(entries)-keep:]})
	}
	if len(items) == 0 {
		return consolidated
	}
	// 组装批量 prompt（每人最多 20 条，控 token 总量）
	var sb strings.Builder
	sb.WriteString("你是记忆整理员。下面是多个角色的近期记忆，请为每个角色分别提炼 1~3 条精炼摘要（保留关键事件、人物关系、伏笔、性格变化），用第一人称。输出严格 JSON 对象：{\"角色名\":\"摘要1；摘要2\"}\n\n")
	for i, it := range items {
		sb.WriteString(fmt.Sprintf("【角色%d】%s\n", i+1, it.actor))
		max := 20
		if len(it.old) > max {
			it.old = it.old[len(it.old)-max:]
		}
		for _, e := range it.old {
			sb.WriteString(fmt.Sprintf("- Day%d: %s\n", e.Day, e.Content))
		}
		sb.WriteString("\n")
	}
	raw, err := c.CompleteTier(ctx, "fast", "你是记忆整理员，严格按 JSON 格式输出。", sb.String())
	results := map[string]string{}
	if err == nil {
		jsonStr := llm.ExtractJSON(raw)
		_ = json.Unmarshal([]byte(jsonStr), &results)
	}
	// 分发：各角色摘要进 Archive，Working 截断保留最近 1/3
	for _, it := range items {
		summary := strings.TrimSpace(results[it.actor])
		if summary == "" {
			// LLM 失败/缺该角色：规则式保底——保留重要性最高的前 5 条
			sorted := append([]MemoryEntry(nil), it.old...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i].Importance > sorted[j].Importance })
			var parts []string
			for i := 0; i < len(sorted) && i < 5; i++ {
				parts = append(parts, sorted[i].Content)
			}
			summary = strings.Join(parts, "；")
		}
		lastDay := it.old[len(it.old)-1].Day
		ms.archive[it.actor] = append(ms.archive[it.actor], MemoryEntry{
			Day: lastDay, Time: time.Now().Format("01-02 15:04"),
			Actor: it.actor, Content: "记忆巩固·Day" + strconv.Itoa(lastDay) + "：" + summary,
			Kind: "summary", Importance: 0.9,
		})
		ms.memories[it.actor] = it.keep
		consolidated[it.actor] = len(it.old)
	}
	return consolidated
}

// BatchReflect 批量反思：一次 LLM 调用产出多个角色的高阶认知，写回各自记忆（省 token）
func (ms *MemoryStore) BatchReflect(ctx context.Context, c *LLMClient, actors []string) map[string]string {
	ctx = llm.WithSpan(ctx, "记忆反思")
	out := map[string]string{}
	if ms == nil || c == nil || len(actors) == 0 {
		return out
	}
	type item struct {
		actor   string
		entries []MemoryEntry
	}
	var items []item
	for _, a := range actors {
		entries := ms.Recent(a, 15)
		if len(entries) < 5 {
			continue
		}
		items = append(items, item{a, entries})
	}
	if len(items) == 0 {
		return out
	}
	var sb strings.Builder
	sb.WriteString("下面是多个角色的近期经历，请为每个角色分别提炼 1~2 条第一人称的高阶认知（对处境的判断/对他人的态度/对世界异常的直觉），每条不超过 50 字。输出严格 JSON 对象：{\"角色名\":[\"认知1\",\"认知2\"]}\n\n")
	for i, it := range items {
		sb.WriteString(fmt.Sprintf("【角色%d】%s：\n", i+1, it.actor))
		for _, e := range it.entries {
			sb.WriteString(fmt.Sprintf("- Day%d: %s\n", e.Day, e.Content))
		}
		sb.WriteString("\n")
	}
	raw, err := c.CompleteTier(ctx, "fast", "你是心理洞察师，严格按 JSON 格式输出。", sb.String())
	if err != nil {
		return out
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return out
	}
	var resp map[string][]string
	if json.Unmarshal([]byte(jsonStr), &resp) != nil {
		return out
	}
	for _, it := range items {
		var got []string
		for _, r := range resp[it.actor] {
			r = strings.TrimSpace(r)
			if r != "" {
				ms.AddDay(it.actor, r, "reflection", 0.9, 0)
				got = append(got, r)
			}
		}
		if len(got) > 0 {
			out[it.actor] = strings.Join(got, "；")
		}
	}
	return out
}

// 转成 prompt 文本（主角/ NPC 注入用）
func formatMemories(entries []MemoryEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, e := range entries {
		dayStr := ""
		if e.Day > 0 {
			dayStr = fmt.Sprintf("Day%d ", e.Day)
		}
		sb.WriteString(fmt.Sprintf("- %s%s\n", dayStr, e.Content))
	}
	return strings.TrimSpace(sb.String())
}

func splitKeywords(s string) []string {
	if s == "" {
		return nil
	}
	// 中文按 2 字滑动窗口切词，简单够用
	runes := []rune(s)
	var words []string
	seen := map[string]bool{}
	for i := 0; i < len(runes)-1; i++ {
		w := string(runes[i : i+2])
		if !seen[w] {
			seen[w] = true
			words = append(words, w)
		}
	}
	return words
}

func parseMemTime(s string) time.Time {
	t, err := time.Parse("01-02 15:04", s)
	if err != nil {
		return time.Now()
	}
	// 补年份
	return t.AddDate(time.Now().Year()-t.Year(), 0, 0)
}

// 记忆文件路径（放在 worlds 目录）
func (s *Simulator) memoryPath() string { return filepath.Join(s.worldDir, "agents_memory.json") }

// ---------- 记忆膨胀治理（P2：长程记忆不爆，10年跨度可用） ----------
// 方案（综合 mem0/TiMEM/openclaw-auto-dream/hippo 业界共识）：
//   1. 分层：工作记忆（近30天完整）+ 存档摘要（更早按月合并成1-3条）
//   2. 睡眠巩固：每月自动把本月记忆 LLM 提炼成月度摘要，替换原始细碎记忆
//   3. 检索强化（hippo）：被想起的记忆 importance 提升（回忆会加深印象）
//   4. 上限保护：单角色记忆超限强制合并最老的

// WorkingWindow 工作记忆窗口（天）
const WorkingWindow = 30

// CapMemory 上限保护：某角色记忆超过 maxN 条时，把最老的合并成摘要
func (ms *MemoryStore) CapMemory(ctx context.Context, c *LLMClient, actor string, maxN int) {
	if ms == nil {
		return
	}
	entries := ms.memories[actor]
	if len(entries) <= maxN {
		return
	}
	// 把最老的 2/3 合并（保留最近 1/3）
	keep := len(entries) / 3
	old := entries[:len(entries)-keep]
	if len(old) < 10 {
		return // 太少不值得合并
	}
	summary := ms.summarizeEntries(ctx, c, actor, old, "记忆巩固·Day"+strconv.Itoa(old[len(old)-1].Day))
	if summary == "" {
		// LLM失败：粗暴截断最老的（保底）
		ms.memories[actor] = entries[len(entries)-maxN:]
		return
	}
	ms.memories[actor] = append([]MemoryEntry{{
		Day: old[len(old)-1].Day, Time: old[len(old)-1].Time,
		Actor: actor, Content: summary, Kind: "archive", Importance: 0.7,
	}}, entries[len(entries)-keep:]...)
}

// MonthlyConsolidate 睡眠巩固：每月把该角色记忆合并成月度摘要（TiMEM/openclaw 理念）
func (ms *MemoryStore) MonthlyConsolidate(ctx context.Context, c *LLMClient, actor string, currentDay int) {
	if ms == nil || c == nil {
		return
	}
	// 只处理"上一个月"的记忆（当前月结束后合并）
	monthEnd := currentDay - 1
	monthStart := monthEnd - WorkingWindow + 1
	var month []MemoryEntry
	var rest []MemoryEntry
	for _, e := range ms.memories[actor] {
		if e.Kind == "archive" || e.Kind == "reflection" {
			rest = append(rest, e) // 摘要/反思保留
			continue
		}
		if e.Day >= monthStart && e.Day <= monthEnd && e.Day > 0 {
			month = append(month, e)
		} else {
			rest = append(rest, e)
		}
	}
	if len(month) < 5 {
		return // 太少不合并
	}
	label := DateLabel(monthStart) + "～" + DateLabel(monthEnd)
	summary := ms.summarizeEntries(ctx, c, actor, month, label)
	if summary == "" {
		return
	}
	ms.memories[actor] = append([]MemoryEntry{{
		Day: monthEnd, Time: month[len(month)-1].Time,
		Actor: actor, Content: summary, Kind: "archive", Importance: 0.8,
	}}, rest...)
}

// summarizeEntries LLM 把一组记忆提炼成1-3条摘要（保留人物/地点/关系/伏笔关键信息）
func (ms *MemoryStore) summarizeEntries(ctx context.Context, c *LLMClient, actor string, entries []MemoryEntry, label string) string {
	ctx = llm.WithSpan(ctx, "记忆摘要")
	if len(entries) < 5 {
		return ""
	}
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("- [Day%d] %s\n", e.Day, e.Content))
	}
	system := `你是{actor}的记忆整理系统。下面是你一段时期内的经历记录（可能很碎）。
请提炼成 2~3 条"这段时期的浓缩记忆"：
1. 保留对剧情重要的人和事：重要事件/关键对话/关系变化/未解谜团/地点变化
2. 丢弃琐碎日常（吃饭/天气/普通工作）——除非对剧情有意义
3. 用第一人称，每条不超过60字
4. 输出严格 JSON：{"summary":["...","..."]}`
	system = strings.ReplaceAll(system, "{actor}", actor)
	raw, err := c.CompleteTier(ctx, "fast", system, sb.String())
	if err != nil {
		return ""
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return ""
	}
	var resp struct {
		Summary []string `json:"summary"`
	}
	if json.Unmarshal([]byte(jsonStr), &resp) != nil {
		return ""
	}
	var out []string
	for _, s := range resp.Summary {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return label + "：" + strings.Join(out, "；")
}

// StrengthenRetrieval 检索强化（hippo 理念）：被召回的记忆重要性微升
func (ms *MemoryStore) StrengthenRetrieval(actor string, recalled []MemoryEntry) {
	if ms == nil {
		return
	}
	entries := ms.memories[actor]
	for i := range entries {
		for _, r := range recalled {
			if entries[i].Time == r.Time && entries[i].Content == r.Content {
				entries[i].Importance = clamp(entries[i].Importance+0.01, 0, 1)
			}
		}
	}
}