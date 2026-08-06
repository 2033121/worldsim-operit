package sim

// ---------- Phase 2 机制层：玩家状态面板 + 行动选项 ----------
//
// 全部由世界现有数据（实体/关系/段落/编年史）规则化生成，零 LLM 零成本：
//   - 状态面板：主角能力/健康/资产/当前位置/当前段落目标/顶级关系
//   - 行动选项：围绕当前形势的 4 个可选行动（调查/交际/积累/休整或探索）
//
// 零污染：行动模板只抽象描述，具体内容由世界书数据填充。

import (
	"encoding/json"
	"sort"
	"strings"
)

// PlayerState 玩家面板完整数据（/api/world/player/state 返回）
type PlayerState struct {
	World   string             `json:"world"`
	Day     int                `json:"day"`
	Hero    PlayerHeroState    `json:"hero"`
	Arc     PlayerArcState     `json:"arc"`
	RelTop  []PlayerRelEntry   `json:"relationships"`
	Recent  []string           `json:"recent_events"`
	Actions []PlayerAction     `json:"actions"`
	Intents []PlayerIntent     `json:"intents"`
	Stats   map[string]int     `json:"stats"`
}

// PlayerHeroState 主角状态卡
type PlayerHeroState struct {
	Name         string         `json:"name"`
	Location     string         `json:"location"`
	Health       float64        `json:"health"`
	Money        float64        `json:"money"`
	Job          string         `json:"job"`
	Status       string         `json:"status"`
	Capabilities map[string]any `json:"capabilities"` // stats 能力值（属性名由世界书决定）
}

// PlayerArcState 当前段落目标
type PlayerArcState struct {
	Name   string   `json:"name"`
	Goal   string   `json:"goal"`
	Villain string  `json:"villain"`
	Milestones []string `json:"milestones"`
}

// PlayerRelEntry 一条关系（角色名 + 数值 + 档位）
type PlayerRelEntry struct {
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Rank   string  `json:"rank"`
}

// PlayerAction 一个可选行动（点击→发指令）
type PlayerAction struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // investigate | social | cultivate | rest | explore
	Icon   string `json:"icon"`
	Label  string `json:"label"`
	Intent string `json:"intent"` // 点击后实际发出的玩家指令
}

// PlayerState 生成玩家面板（世界数据规则化，零 LLM）
func (s *Simulator) PlayerState() *PlayerState {
	st := &PlayerState{
		World:   s.worldName(),
		Day:     s.day,
		RelTop:  []PlayerRelEntry{},
		Recent:  []string{},
		Actions: []PlayerAction{},
		Stats:   map[string]int{"pending": 0, "consumed": 0},
	}
	ints := s.PlayerIntents()
	for _, it := range ints {
		if it.Status == "pending" {
			st.Stats["pending"]++
		} else {
			st.Stats["consumed"]++
		}
	}
	st.Intents = ints

	// 主角状态卡
	ents := s.engine.State().Entities
	if ent, ok := ents[s.heroName]; ok {
		h := PlayerHeroState{
			Name:     s.heroName,
			Location: ent.Location,
			Health:   ent.Health,
			Money:    ent.Money,
			Job:      ent.Job,
			Status:   strings.TrimSpace(ent.Status),
		}
		if ent.Stats != nil {
			h.Capabilities = ent.Stats
		} else if st, ok := ent.Extra["stats"].(map[string]any); ok {
			h.Capabilities = st
		}
		st.Hero = h
	}

	// 当前段落（最后一个 open）
	for i := len(s.arcBook) - 1; i >= 0; i-- {
		if s.arcBook[i].Status == "open" {
			st.Arc = PlayerArcState{
				Name: s.arcBook[i].ArcName, Goal: s.arcBook[i].Goal,
				Villain: s.arcBook[i].Villain, Milestones: s.arcBook[i].Milestones,
			}
			break
		}
	}

	// 顶级关系：主角 relationship 值最高的前 5
	if ent, ok := ents[s.heroName]; ok && ent.Relationship != nil {
		type rp struct{ name string; v float64 }
		var list []rp
		for name, v := range ent.Relationship {
			if v >= 0.1 { // 至少相识
				list = append(list, rp{name, v})
			}
		}
		sort.Slice(list, func(i, j int) bool { return list[i].v > list[j].v })
		if len(list) > 5 {
			list = list[:5]
		}
		for _, r := range list {
			st.RelTop = append(st.RelTop, PlayerRelEntry{
				Name: r.name, Value: round2(r.v),
				Rank: s.relRank(s.heroName, r.name),
			})
		}
	}

	// 最近事件（编年史最后 3 条 EVENT）
	n := 0
	for i := len(s.chronicle) - 1; i >= 0 && n < 3; i-- {
		if s.chronicle[i].Kind == "EVENT" || strings.Contains(s.chronicle[i].Content, "「") {
			st.Recent = append(st.Recent, s.chronicle[i].Content)
			n++
		}
	}

	// 行动选项（世界数据驱动）
	st.Actions = s.buildActions()
	return st
}

// relRank 关系档位（从 extra.rel_status_{对方} 读取，读不到按数值兜底）
func (s *Simulator) relRank(hero, other string) string {
	if ent, ok := s.engine.State().Entities[hero]; ok {
		if v, ok2 := ent.Extra["rel_status_"+other].(string); ok2 && v != "" {
			return v
		}
	}
	// 数值兜底（通用档位，不绑定任何世界）
	if ent, ok := s.engine.State().Entities[hero]; ok && ent.Relationship != nil {
		v := ent.Relationship[other]
		switch {
		case v >= 0.8:
			return "亲密"
		case v >= 0.5:
			return "友好"
		case v >= 0.2:
			return "熟识"
		default:
			return "相识"
		}
	}
	return "相识"
}

// buildActions 生成 4 个行动选项（零 LLM：段落目标/关系/能力/健康 驱动）
func (s *Simulator) buildActions() []PlayerAction {
	ents := s.engine.State().Entities
	// 未初始化（无主角）时无可行动对象
	if _, ok := ents[s.heroName]; !ok || s.heroName == "" {
		return nil
	}
	acts := []PlayerAction{}

	// ① 调查：围绕当前段落目标
	goal := ""
	if len(s.arcBook) > 0 {
		for i := len(s.arcBook) - 1; i >= 0; i-- {
			if s.arcBook[i].Status == "open" {
				goal = s.arcBook[i].Goal
				break
			}
		}
	}
	if goal == "" && len(s.recentTitles(1)) > 0 {
		goal = "最近发生的「" + s.recentTitles(1)[0] + "」"
	}
	if goal != "" {
		acts = append(acts, PlayerAction{
			ID: "act-investigate", Kind: "investigate", Icon: "🔍",
			Label: "深入调查",
			Intent: "围绕当前形势深入调查，摸清来龙去脉：" + goal,
		})
	} else {
		acts = append(acts, PlayerAction{
			ID: "act-explore", Kind: "explore", Icon: "🧭",
			Label: "出门探察",
			Intent: "主动前往当前所在地之外的地方打探消息、寻找机会",
		})
	}

	// ② 交际：找关系最好的角色
	if heroEnt, ok := ents[s.heroName]; ok && heroEnt.Relationship != nil {
		best, bestV := "", 0.0
		for name, v := range heroEnt.Relationship {
			if v > bestV {
				best, bestV = name, v
			}
		}
		if best != "" {
			rank := s.relRank(s.heroName, best)
			acts = append(acts, PlayerAction{
				ID: "act-social", Kind: "social", Icon: "🤝",
				Label: "会晤 " + best,
				Intent: "主动去找" + best + "深谈一次，巩固情谊、交换消息（当前关系：" + rank + "）",
			})
		}
	}

	// ③ 积累：提升核心能力（stats 第一个属性）
	if heroEnt, ok := ents[s.heroName]; ok && heroEnt.Stats != nil && len(heroEnt.Stats) > 0 {
		keys := make([]string, 0, len(heroEnt.Stats))
		for k := range heroEnt.Stats {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		attr := keys[0]
		val := heroEnt.Stats[attr]
		acts = append(acts, PlayerAction{
			ID: "act-cultivate", Kind: "cultivate", Icon: "📈",
			Label: "精进 " + attr,
			Intent: "投入时间精力提升「" + attr + "」（当前：" + fmtAny(val) + "），增强自身实力",
		})
	}

	// ④ 休整或探索：健康低→休整，否则找新目标
	if heroEnt, ok := ents[s.heroName]; ok && heroEnt.Health < 70 {
		acts = append(acts, PlayerAction{
			ID: "act-rest", Kind: "rest", Icon: "🛌",
			Label: "休整调养",
			Intent: "放缓节奏，好好休整调养身体（当前健康偏低），同时梳理手头线索",
		})
	} else {
		acts = append(acts, PlayerAction{
			ID: "act-gather", Kind: "explore", Icon: "🗺️",
			Label: "多方打听",
			Intent: "四处打听新鲜事和隐藏机会，看看有没有值得抓住的机缘",
		})
	}
	return acts
}

// recentTitles 最近 N 条事件标题（从 chronicle 提取）
func (s *Simulator) recentTitles(n int) []string {
	var out []string
	for i := len(s.chronicle) - 1; i >= 0 && len(out) < n; i-- {
		c := s.chronicle[i]
		if c.Kind == "EVENT" || strings.Contains(c.Content, "「") {
			out = append(out, c.Content)
		}
	}
	return out
}

// worldName 世界名（worldDir 最后一段）
func (s *Simulator) worldName() string {
	parts := strings.Split(strings.TrimRight(s.worldDir, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func fmtAny(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}