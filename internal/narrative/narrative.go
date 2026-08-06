// Package narrative 剧情引擎：把世界模拟的编年史/段落账本/伏笔转化为"可写作的小说剧本"。
//
// 核心原则：
//   - 章节骨架 = 模拟层 GM 已规划的 arcBook 段落（一个段落=一个网文情节单元），
//     不再按固定天数切章——LLM 按事件密度/戏剧强度判断段落拆并。
//   - 剧情方向可控：用户可指定主线/爽点密度/文风倾向，注入规划 prompt。
//   - 场景剧本化：每个章节 → 节拍(Beat) → 场景剧本(SceneScript)，novel 包只做纯写作。
//   - 角色状态卡：每个场景前注入登场角色的 stats/关系/记忆，保证一致性。
package narrative

import (
	"sort"
	"strings"
	"worldsim/internal/config"
	"worldsim/internal/engine"
	"worldsim/internal/sim"
	"worldsim/internal/worldbook"
)

// ---------- 剧情方向（用户可控） ----------

// Direction 用户指定的剧情方向。所有字段可选，空值=让 LLM 自行判断。
type Direction struct {
	MainLine      string `json:"main_line,omitempty"`      // 主线/核心冲突方向（如"追查某件失踪案的真相"）
	PayoffDensity string `json:"payoff_density,omitempty"` // 爽点密度：low | normal | high（空=normal）
	StyleTilt     string `json:"style_tilt,omitempty"`     // 文风倾向（如"偏悬疑压抑"、"轻松诙谐"）
	FocusArcs     []int  `json:"focus_arcs,omitempty"`     // 优先展开的段落编号（0=不限制）
	MaxChapters   int    `json:"max_chapters,omitempty"`   // 目标章节数上限（0=不限制）
}

// ---------- 剧情单元（= 最终章节方案） ----------

// StoryUnit 一个剧情单元 = 计划中的一章（由 arcBook 段落 + LLM 细化得到）。
type StoryUnit struct {
	Num      int      `json:"num"`       // 章号（1-based）
	ArcRef   int      `json:"arc_ref"`   // 关联的 arcBook 段落编号（0=自由事件/过渡章）
	Title    string   `json:"title"`     // 章节标题（起悬念/画面感）
	Days     []int    `json:"days"`      // 覆盖的模拟天数（不固定数量，由叙事决定）
	DayStart int      `json:"day_start"` // 起始天
	DayEnd   int      `json:"day_end"`   // 结束天
	Goal     string   `json:"goal"`      // 本章叙事目标（主角要达成什么）
	Villain  string   `json:"villain"`   // 本章反派动作/压迫（若有）
	Payoff   string   `json:"payoff"`    // 本章爽点/收获（若有）
	Hook     string   `json:"hook"`      // 结尾悬念（具体到谁/什么/在哪）
	Chars    []string `json:"chars"`     // 本章登场角色
	// 内部（不入 JSON 展示）
	Milestones []string `json:"-"` // 原段落里程碑（拆章时分配）
	TimeHint   string   `json:"-"` // 原段落时间提示
}

// ---------- 节拍 ----------

// Beat 章节内的一个剧情节拍。
type Beat struct {
	Type      string   `json:"type"`       // setup | conflict | turn | reveal | payoff | hook
	Summary   string   `json:"summary"`    // 本拍内容摘要（写手据此展开场景）
	Location  string   `json:"location"`   // 场景地点（从世界背景取）
	Chars     []string `json:"chars"`      // 本拍登场角色
	SourceDay int      `json:"source_day"` // 对应素材天（-1=写手自由发挥）
}

// ---------- 场景剧本 ----------

// SceneScript 一个可写作的场景剧本（novel 写手直接照此写正文）。
type SceneScript struct {
	Num        int             `json:"num"`         // 场景序号（章节内）
	BeatType   string          `json:"beat_type"`   // 对应节拍类型
	Goal       string          `json:"goal"`        // 本场景目标（角色要达成什么）
	Conflict   string          `json:"conflict"`    // 本场景冲突（谁 vs 什么）
	Characters []CharacterCard `json:"characters"`  // 角色状态卡（只含登场角色）
	Dialogue   string          `json:"dialogue"`    // 对话要点（交锋/潜台词）
	Turn       string          `json:"turn"`        // 转折（场景内的变化）
	Hook       string          `json:"hook"`        // 场景结尾钩子
	SourceDays []int           `json:"source_days"` // 对应素材天
}

// CharacterCard 角色状态卡（每场景注入，保证一致性且省 token）。
type CharacterCard struct {
	Name     string         `json:"name"`
	Role     string         `json:"role"`   // 主角/核心配角/龙套等
	Stats    map[string]any `json:"stats"`  // 世界书驱动的属性集
	Health   float64        `json:"health"` // 健康度（0=死亡）
	Location string         `json:"location"`
	Relation string         `json:"relation"` // 与主角的关系（若有）
	Recent   string         `json:"recent"`   // 最近记忆/状态一句话
}

// ---------- 规划结果 ----------

// Plan 剧情引擎一次规划的完整结果。
type Plan struct {
	Direction Direction       `json:"direction"`
	Units     []StoryUnit     `json:"units"`   // 章节方案
	Scripts   [][]SceneScript `json:"scripts"` // 每章的剧本（与 Units 对齐）
}

// ---------- 素材输入 ----------

// Input 剧情引擎的输入（从 Simulator 拉取）。
type Input struct {
	ArcBook     []sim.ArcEntry            // 段落账本（章节骨架）
	Chronicle   []sim.ChronicleEntry      // 编年史事件流
	Thinkings   map[int]string            // 每日主角内心
	Foreshadows map[string]sim.Foreshadow // 伏笔账本
	Entities    map[string]engine.Entity  // 实体状态（角色卡）
	WB          *worldbook.Worldbook      // 世界书（时间尺度/设定）
	Hero        string                    // 主角名
	Direction   Direction                 // 剧情方向
	LLM         *config.APIConfig         // LLM 配置（nil=降级规则分章）
}

// ---------- 编年史按段落归属 ----------

// groupByArc 把编年史条目按 Day 范围归属到段落。
// 规则：条目 Day ∈ [arc.Day, 下一段落.Day) 归该段落；段落完成后到新段落开启前的松散日归"间隙"。
// 返回：arcIdx -> entries；以及不属于任何段落的松散条目。
func groupByArc(chronicle []sim.ChronicleEntry, arcs []sim.ArcEntry) (map[int][]sim.ChronicleEntry, []sim.ChronicleEntry) {
	if len(arcs) == 0 {
		return nil, chronicle
	}
	// 段落按开段日排序
	sorted := make([]sim.ArcEntry, len(arcs))
	copy(sorted, arcs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Day < sorted[j].Day })

	grouped := map[int][]sim.ChronicleEntry{}
	var loose []sim.ChronicleEntry
	for _, e := range chronicle {
		belong := -1
		for i := 0; i < len(sorted); i++ {
			if e.Day >= sorted[i].Day {
				// 属于当前段落，除非下一个段落已经开启
				if i+1 < len(sorted) && e.Day >= sorted[i+1].Day {
					continue
				}
				belong = sorted[i].Num
				break
			}
		}
		if belong > 0 {
			grouped[belong] = append(grouped[belong], e)
		} else {
			loose = append(loose, e)
		}
	}
	return grouped, loose
}

// daysOf 提取条目的天数集合（排序去重）。
func daysOf(entries []sim.ChronicleEntry) []int {
	set := map[int]bool{}
	for _, e := range entries {
		set[e.Day] = true
	}
	var days []int
	for d := range set {
		days = append(days, d)
	}
	sort.Ints(days)
	return days
}

// entryTexts 把编年史条目格式化成"DayN：内容"文本。
func entryTexts(entries []sim.ChronicleEntry) string {
	byDay := map[int][]string{}
	var days []int
	for _, e := range entries {
		if _, ok := byDay[e.Day]; !ok {
			days = append(days, e.Day)
		}
		byDay[e.Day] = append(byDay[e.Day], e.Content)
	}
	sort.Ints(days)
	var sb strings.Builder
	for _, d := range days {
		sb.WriteString("Day" + itoa(d) + "：" + strings.Join(byDay[d], "；") + "\n")
	}
	return sb.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
