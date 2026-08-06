package sim

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"worldsim/internal/engine"
)

// ---------- Token 省钱：状态精简（LLM 只需核心字段，砍掉 extra 大档案/长记忆） ----------

// slimHeroJSON 分级精简实体：主角 + 重要配角全量（有 stats 能力值或关系网的，事件生成需要
// 知道他们的境界/能力/状态才能生成合理事件），纯龙套（无 stats 无关系网的路人）才用摘要。
// 平衡 token 与信息量：全量 5.4KB vs 摘要 2.4KB vs 混合 4.6KB——混合几乎不损失信息，
// 只把"一面之缘的路人"降为摘要（他们不参与主线，事件 Agent 只需要知道"存在"）。
//
// ★ 前缀缓存优化：实体按"稳定→易变"排序输出（不用 map，map 会按名字乱序）。
// 龙套（无 stats/rel，几乎不变）→ 重要配角（有档案，偶尔变）→ 主角（每天变）放最后。
// 这样每天的 heroJSON 变化只发生在尾部，前面大段稳定内容命中前缀缓存（省 token 不损失信息）。
func slimHeroJSON(heroName string, ents map[string]engine.Entity) string {
	type item struct {
		name  string
		entry map[string]any
		cls   int // 0=龙套(稳定) 1=重要配角 2=主角(易变)
	}
	items := make([]item, 0, len(ents))
	for name, e := range ents {
		if e.Status == "departed" {
			continue
		}
		isHero := name == heroName || heroName == ""
		// 重要配角判定：有 stats（能力/境界等世界书属性）或关系网——参与主线的角色
		important := isHero || len(e.Stats) > 0 || len(e.Relationship) > 0
		entry := map[string]any{
			"location": e.Location,
			"job":      e.Job,
			"status":   e.Status,
		}
		cls := 0
		if isHero {
			cls = 2
		} else if important {
			cls = 1
		}
		if important {
			// 主角/重要配角：全量（含 money/health/关系/能力）
			entry["money"] = e.Money
			entry["health"] = e.Health
			if len(e.Relationship) > 0 {
				entry["rel"] = e.Relationship
			}
			if len(e.Stats) > 0 {
				entry["stats"] = e.Stats
			}
		}
		items = append(items, item{name: name, entry: entry, cls: cls})
	}
	// 排序：cls 升序（龙套→配角→主角），同 cls 按名字（保持稳定、可复现）
	sort.Slice(items, func(i, j int) bool {
		if items[i].cls != items[j].cls {
			return items[i].cls < items[j].cls
		}
		return items[i].name < items[j].name
	})
	// 有序拼接 JSON 对象（不能过 map——json.Marshal 会按 key 重排，破坏稳定性排序）
	var sb strings.Builder
	sb.WriteString("{\n")
	for i, it := range items {
		if i > 0 {
			sb.WriteString(",\n")
		}
		nb, _ := json.Marshal(it.name)
		vb, _ := json.Marshal(it.entry)
		sb.WriteString("  ")
		sb.Write(nb)
		sb.WriteString(": ")
		sb.Write(vb)
	}
	sb.WriteString("\n}")
	return sb.String()
}

// compactState 精简世界状态：world_level（势力/地点/近期事件/张力）+ 稳定序实体。
// 用于 WorldImpactLLM / skipSummaryLLM 等"只需要大致背景"的调用，避免全量 marshal 浪费 token。
// 实体复用 slimHeroJSON（分级+稳定序）：与事件生成同构，缓存前缀友好。
func compactState(st *engine.WorldState, heroName string) string {
	var sb strings.Builder
	sb.WriteString(`{"day":`)
	sb.WriteString(strconv.Itoa(st.Day))
	sb.WriteString(`,"weather":`)
	wb, _ := json.Marshal(st.Weather)
	sb.Write(wb)
	sb.WriteString(`,"world_level":{"tension":`)
	sb.WriteString(strconv.FormatFloat(st.WorldLevel.Tension, 'f', 2, 64))
	sb.WriteString(`,"factions":`)
	fb, _ := json.Marshal(st.WorldLevel.Factions)
	sb.Write(fb)
	sb.WriteString(`,"locations":`)
	lb, _ := json.Marshal(st.WorldLevel.Locations)
	sb.Write(lb)
	sb.WriteString(`,"global_events":`)
	gb, _ := json.Marshal(lastN(st.WorldLevel.GlobalEvents, 5))
	sb.Write(gb)
	sb.WriteString(`},"entities":`)
	sb.WriteString(slimHeroJSON(heroName, st.Entities))
	sb.WriteString(`}`)
	return sb.String()
}

func lastN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
