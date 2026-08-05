package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"worldsim/internal/engine"
	"worldsim/internal/llm"
	"worldsim/internal/worldbook"
)

// ---------- 世界初始化 Agent（按世界书生成初始主角/NPC/地点，杜绝"套都市模板"） ----------
// 每个新世界都应该有自己的起点（由世界书推导，不套用现成模板）。
// 模板只给"维度"（主角要有人设/位置/职业/现状），内容由 LLM 按世界书推导。

type WorldInitPlan struct {
	Protagonist struct {
		Name     string         `json:"name"`
		Location string         `json:"location"`
		Job      string         `json:"job"`
		Money    any            `json:"money"`  // 兼容字符串/数字（LLM可能输出"12"或"五枚银币"）
		Health   any            `json:"health"` // 同上
		Profile  string         `json:"profile"` // 一句话现状（注入 extra.profile）
		Memory   string         `json:"memory"`  // 初始记忆（他记得什么）
		Stats    map[string]any `json:"stats"`   // 世界书驱动的动态属性集
	} `json:"protagonist"`
	NPCs []struct {
		Name     string         `json:"name"`
		Location string         `json:"location"`
		Job      string         `json:"job"`
		Profile  string         `json:"profile"` // 一句话身份（他清楚自己，主角不知道）
		Memory   string         `json:"memory"`  // 初始记忆
		Stats    map[string]any `json:"stats"`   // 世界书驱动的动态属性集
		Tier     string         `json:"tier"`    // core=核心配角 | support=重要配角
	} `json:"npcs"`
	SeedLocations []struct {
		Name string `json:"name"`
		Type string `json:"type"` // 村镇/宗门/坊市/城区/自然/建筑
		Note string `json:"note"`
	} `json:"seed_locations"`
	WorldEvents []string `json:"world_events"` // 初始世界事件（背景设定）
}

// WorldInitPlanLLM 用 LLM 按世界书生成世界初始方案（1次 fast 调用）
func WorldInitPlanLLM(ctx context.Context, c *LLMClient, wb *worldbook.Worldbook) *WorldInitPlan {
	if c == nil || wb == nil {
		return nil
	}
	ctx = llm.WithSpan(ctx, "世界初始化")
	worldCtx := wb.ForWorldAgent()
	system := `你是这方世界的"创世者"。根据世界书，生成这个世界的**初始状态**——主角是谁、他在哪、干什么，常驻NPC，种子地点，初始背景事件。
输出严格 JSON，格式：
{"protagonist":{"name":"主角名","location":"初始位置（按世界书的设定）","job":"职业/身份","money":<数值>,"health":90,"profile":"一句话现状（贴合世界质感）","memory":"主角的初始记忆（他记得的最近的事）","stats":{"<属性名>":<数值>,...}},
"npcs":[{"name":"常驻NPC名","location":"位置","job":"身份","profile":"一句话身份（他清楚自己是谁，主角不知道全貌）","memory":"他记得什么","stats":{"<属性名>":<数值>,...},"tier":"core|support"}],
"seed_locations":[{"name":"地点名","type":"<按世界类型>","note":"地点简述"}],
"world_events":["初始背景事件（世界正在发生的事）"]}
要求：
1. **一切按世界书来**：主角的职业/处境/目标必须从世界书的弧线（B3）和世界观（A1）推导，NPC 从势力（A3）和秘密（B1）推导——绝不允许套用别的世界的现成模板。
2. 主角要有"生活质感"：他的日常是他那个世界真实的生活，不是抽象设定。
3. **NPC 分层生成（小说配角的生态，不是几个工具人）**：
   · **core（核心配角，3~4个）**：主角身边高频出现的人——亲人/同门/同事/挚友/房东/对头等。他们与主角有持续互动，需要完整人设和记忆，是本世界的"常驻面孔"。
   · **support（重要配角，2~3个）**：经常出现在主角生活圈但戏份较轻的人——邻居/摊主/酒馆常客/同窗/同僚等。他们认识主角、偶尔互动，不需要每天在场。
   · **提及型（不出现在 npcs 数组，只写进 world_events 或 A3/A4 背景）**：只活在传闻和背景里的名字——街角卖报的、码头管事、某势力的边缘人物。他们不必注册实体，偶尔被提起即可。
   · NPC 的 tier 从世界书的势力（A3）、地理（A4）、人物关系推导：谁跟主角最亲近就 core，谁只是生活背景就 support。
   · **总量控制在 6~7 个以内**（宁精勿滥）：初始只放最核心的几个人，更多配角会在模拟过程中由事件自然登场——这不是偷懒，是让配角"活"在故事里，而不是一次全塞进来。
4. money 用该世界的货币单位数值（按世界书的设定）。
5. **stats 是这个世界定义的角色属性集**：从世界书的力量体系（A7/A9）、资源体系（A2）、生存要素（A1/A4）推导这个世界的人"身上有哪些值得追踪的属性"，每个角色填 3~6 个关键属性（数值型或文本型，数值型的给初始数值）。属性名必须取自世界书自身的概念，禁止套用其他世界的现成属性名。
6. 只输出 JSON，不要其他文字。`
	raw, err := llm.CallAPITierSync(ctx, c.Cfg, "fast", system, "请生成这个世界的初始方案。世界书内容：\n"+worldCtx)
	if err != nil {
		fmt.Printf(" [世界初始化] LLM 调用失败: %v\n", err)
		return nil
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		fmt.Printf(" [世界初始化] LLM 输出无 JSON: %s\n", truncate(raw, 200))
		return nil
	}
	var plan WorldInitPlan
	if err := json.Unmarshal([]byte(jsonStr), &plan); err != nil {
		fmt.Printf(" [世界初始化] JSON 解析失败: %v\n原始输出: %s\n", err, truncate(jsonStr, 300))
		return nil
	}
	if strings.TrimSpace(plan.Protagonist.Name) == "" {
		fmt.Printf(" [世界初始化] 主角名为空，原始输出: %s\n", truncate(jsonStr, 300))
		return nil
	}
	if len(plan.SeedLocations) == 0 {
		fmt.Printf(" [世界初始化] 种子地点为空，原始输出: %s\n", truncate(jsonStr, 300))
		return nil
	}
	return &plan
}

// ApplyPlan 把初始方案转成 engine.Change 列表（供 init 提交）
func (p *WorldInitPlan) Changes(hero string) []engine.Change {
	var ch []engine.Change
	ch = append(ch,
		engine.Change{Path: "world_level.tension", Op: "set", Value: 0.2},
	)
	// 主角
	ch = append(ch,
		engine.Change{Path: "entities." + hero + ".location", Op: "set", Value: p.Protagonist.Location},
		engine.Change{Path: "entities." + hero + ".money", Op: "set", Value: numOrZero(p.Protagonist.Money)},
		engine.Change{Path: "entities." + hero + ".health", Op: "set", Value: numOrZero(p.Protagonist.Health)},
		engine.Change{Path: "entities." + hero + ".job", Op: "set", Value: p.Protagonist.Job},
		engine.Change{Path: "entities." + hero + ".alive", Op: "set", Value: true},
		engine.Change{Path: "entities." + hero + ".status", Op: "set", Value: "active"},
		engine.Change{Path: "entities." + hero + ".extra.role", Op: "set", Value: "protagonist"},
		engine.Change{Path: "entities." + hero + ".extra.profile", Op: "set", Value: p.Protagonist.Profile},
		engine.Change{Path: "entities." + hero + ".extra.memory", Op: "set", Value: p.Protagonist.Memory},
	)
	// 主角 stats（世界书驱动的动态属性集）
	for k, v := range p.Protagonist.Stats {
		ch = append(ch, engine.Change{Path: "entities." + hero + ".stats." + k, Op: "set", Value: v})
	}
	// 常驻 NPC
	for _, n := range p.NPCs {
		tier := strings.TrimSpace(n.Tier)
		if tier == "" {
			tier = "support" // 未标注默认 support（轻量配角）
		}
		ch = append(ch,
			engine.Change{Path: "entities." + n.Name + ".location", Op: "set", Value: n.Location},
			engine.Change{Path: "entities." + n.Name + ".job", Op: "set", Value: n.Job},
			engine.Change{Path: "entities." + n.Name + ".alive", Op: "set", Value: true},
			engine.Change{Path: "entities." + n.Name + ".status", Op: "set", Value: "active"},
			engine.Change{Path: "entities." + n.Name + ".extra.role", Op: "set", Value: "important_npc"},
			engine.Change{Path: "entities." + n.Name + ".extra.tier", Op: "set", Value: tier},
			engine.Change{Path: "entities." + n.Name + ".extra.profile", Op: "set", Value: n.Profile},
			engine.Change{Path: "entities." + n.Name + ".extra.memory", Op: "set", Value: n.Memory},
		)
		// NPC stats
		for k, v := range n.Stats {
			ch = append(ch, engine.Change{Path: "entities." + n.Name + ".stats." + k, Op: "set", Value: v})
		}
	}
	// 种子地点（写进去后 SeedLocations 检测到非空就不会套都市默认池）
	for _, l := range p.SeedLocations {
		ch = append(ch,
			engine.Change{Path: "world_level.locations." + l.Name + ".type", Op: "set", Value: l.Type},
			engine.Change{Path: "world_level.locations." + l.Name + ".state", Op: "set", Value: "正常"},
			engine.Change{Path: "world_level.locations." + l.Name + ".note", Op: "set", Value: l.Note},
		)
	}
	// 初始背景事件
	for _, e := range p.WorldEvents {
		ch = append(ch, engine.Change{Path: "world_level.global_events", Op: "add", Value: e})
	}
	return ch
}

func (p *WorldInitPlan) String() string {
	return fmt.Sprintf("主角=%s(%s@%s) NPC=%d 地点=%d 事件=%d",
		p.Protagonist.Name, p.Protagonist.Job, p.Protagonist.Location, len(p.NPCs), len(p.SeedLocations), len(p.WorldEvents))
}

// numOrZero 把 LLM 输出的 money/health 转成数值：兼容数字、数字字符串（"12"）、
// 带单位的字符串（"12银币"→12、"五枚"→0 兜底）——解析失败返回 0
func numOrZero(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, err := t.Float64()
		if err == nil {
			return f
		}
	case string:
		// 提取开头的数字部分（"12银币"→12）
		var num strings.Builder
		for _, r := range strings.TrimSpace(t) {
			if r >= '0' && r <= '9' || r == '.' || r == '-' {
				num.WriteRune(r)
			} else {
				break
			}
		}
		if num.Len() > 0 {
			if f, err := strconv.ParseFloat(num.String(), 64); err == nil {
				return f
			}
		}
	}
	return 0
}