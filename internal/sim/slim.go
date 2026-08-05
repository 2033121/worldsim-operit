package sim

import (
	"encoding/json"

	"worldsim/internal/engine"
)

// ---------- Token 省钱：状态精简（LLM 只需核心字段，砍掉 extra 大档案/长记忆） ----------

// slimEntities 精简实体：只保留决策所需字段（location/job/money/status/关系值），
// 丢弃 Extra（persona_sheet 人设卡、记忆等大字段——这些按需单独注入）
func slimEntities(ents map[string]engine.Entity) map[string]any {
	out := map[string]any{}
	for name, e := range ents {
		if e.Status == "departed" {
			continue
		}
		entry := map[string]any{
			"location": e.Location,
			"job":      e.Job,
			"money":    e.Money,
			"health":   e.Health,
			"status":   e.Status,
			"rel":      e.Relationship,
		}
		// 世界书驱动的动态属性集（属性名随世界变化，引擎不预设）
		if len(e.Stats) > 0 {
			entry["stats"] = e.Stats
		}
		out[name] = entry
	}
	return out
}

// compactState 精简世界状态：world_level（势力/地点/近期事件/张力）+ 精简实体。
// 用于 WorldImpactLLM 等"只需要大致背景"的调用，避免全量 marshal 浪费 token。
func compactState(st *engine.WorldState) string {
	out := map[string]any{
		"day":     st.Day,
		"weather": st.Weather,
		"world_level": map[string]any{
			"tension":      st.WorldLevel.Tension,
			"factions":     st.WorldLevel.Factions,
			"locations":    st.WorldLevel.Locations,
			"global_events": lastN(st.WorldLevel.GlobalEvents, 5),
		},
		"entities": slimEntities(st.Entities),
	}
	b, _ := json.MarshalIndent(out, "", " ")
	return string(b)
}

func lastN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
