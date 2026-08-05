package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"worldsim/internal/engine"
	"worldsim/internal/llm"
	"worldsim/internal/worldbook"
)

// ---------- NPC 互动（§5.1 场景状态机：Init → Act → GM裁决 → React） ----------

type DialogueTurn struct {
	Speaker string `json:"speaker"` // npc_某角色名 | 主角名
	Speech  string `json:"speech"`
	Mood    string `json:"mood,omitempty"`
}

// NPCRespondLLM NPC 基于完整人设档案+记忆/场景自主发言（§7.5：NPC言行只能由自己产出）
func (s *Simulator) NPCRespond(ctx context.Context, npcName, npcMemory, scene string, wb *worldbook.Worldbook) (DialogueTurn, error) {
	ctx = llm.WithSpan(ctx, "NPC对话")
	if s.llm == nil {
		return DialogueTurn{Speaker: npcName, Speech: "……（沉默）", Mood: "平静"}, nil
	}
	// 注入完整人设档案（性格/习惯/社交/行为/思考方式）——前缀稳定，利于 DeepSeek 缓存
	profile := s.FormatSheetForPrompt(npcName)
	system := `你是{npc}，一个活在这个世界里的真人。你必须有灵魂——言行由你的人格档案决定，保持一致性：
` + profile + `

规则：
1. 输出严格 JSON：{"speech":"你说出口的一句话（口语化，带你的习惯/口头禅，符合身份，不超过50字）","mood":"当前情绪","relation_delta":<数值>}
2. relation_delta 是你对主角好感度的变化（-0.3~0.3）
3. 你的习惯和性格会自然流露在语言里（小动作、口头禅、思维方式）；话里有话可以，但别直白剧透；不知道的事不要装知道。`
	system = strings.ReplaceAll(system, "{npc}", npcName)

	// 记忆是动态的，放 user 末尾（保证 system 前缀字节级稳定 → 缓存命中）
	user := fmt.Sprintf("你的记忆：\n%s\n\n当前场景：%s\n你看到主角（{HERO}）来了。请自然地、按你的性格说一句话。", npcMemory, scene)
	user = strings.ReplaceAll(user, "{HERO}", s.heroName)

	raw, err := s.llm.CompleteTier(ctx, "fast", system, user)
	if err != nil {
		return DialogueTurn{}, err
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return DialogueTurn{}, fmt.Errorf("NPC输出无JSON: %s", truncate(raw, 120))
	}
	var resp struct {
		Speech        string  `json:"speech"`
		Mood          string  `json:"mood"`
		RelationDelta float64 `json:"relation_delta"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return DialogueTurn{}, fmt.Errorf("NPC JSON解析失败: %v", err)
	}
	if resp.Speech == "" {
		resp.Speech = "……（沉默）"
	}
	return DialogueTurn{Speaker: npcName, Speech: resp.Speech, Mood: resp.Mood}, nil
}

// HeroRespondLLM 主角在对话中的回应（简短自然，基于感知与性格；动态内容放user保持前缀稳定）
func HeroRespondLLM(ctx context.Context, c *LLMClient, hero, heroProfile, npcSpeech string, wb *worldbook.Worldbook) (DialogueTurn, error) {
	ctx = llm.WithSpan(ctx, "主角回应")
	worldCtx := ""
	if wb != nil {
		worldCtx = wb.ForProtagonist(hero, heroProfile)
	}
	system := `你是主角{hero}，一个活在这个世界里的真人。{world}

规则：
1. 输出严格 JSON：{"speech":"你的回应（口语化，不超过40字）","thinking":"你心里怎么想（一句话）"}
2. 回应要符合普通人的反应，不要全知，不要突然知道不该知道的事。`
	system = strings.ReplaceAll(system, "{hero}", hero)
	system = strings.ReplaceAll(system, "{world}", worldCtx)

	// 对方说了什么 = 动态，放 user（system 前缀稳定）
	user := fmt.Sprintf("你正在和面前的人对话，对方说：\"%s\"\n请用你的身份与性格自然地回应一句话。", npcSpeech)

	raw, err := c.CompleteTier(ctx, "fast", system, user)
	if err != nil {
		return DialogueTurn{}, err
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return DialogueTurn{}, fmt.Errorf("主角回应无JSON: %s", truncate(raw, 120))
	}
	var resp struct {
		Speech   string `json:"speech"`
		Thinking string `json:"thinking"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return DialogueTurn{}, fmt.Errorf("主角回应 JSON解析失败: %v", err)
	}
	if resp.Thinking != "" {
		// 对话中的内心也计入 lastThinking
		_ = resp.Thinking
	}
	return DialogueTurn{Speaker: hero, Speech: resp.Speech}, nil
}

// ---------- 对话循环（§5.1：Init → NPC开口 → 主角回应 → NPC收尾，≤3轮） ----------

// DialogueBatchLLM 一次调用生成完整三轮对话（NPC开口→主角回应→NPC收尾）
// 替代原来的 3 次独立调用 → 每天省 2 次 LLM 调用（大幅提速，质量靠人格卡保持）
func (s *Simulator) DialogueBatchLLM(ctx context.Context, npcName, npcMemory, heroProfile, scene string) ([]DialogueTurn, error) {
	ctx = llm.WithSpan(ctx, "NPC批量对话")
	profile := s.FormatSheetForPrompt(npcName)
	system := `你是对话导演，负责生成一场真实的日常相遇对话。角色：
NPC：{npc}，人格档案（言行必须符合，保持一致性）：
` + profile + `

主角：{HERO}，性格：{heroProfile}

规则：
1. 输出严格 JSON 数组，恰好 3 段：
[{"speaker":"{npc}","speech":"NPC开口的话（口语化，带习惯/口头禅，不超过50字）","mood":"情绪"},
{"speaker":"{HERO}","speech":"主角回应（自然，符合性格，不超过40字）"},
{"speaker":"{npc}","speech":"NPC收尾（呼应前文，可话里有话，不超过50字）"}]
2. 言行符合各自人格；NPC 不能全知（不知道的事不装知道，世界秘密不能说）；对话有生活气息、像真人聊天。
3. 根据场景自然切入，别生硬，别喊名字式开场。`
	system = strings.ReplaceAll(system, "{npc}", npcName)
	system = strings.ReplaceAll(system, "{HERO}", s.heroName)
	system = strings.ReplaceAll(system, "{heroProfile}", heroProfile)
	user := fmt.Sprintf("NPC 记忆：\n%s\n\n当前场景：%s", npcMemory, scene)
	raw, err := s.llm.CompleteTier(ctx, "fast", system, user)
	if err != nil {
		return nil, err
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return nil, fmt.Errorf("批量对话无JSON: %s", truncate(raw, 120))
	}
	var turns []DialogueTurn
	if err := json.Unmarshal([]byte(jsonStr), &turns); err != nil {
		return nil, fmt.Errorf("批量对话JSON解析失败: %v", err)
	}
	// 兜底：至少要有NPC开口和主角回应两段
	if len(turns) >= 2 && turns[0].Speech != "" && turns[1].Speech != "" {
		return turns[:3], nil
	}
	return nil, fmt.Errorf("批量对话轮次不足: %d", len(turns))
}

// RunDialogue 执行一段 NPC 互动对话，返回对话记录与关系值变化（走提案通道）
func (s *Simulator) RunDialogue(ctx context.Context, ev EventCard) ([]DialogueTurn, *engine.Proposal, error) {
	var turns []DialogueTurn

	// NPC 人设与记忆（从实体 extra 读取；无则用世界第一个非主角实体）
	// 优先选 active 角色（mentioned=已淡出，只在编年史里被提起，不主动登场）
	npcName := ""
	if len(ev.NPCs) > 0 {
		npcName = ev.NPCs[0] // 事件明确指定 NPC 时，即使 mentioned 也要登场（背景人物被唤醒）
	} else {
		for n, ent := range s.engine.State().Entities {
			if n == s.heroName || ent.Status != "active" {
				continue
			}
			npcName = n
			break
		}
		if npcName == "" {
			for n := range s.engine.State().Entities {
				if n != s.heroName {
					npcName = n
					break
				}
			}
		}
	}
	if npcName == "" {
		npcName = "熟人"
	}
	memory := s.npcMemory(npcName)

	// 加速路径：一次调用生成完整三轮对话（省 2 次 LLM 调用）
	if s.llm != nil {
		if batch, err := s.DialogueBatchLLM(ctx, npcName, memory, s.heroProfile(), ev.Frame); err == nil {
			turns = batch
		}
	}
	// 回退：逐段生成（批量失败时）
	if len(turns) == 0 {
		// Init：NPC 先开口（§5.1 规则1，注入完整人设档案）
		if s.llm != nil {
			t, err := s.NPCRespond(ctx, npcName, memory, ev.Frame, s.wb)
			if err == nil {
				turns = append(turns, t)
			}
		} else {
			// dry-run：NPC模板台词
			turns = append(turns, DialogueTurn{Speaker: npcName, Speech: "今天怎么有空来？", Mood: "平静"})
		}

		// 主角回应
		if len(turns) > 0 {
			if s.llm != nil {
				heroProfile := s.heroProfile()
				t, err := HeroRespondLLM(ctx, s.llm, s.heroName, heroProfile, turns[0].Speech, s.wb)
				if err == nil {
					turns = append(turns, t)
				} else {
					turns = append(turns, DialogueTurn{Speaker: s.heroName, Speech: "嗯，路过顺便看看。"})
				}
			} else {
				turns = append(turns, DialogueTurn{Speaker: s.heroName, Speech: "嗯，路过顺便看看。"})
			}
		}

		// NPC 收尾（视对话氛围决定是否继续；简化：NPC 补一句）
		if len(turns) >= 2 && s.llm != nil {
			last := turns[len(turns)-1].Speech
			t, err := s.NPCRespond(ctx, npcName, memory+" 刚才主角回应："+last, ev.Frame, s.wb)
			if err == nil {
				turns = append(turns, t)
			}
		}
	}

	// 关系值双向更新（升级：好感/信任/状态/大事记 全维度）
	prop := &engine.Proposal{
		Changes: s.UpdateRelation(s.heroName, npcName, 0.05, 0.03, "与"+npcName+"的日常对话"),
		Reason:  "与" + npcName + "的对话互动",
	}
	// 记录最近出场日（淡出机制依赖：长时间没见的配角会自动淡出）
	// 若该角色已淡出（mentioned）但事件又让他登场 → 恢复 active（背景人物重新走上前台）
	if ent, ok := s.engine.State().Entities[npcName]; ok {
		if ent.Status == "mentioned" {
			prop.Changes = append(prop.Changes,
				engine.Change{Path: "entities." + npcName + ".status", Op: "set", Value: "active"},
				engine.Change{Path: "entities." + npcName + ".extra.revived_day", Op: "set", Value: s.day},
			)
		}
	}
	prop.Changes = append(prop.Changes,
		engine.Change{Path: "entities." + npcName + ".extra.last_seen_day", Op: "set", Value: s.day},
	)
	return turns, prop, nil
}

// npcProfile NPC 人设（从实体 extra.profile 读取，或默认）
func (s *Simulator) npcProfile(name string) string {
	if ent, ok := s.engine.State().Entities[name]; ok {
		if p, ok := ent.Extra["profile"].(string); ok && p != "" {
			return p
		}
	}
	return fmt.Sprintf("%s：本地的熟人，话不多但消息灵通。", name)
}

// npcMemory 组装 NPC 发言用记忆：优先从 MemoryStore 双源召回（Working完整+Archive摘要），
// 再拼上人设卡里的长期设定（extra.memory）——NPC 从此有"记忆"，不再次次像初见
func (s *Simulator) npcMemory(name string) string {
	var sb strings.Builder
	if ent, ok := s.engine.State().Entities[name]; ok {
		if m, ok := ent.Extra["memory"].(string); ok && m != "" {
			sb.WriteString(m)
			sb.WriteString("\n")
		}
	}
	if s.mem != nil {
		if entries := s.mem.Retrieve(name, "最近发生的事、对话、关系", 12); len(entries) > 0 {
			s.mem.StrengthenRetrieval(name, entries) // 检索强化：NPC 被想起的记忆也加深
			sb.WriteString("最近经历（记忆库召回）：\n")
			for _, e := range entries {
				sb.WriteString(fmt.Sprintf("· Day%d：%s\n", e.Day, e.Content))
			}
		}
	}
	if sb.Len() == 0 {
		return "记得与主角（常来往的熟人）的往来，其他生活平淡。"
	}
	return strings.TrimSpace(sb.String())
}

func (s *Simulator) heroProfile() string {
	if ent, ok := s.engine.State().Entities[s.heroName]; ok {
		if p, ok := ent.Extra["profile"].(string); ok && p != "" {
			return p
		}
	}
	return "这个世界里的一名普通人，有自己的日子要过。"
}