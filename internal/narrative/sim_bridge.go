package narrative

import (
	"strings"
	"worldsim/internal/engine"
	"worldsim/internal/sim"
	"worldsim/internal/worldbook"
)

// ---------- 从 Simulator 拉取输入 ----------

// SimDataSource 剧情引擎需要从模拟器读取的数据源（接口化，便于测试）。
type SimDataSource interface {
	ArcBook() []sim.ArcEntry
	Chronicle() []sim.ChronicleEntry
	Thinkings() map[int]string
	ForeshadowsAll() map[string]sim.Foreshadow
	HeroName() string
}

// BuildInput 从模拟器 + 引擎状态组装剧情引擎输入。
// hero 主角名（空则用 sim.HeroName()）；entities 从 engine.State() 取。
func BuildInput(simS SimDataSource, entities map[string]engine.Entity, wb *worldbook.Worldbook, dir Direction) *Input {
	hero := simS.HeroName()
	return &Input{
		ArcBook:     simS.ArcBook(),
		Chronicle:   simS.Chronicle(),
		Thinkings:   simS.Thinkings(),
		Foreshadows: simS.ForeshadowsAll(),
		Entities:    entities,
		WB:          wb,
		Hero:        hero,
		Direction:   dir,
	}
}

// ---------- 编年史 → 段落分组 ----------

// GroupedArc 段落及其编年史素材。
type GroupedArc struct {
	Arc     sim.ArcEntry         // 段落本身
	Entries []sim.ChronicleEntry // 归属该段落的编年史条目
	Days    []int                // 有记录的天
	Loose   []sim.ChronicleEntry // 松散条目（段落间隙）
}

// GroupArcs 把编年史按段落分组，返回按开段日排序的段落组。
// loose 里是所有不属于任何段落的条目（世界初期/段落间隙），调用方决定怎么归并。
func GroupArcs(in *Input) ([]GroupedArc, []sim.ChronicleEntry) {
	grouped, loose := groupByArc(in.Chronicle, in.ArcBook)
	// 按段落 Num 排序（Num 即创建顺序）
	arcs := in.ArcBook
	var out []GroupedArc
	seen := map[int]bool{}
	for _, a := range arcs {
		if seen[a.Num] {
			continue
		}
		seen[a.Num] = true
		entries := grouped[a.Num]
		out = append(out, GroupedArc{
			Arc:     a,
			Entries: entries,
			Days:    daysOf(entries),
		})
	}
	// 没有段落时的兜底：全部算松散
	if len(out) == 0 {
		return nil, in.Chronicle
	}
	return out, loose
}

// ---------- 角色状态卡 ----------

// buildCharacterCard 从实体状态构造角色卡（含 stats/健康/位置/关系）。
func buildCharacterCard(name string, entities map[string]engine.Entity, hero string) CharacterCard {
	ent, ok := entities[name]
	if !ok {
		return CharacterCard{Name: name, Role: "背景人物"}
	}
	role := "配角"
	if name == hero {
		role = "主角"
	}
	rel := ""
	if hero != "" && hero != name {
		if v, ok := ent.Relationship[hero]; ok {
			rel = formatRel(v)
		}
	}
	recent := ""
	if ent.Extra != nil {
		if s, ok := ent.Extra["recent"].(string); ok && s != "" {
			recent = s
		}
	}
	return CharacterCard{
		Name:     name,
		Role:     role,
		Stats:    ent.Stats,
		Health:   ent.Health,
		Location: ent.Location,
		Relation: rel,
		Recent:   recent,
	}
}

// collectChars 从编年史条目 Tags + 事件中收集登场角色名（按出现频次去重排序）。
func collectChars(entries []sim.ChronicleEntry, entities map[string]engine.Entity, hero string) []string {
	cnt := map[string]int{}
	for _, e := range entries {
		for _, t := range e.Tags {
			// 只认实体存在的名字（角色），过滤流程标签
			if _, ok := entities[t]; ok {
				cnt[t]++
			}
		}
	}
	if hero != "" {
		cnt[hero] += 100 // 主角必然登场
	}
	// 按频次排序
	var names []string
	for n := range cnt {
		names = append(names, n)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if cnt[names[j]] > cnt[names[i]] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}

// buildCharCards 批量构造角色卡（只含登场角色）。
func buildCharCards(names []string, entities map[string]engine.Entity, hero string) []CharacterCard {
	var cards []CharacterCard
	for _, n := range names {
		c := buildCharacterCard(n, entities, hero)
		cards = append(cards, c)
	}
	return cards
}

func formatRel(v float64) string {
	switch {
	case v >= 0.7:
		return "亲近"
	case v >= 0.3:
		return "友好"
	case v >= 0.05:
		return "一般"
	case v >= -0.05:
		return "中立"
	case v >= -0.3:
		return "冷淡"
	default:
		return "敌对"
	}
}

// formatCards 角色卡文本（注入剧本）。
func formatCards(cards []CharacterCard) string {
	var sb strings.Builder
	for _, c := range cards {
		sb.WriteString("· " + c.Name + "（" + c.Role + "）")
		if c.Location != "" {
			sb.WriteString(" @" + c.Location)
		}
		if len(c.Stats) > 0 {
			sb.WriteString(" 属性[")
			first := true
			for k, v := range c.Stats {
				if !first {
					sb.WriteString(" ")
				}
				first = false
				sb.WriteString(k + ":" + anyStr(v))
			}
			sb.WriteString("]")
		}
		if c.Relation != "" {
			sb.WriteString(" 关系:" + c.Relation)
		}
		if c.Recent != "" {
			sb.WriteString(" 近况:" + c.Recent)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func anyStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int(t)) {
			return itoa(int(t))
		}
		return ftoa(t)
	case int:
		return itoa(t)
	default:
		return ""
	}
}

func ftoa(f float64) string {
	if f == float64(int(f)) {
		return itoa(int(f))
	}
	// 简单两位小数
	s := strings.TrimRight(strings.TrimRight(sprintF(f), "0"), ".")
	return s
}

func sprintF(f float64) string {
	b := make([]byte, 0, 16)
	b = appendFloat(b, f)
	return string(b)
}

func appendFloat(b []byte, f float64) []byte {
	// 最简实现：整数部分 + 两位小数
	neg := f < 0
	if neg {
		f = -f
		b = append(b, '-')
	}
	ip := int(f)
	b = appendInt(b, ip)
	fr := int((f-float64(ip))*100 + 0.5)
	if fr > 0 {
		b = append(b, '.')
		if fr < 10 {
			b = append(b, '0')
		}
		b = appendInt(b, fr)
	}
	return b
}

func appendInt(b []byte, n int) []byte {
	if n == 0 {
		return append(b, '0')
	}
	var tmp [20]byte
	i := len(tmp)
	for n > 0 {
		i--
		tmp[i] = byte('0' + n%10)
		n /= 10
	}
	return append(b, tmp[i:]...)
}
