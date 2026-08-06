package narrative

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"worldsim/internal/llm"
)

// ---------- 节拍设计（每章 → Beat[]） ----------

// PlanBeats 为一个章节单元设计剧情节拍（setup→conflict→turn→reveal→payoff→hook）。
// 注入剧情方向（爽点密度）与伏笔调度，让写手有明确的"这一章怎么演"。
func PlanBeats(ctx context.Context, in *Input, u StoryUnit) ([]Beat, error) {
	if in.LLM == nil {
		return fallbackBeats(in, u), nil
	}
	// 本章素材（编年史按天）
	entries := chronicleForDays(in.Chronicle, u.Days)
	// 伏笔调度：本章涉及哪些伏笔（未回收的 + 本章天范围内回收的）
	foreshadowText := foreshadowBlock(in, u)

	var sb strings.Builder
	sb.WriteString("【本章信息】\n")
	sb.WriteString(fmt.Sprintf("章号：%d（模拟Day%d～Day%d）\n", u.Num, u.DayStart, u.DayEnd))
	sb.WriteString("目标：" + u.Goal + "\n")
	if u.Villain != "" {
		sb.WriteString("反派动作：" + u.Villain + "\n")
	}
	if u.Payoff != "" {
		sb.WriteString("本章爽点：" + u.Payoff + "\n")
	}
	if u.TimeHint != "" {
		sb.WriteString("时间跨度：" + u.TimeHint + "\n")
	}
	if len(u.Milestones) > 0 {
		sb.WriteString("关键节点：" + strings.Join(u.Milestones, " → ") + "\n")
	}
	if len(u.Chars) > 0 {
		sb.WriteString("登场角色：" + strings.Join(u.Chars, "、") + "\n")
	}
	sb.WriteString("\n【本章素材（编年史，按天）】\n" + entryTexts(entries))
	dirText := directionText(in.Direction)
	if dirText != "" {
		sb.WriteString("\n【用户剧情方向】\n" + dirText + "\n")
	}
	if foreshadowText != "" {
		sb.WriteString(foreshadowText)
	}

	system := `你是网文小说的"节拍设计器"。你拿到一章的素材（目标/反派/爽点/编年史事件），你的任务是把这一章设计成**剧情节拍序列**——写手按节拍展开场景，不是翻译素材。
输出严格 JSON，格式：
{"beats":[{"type":"setup|conflict|turn|reveal|payoff|hook","summary":"本拍内容摘要（谁在做什么、发生什么，具体到画面/动作/对话要点）","location":"场景地点（从世界背景取）","chars":["登场角色"],"source_day":<对应素材天,整数，-1=写手自由发挥>}]}
节拍设计铁律：
1. 一章 4~7 个节拍，必须覆盖"起承转合"：开场钩子（setup）→ 冲突/推进（conflict）→ 转折（turn）→ 揭示/收获（reveal/payoff）→ 结尾悬念（hook）。
2. 每个节拍都要有"事"：有动作、有对话、有变化。禁止空氛围节拍。
3. 素材是原料不是剧本：平淡的日子压缩成一句过渡（放 summary 里注明"过渡"），戏剧性时刻放大成完整节拍。
4. 结尾 hook 必须具体（谁/什么/在哪），禁止空泛预感。
5. 爽点节奏：按用户要求的爽点密度（high=每章有 payoff；normal=张弛；low=蓄力），该给的爽点（打脸/收获/突破/情感）设计成 payoff 节拍，写足。
6. 视角限制：所有节拍以主角亲眼所见/亲耳所闻/心中所想为准，禁止写主角不知道的事。
7. 时间跨度自由：跳年/跳月都可以，用"三年后"这类过渡写在 setup 或 conflict 的 summary 里。
8. 只输出 JSON，不要其他文字。`
	user := sb.String() + "\n请设计这一章的节拍。"
	raw, err := llm.CallAPITierSync(ctx, in.LLM, "fast", system, user)
	if err != nil {
		return fallbackBeats(in, u), fmt.Errorf("节拍设计失败(%v)，用规则兜底", err)
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return fallbackBeats(in, u), fmt.Errorf("节拍设计输出无JSON，用规则兜底")
	}
	var resp struct {
		Beats []struct {
			Type      string   `json:"type"`
			Summary   string   `json:"summary"`
			Location  string   `json:"location"`
			Chars     []string `json:"chars"`
			SourceDay int      `json:"source_day"`
		} `json:"beats"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return fallbackBeats(in, u), fmt.Errorf("节拍JSON解析失败(%v)，用规则兜底", err)
	}
	if len(resp.Beats) == 0 {
		return fallbackBeats(in, u), nil
	}
	var beats []Beat
	for _, b := range resp.Beats {
		beats = append(beats, Beat{
			Type:      b.Type,
			Summary:   b.Summary,
			Location:  b.Location,
			Chars:     b.Chars,
			SourceDay: b.SourceDay,
		})
	}
	return beats, nil
}

// fallbackBeats LLM 失败时：按素材戏剧日生成简化节拍（开场/冲突/收尾钩子）。
func fallbackBeats(in *Input, u StoryUnit) []Beat {
	entries := chronicleForDays(in.Chronicle, u.Days)
	days := daysOf(entries)
	if len(days) == 0 {
		return []Beat{{Type: "setup", Summary: "本章以主角视角展开，交代状态与处境。"}}
	}
	var beats []Beat
	beats = append(beats, Beat{Type: "setup", Summary: "开场切入：主角当前处境与本章要面对的局面。", SourceDay: days[0]})
	if len(days) >= 2 {
		beats = append(beats, Beat{Type: "conflict", Summary: "主角主动行动，遭遇阻碍或对话交锋。", SourceDay: days[len(days)/2]})
	}
	if len(days) >= 3 {
		beats = append(beats, Beat{Type: "turn", Summary: "转折：情况发生变化，主角得到新信息或做出关键选择。", SourceDay: days[len(days)-1]})
	}
	beats = append(beats, Beat{Type: "hook", Summary: "结尾悬念：具体的新威胁或秘密露出一角。", SourceDay: days[len(days)-1]})
	return beats
}

// foreshadowBlock 本章伏笔调度（未回收的 + 本章回收的）。
func foreshadowBlock(in *Input, u StoryUnit) string {
	if len(in.Foreshadows) == 0 {
		return ""
	}
	var open, resolved []string
	for name, f := range in.Foreshadows {
		if f.Status == "planted" || f.Status == "progressing" {
			// 只提本章天范围内的（埋设日 <= 本章结束）
			if f.Planted <= u.DayEnd {
				open = append(open, name+"（埋设Day"+itoa(f.Planted)+"）")
			}
		} else if f.Status == "resolved" && f.Resolved >= u.DayStart && f.Resolved <= u.DayEnd {
			resolved = append(resolved, name+"（Day"+itoa(f.Resolved)+"回收）")
		}
	}
	var sb strings.Builder
	if len(open) > 0 {
		sb.WriteString("\n【未回收伏笔（本章可推进，别忘）】\n" + strings.Join(open, "；") + "\n")
	}
	if len(resolved) > 0 {
		sb.WriteString("\n【本章已回收伏笔（写手必须呈现揭晓时刻）】\n" + strings.Join(resolved, "；") + "\n")
	}
	return sb.String()
}
