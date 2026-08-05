package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"worldsim/internal/engine"
	"worldsim/internal/llm"
)

// ---------- 角色档案系统（每个角色有完整"灵魂"：性格/习惯/社交/行为/思考方式） ----------
// 参考：NovelClaw 角色视图 / 马良写作人设卡（身份/性格/动机/软肋/关系/标签）
// 核心原则：角色定位不固定——谁成为主角/女主/重要配角由剧情演化，档案是"活的"

// CharacterSheet 完整人设卡（存 extra.persona_sheet，JSON字符串）
type CharacterSheet struct {
	Name      string   `json:"name"`
	Role      string   `json:"role"` // protagonist | love_interest | important_npc | rival | npc（可演化）
	Age       string   `json:"age"`
	Identity  string   `json:"identity"`  // 职业/身份
	Personality []string `json:"personality"` // 性格特质（3-5个）：谨慎/固执/热心/毒舌…
	Habits    []string `json:"habits"`    // 习惯/小动作/口头禅（2-3个）
	Social    []string `json:"social"`    // 社交/人脉/对谁什么态度（2-3条）
	Behavior  []string `json:"behavior"`  // 行为方式/决策倾向（2-3条）
	Thinking  []string `json:"thinking"`  // 思考方式/价值观/底层逻辑（2-3条）
	Motives   []string `json:"motives"`   // 目标：短期+长期（2条）
	Fears     []string `json:"fears"`     // 软肋/恐惧（1-2条）
	Secret    string   `json:"secret"`    // 秘密（他自己知道，别人不知道）
}

// SheetPrompt 生成可注入 prompt 的角色档案文本（让NPC言行有灵魂且一致）
func (cs *CharacterSheet) SheetPrompt() string {
	if cs == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("【%s 的人格档案】\n", cs.Name))
	if len(cs.Personality) > 0 {
		sb.WriteString("性格：" + strings.Join(cs.Personality, "、") + "\n")
	}
	if len(cs.Habits) > 0 {
		sb.WriteString("习惯/小动作：" + strings.Join(cs.Habits, "；") + "\n")
	}
	if len(cs.Social) > 0 {
		sb.WriteString("社交/人脉：" + strings.Join(cs.Social, "；") + "\n")
	}
	if len(cs.Behavior) > 0 {
		sb.WriteString("行为方式：" + strings.Join(cs.Behavior, "；") + "\n")
	}
	if len(cs.Thinking) > 0 {
		sb.WriteString("思考方式/价值观：" + strings.Join(cs.Thinking, "；") + "\n")
	}
	if len(cs.Motives) > 0 {
		sb.WriteString("目标：" + strings.Join(cs.Motives, "；") + "\n")
	}
	if len(cs.Fears) > 0 {
		sb.WriteString("软肋/恐惧：" + strings.Join(cs.Fears, "；") + "\n")
	}
	return strings.TrimSpace(sb.String())
}

// CharacterSheetLLM 用 LLM 把角色雏形扩展成完整人设卡（1次 fast 调用）
func CharacterSheetLLM(ctx context.Context, c *LLMClient, name, identity, hint string) *CharacterSheet {
	if c == nil {
		return nil
	}
	system := `你是一位角色设计大师。为下面这位新登场的角色设计一张完整的人设卡（让TA像真人一样有血有肉）。
角色：` + name + `（` + identity + `）
线索：` + hint + `
输出严格 JSON：
{"name":"` + name + `","age":"大致年龄段","identity":"职业/身份","personality":["性格特质×4，具体可感"],"habits":["习惯/小动作/口头禅×2，具体可感"],"social":["社交关系×2，具体可感"],"behavior":["行为方式×2，具体可感"],"thinking":["思考方式×2，具体可感"],"motives":["目标×2，具体可感"],"fears":["软肋×1，具体可感"],"secret":"只有TA自己知道的秘密（一句话）"}
要求：
1. 具体、可感、有矛盾感（人不是单面的），每个习惯/社交都要有画面感，禁止空泛形容词堆砌。
2. **习惯/口头禅必须从TA的职业、时代背景、生活日常推导**——TA的日常职业动作、生活细节、时代特有的行为方式；至少1个"与主线无关的生活小碎片"（TA私下爱做的事、小癖好、日常routine），这是让人物像"活人"而不是"剧情NPC"的关键。
3. 说话方式要有个人烙印：呛人/话痨/沉默/爱反问/口头禅，符合TA的身份与性格。
4. **记忆钉四件套（让角色"被记住"的具象符号，主角必须四件齐全，配角至少1~2件）**：
   · 招牌动作：能"被模仿"的动作，与性格绑定
   · 口头禅：一句反复出现的口头语，能体现性格
   · 专属反应：遇到特定情况时的个人化反应（紧张会笑的人/难过会吃东西的人/生气时反而安静的人）
   · 专属物件：随身携带、有故事的物件
   以上四件套写进 habits/social/behavior 相应字段，必须与TA的身份和世界设定自洽，不得套用别的世界的现成符号。` + CharacterDesignSkills()
	raw, err := c.CompleteTier(ctx, "fast", system, "请设计这张人设卡。")
	if err != nil {
		return nil
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return nil
	}
	var cs CharacterSheet
	if json.Unmarshal([]byte(jsonStr), &cs) != nil {
		return nil
	}
	if cs.Name == "" {
		cs.Name = name
	}
	if cs.Identity == "" {
		cs.Identity = identity
	}
	// 保底：避免空数组
	cs.Personality = nonEmptyOr(cs.Personality, "话不多但心里有数")
	cs.Thinking = nonEmptyOr(cs.Thinking, "先观察再行动")
	cs.Habits = nonEmptyOr(cs.Habits, "习惯性把话咽回去一半")
	return &cs
}

// BuildCharacterSheet 生成并保存角色档案（extra.persona_sheet），返回变更
func (s *Simulator) BuildCharacterSheet(ctx context.Context, name string) []engine.Change {
	ctx = llm.WithSpan(ctx, "角色档案")
	st := s.engine.State()
	ent, ok := st.Entities[name]
	if !ok {
		return nil
	}
	// 已有完整档案（性格≥3条=LLM生成过）则跳过；兜底模板允许重试
	if raw, ok := ent.Extra["persona_sheet"].(string); ok && raw != "" {
		var old CharacterSheet
		if json.Unmarshal([]byte(raw), &old) == nil && len(old.Personality) >= 3 {
			return nil
		}
	}
	identity, _ := ent.Extra["identity"].(string)
	if identity == "" {
		identity = ent.Job
	}
	hint, _ := ent.Extra["persona"].(string)
	// 注入世界语境：习惯/小动作必须贴合本世界的世界观与时代
	if s.wb != nil {
		if wc := s.wb.ForWorldBrief(); wc != "" {
			hint = strings.TrimSpace(hint + "\n世界背景：" + wc)
		}
	}
	var cs *CharacterSheet
	if s.llm != nil {
		cs = CharacterSheetLLM(ctx, s.llm, name, identity, hint)
	}
	if cs == nil {
		// LLM失败：用线索兜底生成最小档案
		cs = &CharacterSheet{
			Name: name, Role: roleOf(ent), Identity: identity,
			Personality: []string{"话不多但心里有数"}, Habits: []string{"习惯性先观察"},
			Social:   []string{"认识主角，点头之交"},
			Behavior: []string{"低调行事"},
			Thinking: []string{"先观察再行动"}, Motives: []string{"过好自己的日子"},
			Fears:    []string{"被卷进麻烦"}, Secret: "心里藏着一些没说出口的事",
		}
	}
	b, _ := json.Marshal(cs)
	var changes []engine.Change
	changes = append(changes,
		engine.Change{Path: "entities." + name + ".extra.persona_sheet", Op: "set", Value: string(b)},
		// 注意：不覆盖 extra.role —— role 是系统标记（protagonist/npc），由初始化/注册管理，
		// LLM 人设卡的 role 字段是空的，覆盖会把主角标记抹掉（曾导致重启后主角漂移）
	)
	return changes
}

// SheetOf 读取角色档案（从 state 的 extra.persona_sheet）
func (s *Simulator) SheetOf(name string) *CharacterSheet {
	ent, ok := s.engine.State().Entities[name]
	if !ok {
		return nil
	}
	raw, ok := ent.Extra["persona_sheet"].(string)
	if !ok || raw == "" {
		return nil
	}
	var cs CharacterSheet
	if json.Unmarshal([]byte(raw), &cs) != nil {
		return nil
	}
	return &cs
}

// FormatSheetForPrompt 组合角色档案+记忆+动态属性 注入 NPC prompt
func (s *Simulator) FormatSheetForPrompt(name string) string {
	var parts []string
	if cs := s.SheetOf(name); cs != nil {
		parts = append(parts, cs.SheetPrompt())
	}
	if ent, ok := s.engine.State().Entities[name]; ok {
		if p, ok := ent.Extra["persona"].(string); ok && p != "" {
			parts = append(parts, "一句话人设："+p)
		}
		// 世界书驱动的动态属性集（属性名随世界变化）
		if len(ent.Stats) > 0 {
			b, _ := json.Marshal(ent.Stats)
			parts = append(parts, "个人属性："+string(b))
		}
	}
	return strings.Join(parts, "\n")
}

func roleOf(ent engine.Entity) string {
	if r, ok := ent.Extra["role"].(string); ok && r != "" {
		return r
	}
	return "npc"
}

// isWalkon 判断角色是否为龙套（walkon）：只出场一两次、不建档不占记忆的路人
func (s *Simulator) isWalkon(name string) bool {
	if ent, ok := s.engine.State().Entities[name]; ok {
		if t, ok := ent.Extra["tier"].(string); ok && t == "walkon" {
			return true
		}
	}
	return false
}

func nonEmptyOr(list []string, fallback string) []string {
	for _, s := range list {
		if strings.TrimSpace(s) != "" {
			return list
		}
	}
	return []string{fallback}
}