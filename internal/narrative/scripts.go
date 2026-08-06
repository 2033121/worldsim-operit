package narrative

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"worldsim/internal/llm"
)

// ---------- 场景剧本化（Beat → SceneScript[]） ----------

// PlanScripts 把一章的节拍细化为"可写作的场景剧本"，并为每个场景注入角色状态卡。
// 返回按顺序排列的 SceneScript[]。
func PlanScripts(ctx context.Context, in *Input, u StoryUnit, beats []Beat) ([]SceneScript, error) {
	if len(beats) == 0 {
		return nil, nil
	}
	if in.LLM == nil {
		return fallbackScripts(in, u, beats), nil
	}
	// 本章登场角色（节拍里出现的人 + 单元 chars）
	names := map[string]bool{}
	for _, c := range u.Chars {
		names[c] = true
	}
	for _, b := range beats {
		for _, c := range b.Chars {
			names[c] = true
		}
	}
	var charNames []string
	for n := range names {
		charNames = append(charNames, n)
	}
	cards := buildCharCards(charNames, in.Entities, in.Hero)
	cardsText := formatCards(cards)

	// 节拍文本
	var beatsText strings.Builder
	for i, b := range beats {
		beatsText.WriteString(fmt.Sprintf("%d.[%s] %s", i+1, b.Type, b.Summary))
		if b.Location != "" {
			beatsText.WriteString(" @" + b.Location)
		}
		if len(b.Chars) > 0 {
			beatsText.WriteString(" 角色:" + strings.Join(b.Chars, "/"))
		}
		beatsText.WriteString("\n")
	}

	system := `你是网文小说的"场景剧本化师"。你把一章的剧情节拍，细化为**每个场景的剧本**——写手拿到剧本直接写正文，不需要自己设计剧情。
输出严格 JSON，格式：
{"scenes":[{"beat_type":"对应节拍类型","goal":"本场景目标（角色要达成什么）","conflict":"本场景冲突（谁 vs 什么，或内心挣扎）","dialogue":"对话要点（交锋/潜台词/信息量，写手据此写对白）","turn":"本场景的转折（发生什么变化）","hook":"本场景结尾钩子（若有）","source_days":[对应素材天,整数数组]}]}
规则：
1. 一个节拍可以是一个场景，也可以拆成 2 个场景（比如 conflict 拆成"对峙"和"结果"）；不要合并节拍。
2. 每个场景必须有"事"：角色带着目标进场，遇到阻碍/变化，场景结束时局势和开场不同。
3. dialogue 是重点：写出这场戏的对白要点（谁说什么、话里有话、信息怎么漏出来），写手照此扩写，禁止一问一答干巴巴。
4. 冲突要具体：目标冲突/观念冲突/资源冲突/情绪冲突——想清楚是哪类再写。
5. 角色状态卡已给出：场景内角色的言行必须符合其属性/关系/近况，禁止 OOC。
6. 视角限制：一切以主角所见所闻所想为准。
7. 只输出 JSON，不要其他文字。`
	user := fmt.Sprintf("【本章角色状态卡（场景内角色言行必须符合）】\n%s\n【本章节拍序列】\n%s\n请把节拍细化为场景剧本。", cardsText, beatsText.String())

	raw, err := llm.CallAPITierSync(ctx, in.LLM, "fast", system, user)
	if err != nil {
		return fallbackScripts(in, u, beats), fmt.Errorf("场景剧本化失败(%v)，用规则兜底", err)
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return fallbackScripts(in, u, beats), fmt.Errorf("场景剧本化输出无JSON，用规则兜底")
	}
	var resp struct {
		Scenes []struct {
			BeatType   string `json:"beat_type"`
			Goal       string `json:"goal"`
			Conflict   string `json:"conflict"`
			Dialogue   string `json:"dialogue"`
			Turn       string `json:"turn"`
			Hook       string `json:"hook"`
			SourceDays []int  `json:"source_days"`
		} `json:"scenes"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return fallbackScripts(in, u, beats), fmt.Errorf("场景JSON解析失败(%v)，用规则兜底", err)
	}
	if len(resp.Scenes) == 0 {
		return fallbackScripts(in, u, beats), nil
	}
	var scripts []SceneScript
	for i, sc := range resp.Scenes {
		scripts = append(scripts, SceneScript{
			Num:        i + 1,
			BeatType:   sc.BeatType,
			Goal:       sc.Goal,
			Conflict:   sc.Conflict,
			Characters: cards,
			Dialogue:   sc.Dialogue,
			Turn:       sc.Turn,
			Hook:       sc.Hook,
			SourceDays: sc.SourceDays,
		})
	}
	return scripts, nil
}

// fallbackScripts LLM 失败：每个节拍直接转场景剧本（角色卡注入）。
func fallbackScripts(in *Input, u StoryUnit, beats []Beat) []SceneScript {
	names := map[string]bool{}
	for _, c := range u.Chars {
		names[c] = true
	}
	for _, b := range beats {
		for _, c := range b.Chars {
			names[c] = true
		}
	}
	var charNames []string
	for n := range names {
		charNames = append(charNames, n)
	}
	cards := buildCharCards(charNames, in.Entities, in.Hero)
	var scripts []SceneScript
	for i, b := range beats {
		scripts = append(scripts, SceneScript{
			Num:        i + 1,
			BeatType:   b.Type,
			Goal:       "推进本章目标",
			Conflict:   "主角行动遭遇阻碍或信息变化",
			Characters: cards,
			Dialogue:   "按角色性格展开对话，带信息量与潜台词",
			Turn:       "情况发生变化，主角做出应对",
			Hook:       "",
			SourceDays: []int{b.SourceDay},
		})
	}
	return scripts
}

// ---------- 剧本格式化（供 novel 写手） ----------

// FormatScripts 把场景剧本格式化为写手可读的文本。
func FormatScripts(scripts []SceneScript) string {
	var sb strings.Builder
	sb.WriteString("【本章场景剧本（按顺序写，每个场景独立成段）】\n")
	for i, sc := range scripts {
		sb.WriteString(fmt.Sprintf("\n场景%d（%s）：\n", i+1, sc.BeatType))
		if sc.Goal != "" {
			sb.WriteString("· 目标：" + sc.Goal + "\n")
		}
		if sc.Conflict != "" {
			sb.WriteString("· 冲突：" + sc.Conflict + "\n")
		}
		if sc.Dialogue != "" {
			sb.WriteString("· 对话要点：" + sc.Dialogue + "\n")
		}
		if sc.Turn != "" {
			sb.WriteString("· 转折：" + sc.Turn + "\n")
		}
		if sc.Hook != "" {
			sb.WriteString("· 结尾钩子：" + sc.Hook + "\n")
		}
		if len(sc.SourceDays) > 0 {
			sb.WriteString("· 素材天：" + joinInts(sc.SourceDays) + "\n")
		}
	}
	return sb.String()
}

// FormatCharCards 角色状态卡文本（章节级注入）。
func FormatCharCards(cards []CharacterCard) string {
	return formatCards(cards)
}
