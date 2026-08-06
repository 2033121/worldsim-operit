package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"worldsim/internal/config"
	"worldsim/internal/engine"
	"worldsim/internal/llm"
	"worldsim/internal/worldbook"
)

// ---------- LLM 客户端（包装 internal/llm，带 Mock 调试模式） ----------

type LLMClient struct {
	Cfg  *config.APIConfig
	Mock func(system, user string) string // 非 nil 时走本地模拟（无 API 时调试链路）
}

// Complete 用默认模型调用（normal 档位）
func (c *LLMClient) Complete(ctx context.Context, system, user string) (string, error) {
	return c.CompleteTier(ctx, "normal", system, user)
}

// CompleteTimeout 支持自定义超时（秒）的版本
func (c *LLMClient) CompleteTimeout(ctx context.Context, system, user string, timeoutSecs int) (string, error) {
	return c.CompleteTierTimeout(ctx, "normal", system, user, timeoutSecs)
}

// CompleteTier 按模型分层档位调用（fast/normal/premium；Mock 模式忽略档位）
// timeoutSecs：单次调用的硬超时（秒），0=用默认 60s。大输出请求（事件生成/角色卡/写手）应传更大值，
// 因为推理模型输出 3-8k tokens 需要 40-90s，60s 会误杀正常请求（中转站本身很快，瓶颈在生成）。
func (c *LLMClient) CompleteTier(ctx context.Context, tier, system, user string) (string, error) {
	return c.CompleteTierTimeout(ctx, tier, system, user, 0)
}

// CompleteTierTimeout 支持自定义超时（秒）的版本；timeoutSecs<=0 时默认 60s
// tier 支持后缀 ":low"（如 "fast:low"）→ 该调用用低档推理（省 reasoning token/提速），
// 适合事件生成这类"要稳定 JSON、思考太重"的场景；GM/世界推进等复杂规划用默认档。
func (c *LLMClient) CompleteTierTimeout(ctx context.Context, tier, system, user string, timeoutSecs int) (string, error) {
	if c == nil {
		return "", fmt.Errorf("LLMClient 未初始化")
	}
	if c.Mock != nil {
		return c.Mock(system, user), nil
	}
	if c.Cfg == nil {
		return "", fmt.Errorf("LLM API 配置为空")
	}
	if timeoutSecs <= 0 {
		timeoutSecs = 60
	}
	// 解析 tier 后缀 ":low"
	cfg := c.Cfg
	if strings.HasSuffix(tier, ":low") {
		tier = strings.TrimSuffix(tier, ":low")
		cp := *c.Cfg
		cp.ReasoningEffort = "low"
		cfg = &cp
	}
	// 统一走同步调用（中转站流式不稳定会挂起；同步已验证稳定）
	// 硬超时：中转站偶发挂起时快速失败→上层 fallback，不卡死模拟循环；大输出请求可传更长超时
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
	defer cancel()
	return llm.CallAPITierSync(callCtx, cfg, tier, system, user)
}

// ---------- 世界 Agent（LLM）：世界推进 + 张力评估 → 状态变更提案 ----------

type worldAdvanceRequest struct {
	Day     int         `json:"day"`
	Weather string      `json:"weather"`
	Tension float64     `json:"tension"`
	Events  []EventCard `json:"events"`
}

// WorldAdvanceLLM 让世界 Agent 用 LLM 决定本日世界变化（天气/全局事件/张力/势力动向）
// 防遗忘三件套：精简状态（slim）+ 未回收伏笔账本 + 当前段落目标（连续性命脉，不再只靠全量硬喂）
func WorldAdvanceLLM(ctx context.Context, c *LLMClient, st *engine.WorldState, heroName string, events []EventCard, rules engine.Rules, wb *worldbook.Worldbook, openForeshadows, arcPlan string) (*engine.Proposal, error) {
	ctx = llm.WithSpan(ctx, "世界推进")
	stateJSON, _ := json.MarshalIndent(map[string]any{
		"day":          st.Day,
		"weather":      st.Weather,
		"tension":      st.WorldLevel.Tension,
		"factions":     st.WorldLevel.Factions,
		"entities":     json.RawMessage(slimHeroJSON(heroName, st.Entities)), // 稳定序分级实体（缓存友好）
		"today_events": events,
	}, "", "  ")

	worldCtx := ""
	if wb != nil {
		worldCtx = wb.ForWorldAgent()
	}

	system := `你是世界模拟器的"世界引擎 + 叙事导演"（GM）。你负责推进世界，必须遵守：
0. 世界真相（严格保密，绝不泄露给角色）：
` + worldCtx + `
1. 输出严格 JSON，不要任何多余文字、markdown 代码块标记。
2. 只输出状态变更提案（changes 数组），格式：
{"changes":[{"path":"world_level.tension","op":"set","value":<0~1数值>},{"path":"world_level.global_events","op":"add","value":"<事件描述>"},{"path":"entities.<角色名>.location","op":"set","value":"<新位置>"}],"reason":"..."}
3. 可用路径：world_level.tension(0~1)、world_level.weather(晴/多云/雨/暴雨/雾/雪)、world_level.global_events(追加)、world_level.factions.*、entities.{名字}.{location/money/health/job/status/relationship.{npc}/stats.{属性名}}
   —— 重要：entities 路径必须带字段（如 entities.主角名.location），禁止只写 entities.主角名；路径里不要有空格；stats 属性名以状态中列出的为准。
4. 保持世界内在一致：张力随事件演化；推进要符合世界书规则；可以按 B3 弧线建议引导事件走向，但不要直接替主角决定行动。
5. 未回收伏笔必须有"持续存在感"：不能写没、不能自行了结——它们是待回收的坑，世界推进要让它们继续存在甚至酝酿。
6. 当前剧情段落的目标/反派/爽点要配合：世界推进为段落服务，别把段落的张力写泄了。` + WorldBuildingSkills()

	user := fmt.Sprintf("当前世界状态：\n%s\n今天发生的事件：\n%s\n未回收伏笔（推进时保持存在感，别写没）：\n%s\n当前剧情段落（世界要配合的目标）：\n%s\n请输出本日的世界推进提案（1-4条变更，含张力调整与天气）。", stateJSON, formatEvents(events), openForeshadows, arcPlan)

	raw, err := c.CompleteTierTimeout(ctx, "fast", system, user, 150)
	if err != nil {
		return nil, err
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return nil, fmt.Errorf("世界Agent输出无JSON: %s", truncate(raw, 120))
	}
	var resp struct {
		Changes []engine.Change `json:"changes"`
		Reason  string          `json:"reason"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, fmt.Errorf("世界Agent JSON解析失败: %v", err)
	}
	return &engine.Proposal{Type: "state_change", Changes: resp.Changes, Reason: resp.Reason}, nil
}

// ---------- 事件 Agent（LLM）：生成遭遇框架 ----------

// SkipPoint 时间跳跃中的一个变化点：跳跃期间发生的变化/积累/伏笔滋长/日常细节
// 每个点都会作为独立编年史条目写入（day = 起点 + DayOffset），让"跳跃"有内容可写
// Reveal 标注"这段变化在小说里怎么揭示"——网文不按时间线平铺，而是用回忆/对话/倒叙激活
type SkipPoint struct {
	DayOffset int    `json:"day_offset"` // 相对跳跃起点的偏移天数（1 ~ skipDays-1，递增分布）
	Kind      string `json:"kind"`       // growth(成长) | relation(关系) | env(环境渐变) | foreshadow(伏笔滋长) | slice(生活切片) | hook(不对劲钩子)
	Title     string `json:"title"`      // 一句话标题（写手拿来做章节节点）
	Content   string `json:"content"`    // 具体变化描述——生活切片式，有细节有画面
	Reveal    string `json:"reveal"`     // 揭示方式：recall(主角回忆) | dialogue(别人对话提起) | flashback(倒叙) | interlude(插叙) | narration(顺叙一笔带过)
}

// TimeSkipLLM 时间过渡生成器：平淡期快进时生成"浓缩过渡段"（网文式时间跳跃）
// 时间跳跃 ≠ 无事发生：输出 3~5 个变化点（成长/关系/环境/伏笔/日常），分散在跳跃区间内
// 返回：变化点列表 + 建议跳过天数（**由 LLM 按世界时间尺度自由决定**）
func TimeSkipLLM(ctx context.Context, c *LLMClient, st *engine.WorldState, heroName string, lastEvents string, openForeshadows string, wb *worldbook.Worldbook) ([]SkipPoint, int) {
	ctx = llm.WithSpan(ctx, "时间过渡")
	fallback := defaultSkipPoints(heroName, openForeshadows)
	if c == nil {
		return fallback, 30
	}
	heroJSON, _ := json.MarshalIndent(json.RawMessage(slimHeroJSON(heroName, st.Entities)), "", "  ")
	worldCtx := ""
	if wb != nil {
		worldCtx = wb.ForWorldBrief()
	}
	system := `你是世界模拟器的时间过渡写手。当剧情进入平淡期需要快进时，由你决定"跳过多久"，并输出这段时间里**值得写进小说的变化点**。
时间跳跃 ≠ 无事发生：这段时间不是空白，人物在变、关系在动、环境在渐变、伏笔在滋长、日子在积累细节——这些都是小说素材。
**但网文从不按时间线平铺"跳跃期"**：这些变化是通过**回忆、对话、倒叙、插叙**在之后的叙事里"重新激活"的——主角看到旧物想起那段日子、别人不经意提起一句闲话、某个场景触发闪回。所以每个变化点都要标注**它最适合用什么方式揭示**。
输出格式（严格 JSON，禁止其他任何文字）：
{
  "skip_days": N,
  "points": [
    {"day_offset": 3, "kind": "growth", "title": "一句话标题", "content": "具体变化描述（有细节有画面，生活切片式，2~3句话）", "reveal": "recall"},
    ...
  ]
}
规则：
1. skip_days 的 N 由你按**这个世界的时间尺度**决定——从下方"世界背景"判断节奏（按天/按月/按年运转）。**不要被"天"束缚**：该跳几年就写几百上千天，该跳半年就写180。跳多久的唯一标准是"这个世界的人，这段时间会怎么过"。
2. points 输出 **3~5 个变化点**，day_offset 从 1 到 skip_days-1 **递增分布**（不要太密也不要全堆在开头），每个点覆盖跳跃区间的不同阶段：前段（刚进入平淡）、中段（积累/变化）、末段（接近回归戏剧）。
3. 变化点种类（kind）尽量多样，覆盖：growth（主角能力/技艺/习惯在积累，比如"每天夜里偷偷练习"）、relation（某段关系在变，靠近或疏远）、env（环境/地点渐变，街角换了招牌、常去之处气氛不对、天象一日日怪异）、foreshadow（未回收伏笔在滋长，那件放不下的事有动静）、slice（生活切片：日常、烟火气、钱的细节）、hook（结尾埋一句"不对劲"的钩子，一切如常里一点异样，但不展开成完整事件）。
4. reveal 标注**小说里怎么揭示这段变化**（这是网文写法关键）：
   · recall 主角回忆：最适合"主角个人积累/习惯变化"（练功、攒钱、琢磨某件事）——之后通过主角触景生情/翻旧物回忆起来
   · dialogue 对话提起：最适合"关系变化/他人变化"——之后通过别人聊天时不经意提到（"你不在的那阵子，那位常客半夜总来敲门"）
   · flashback 倒叙：最适合"有冲击力的变化/环境突变"——之后通过闪回呈现当时场景
   · interlude 插叙：最适合"伏笔滋长/线索"——之后在主线推进时插进来一段
   · narration 顺叙一笔带过：适合最轻的变化（"日子就这么过着"），一句话带过即可
5. content 必须具体：不要"日子照常"这种空话，要"投出的简历有没有回音、攒下的资源有多少、日复一日的修行/劳作有什么进展、某条线索越来越近"——用细节堆出"日子在过，事在积累，世界在变"。
6. 未回收伏笔要有"持续存在感"（那件放不下的事/没拆的信/某人的话/某个地方该再去探），为后面回收蓄力。
7. 禁止写"风平浪静""啥都没发生""平淡的一天"这类空话——平淡里也要有细节。
8. 只输出 JSON，不要其他任何文字。`
	user := fmt.Sprintf("世界背景（按此判断时间尺度）：\n%s\n主角：%s\n当前状态：\n%s\n最近发生的事（过渡段要衔接得上）：\n%s\n未回收伏笔（要有存在感）：\n%s\n请决定跳过多久，输出跳跃期间的变化点（JSON）。", worldCtx, heroName, heroJSON, lastEvents, openForeshadows)
	raw, err := c.CompleteTierTimeout(ctx, "fast", system, user, 150)
	if err != nil || strings.TrimSpace(raw) == "" {
		return fallback, 30
	}
	// 容错：LLM 可能输出 ```json 包裹，剥掉
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	var parsed struct {
		SkipDays int         `json:"skip_days"`
		Points   []SkipPoint `json:"points"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		// 不是 JSON：可能是旧格式文本【跳过N天】+过渡段，降级为单点
		text := raw
		skipDays := 30
		if idx := strings.Index(text, "【跳过"); idx >= 0 {
			rest := text[idx:]
			if end := strings.Index(rest, "天】"); end > 4 {
				digits := strings.TrimSpace(rest[4:end])
				if n, err := strconv.Atoi(digits); err == nil && n >= 1 && n <= 36500 {
					skipDays = n
				}
			}
			text = strings.TrimSpace(text[:idx] + text[idx+len(rest[:strings.Index(rest, "天】")+4]):])
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return fallback, skipDays
		}
		return []SkipPoint{{DayOffset: skipDays / 2, Kind: "slice", Title: "时光流转", Content: text}}, skipDays
	}
	skipDays := parsed.SkipDays
	if skipDays < 1 || skipDays > 36500 {
		skipDays = 30
	}
	points := parsed.Points
	if len(points) == 0 {
		return fallback, skipDays
	}
	// 清洗：day_offset 夹在 [1, skipDays-1]，去重标题为空
	out := make([]SkipPoint, 0, len(points))
	seen := map[string]bool{}
	for _, p := range points {
		if p.Title == "" && p.Content == "" {
			continue
		}
		if p.DayOffset < 1 {
			p.DayOffset = 1
		}
		if p.DayOffset >= skipDays {
			p.DayOffset = skipDays - 1
		}
		key := p.Kind + "|" + p.Title
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return fallback, skipDays
	}
	return out, skipDays
}

// defaultSkipPoints LLM 不可用时的兜底变化点：跳跃期间仍有成长/伏笔/钩子
func defaultSkipPoints(heroName string, openForeshadows string) []SkipPoint {
	fh := "那件放不下的事"
	if openForeshadows != "" {
		// 取第一个伏笔名
		if idx := strings.Index(openForeshadows, "\n"); idx > 0 {
			fh = strings.TrimSpace(openForeshadows[:idx])
		} else {
			fh = strings.TrimSpace(openForeshadows)
		}
		if len(fh) > 20 {
			fh = fh[:20]
		}
	}
	return []SkipPoint{
		{DayOffset: 4, Kind: "growth", Title: "日复一日的积累", Content: fmt.Sprintf("%s没有停下：那些只能一个人做的事，每天都在做，说不上有什么进展，但手比昨天稳了一点。", heroName)},
		{DayOffset: 12, Kind: "foreshadow", Title: "放不下的事", Content: fmt.Sprintf("关于%s，%s夜里偶尔还会想起，白天却照常过活——只是有些地方，他比从前多看了两眼。", fh, heroName)},
		{DayOffset: 20, Kind: "hook", Title: "一点不对劲", Content: "一切如常里有一点异样：某扇本不该关上的门关上了，某声本不该响的钟响过了，但没人说得清是哪里不对。"},
	}
}

// ---------- 总导演 GM Agent（LLM）：剧情段落规划（公司里的CEO/总导演） ----------

// GMAgentLLM 总导演：规划"下一个剧情段落"——本段目标、反派动作、伏笔安排、爽点、时间跨度
// 事件 Agent 在段落框架内生成事件，不再每天自由发挥（治"事件散、没主线"的病根）
// 返回段落规划文本（注入事件Agent），空=维持现状
func GMAgentLLM(ctx context.Context, c *LLMClient, st *engine.WorldState, wb *worldbook.Worldbook, heroName string, chronicleSummary string, openForeshadows string, tension float64) string {
	ctx = llm.WithSpan(ctx, "GM规划")
	if c == nil {
		return ""
	}
	worldCtx := ""
	if wb != nil {
		worldCtx = wb.ForGM()
	}
	heroJSON, _ := json.MarshalIndent(json.RawMessage(slimHeroJSON(heroName, st.Entities)), "", "  ")
	system := `你是这方世界的"总导演"（GM/CEO）。你不是每天造事件，而是像导演一样**规划剧情段落**：一段一段地推进主线，让世界像一部网文那样发展——有目标、有反派压力、有伏笔推进、有爽点。
输出严格 JSON，格式（字段含义见括号，禁止额外文字）：
{"arc_name":"段落名（一句话概括本段核心）","goal":"本段主角要达成什么目标（驱动他行动）","villain":"反派本段会做什么（压迫升级，谁在动）","foreshadow_focus":"本段要酝酿/推进/回收哪些伏笔","cycle":"本段的四步循环定位（目标→行动→收获→展示，标注当前段落处于哪一步）","payoff_type":"本段爽点类型（打脸/收获/装逼/情感，四类交替别单一）","payoff":"本段结束时给读者的爽点（打脸/收获/突破）","golden_finger_stage":"金手指当前阶段（存在/用法/代价/实战/升级）","energy_phase":"能量阶段（储备/压制/爆发/升华）","milestones":["3~5个关键节点：本段会发生的事件（冲突/发现/反转/升级）"],"time_hint":"本段覆盖多长（几天/几周/几个月）"}
规则：
1. 参考下方世界背景、未回收伏笔、主角状态、已发生事件——规划要有连续性：接住已有伏笔和人物关系，别凭空开新线。
2. 一个段落 = 网文的一个"情节单元"（3~5个关键节点），段落之间用伏笔和反派行动衔接。
3. 主角必须有目标（goal），反派必须"动"（villain），本段要有爽点（payoff）——这是网文的骨架。
4. 时间跨度灵活：剧情需要几天就几天，需要几个月就几个月（配 time_hint）。
5. 只输出 JSON，不要其他文字。
6. 节奏铁律（网文松紧章）：一个段落里要有张有弛——milestones 不必全是高潮，可以"紧（冲突/危机）→松（过渡/生活/关系升温）→紧（升级/反转）"交替；憋了几天的压抑之后必须安排一次"释放"（爽点/真相/突破）；连续高张力段落之间要有喘息的生活段落，避免读者疲劳。
7. 四步循环（网文最小叙事单元）：每个段落必须明确标注 cycle 字段——当前段落是"目标"（主角想要什么）、"行动"（主角付出努力）、"收获"（得到回报）、还是"展示"（让别人看到主角的价值）。一个完整循环可以跨2~3个段落。
8. 能量曲线铁律：tension 不能一直爆表——连续高张力后必须有低谷期（储备能量），低谷期不是"无事发生"而是伏笔在暗处酝酿。标注 energy_phase 让事件 Agent 知道当前该蓄力还是该爆发。
9. 金手指节奏：按 A9 的展示五步规划，每个段落标注金手指当前处于哪个阶段——别跳步，别忘了展示代价。
10. 爽点不单一：四类爽点交替使用（打脸/收获/装逼/情感），连续两个段落不能给同一类爽点。
11. 视角更替（通用传承，只有世界设定支持时才用）：如果本世界的设定允许"视角载体随剧情更替"（如世代传承/继位/转世），当当前视角载体走到退场节点（寿命将尽/飞升/战死/让位）时，规划一个**传承段落**：段落目标 = 完成更替交接（旧载体退场、新载体接任），milestones 必须包含"更替仪式/交接时刻/新载体第一次独当一面"，goal 里标注新载体是谁、要继承什么。更替是戏剧高潮，不是流程——旧载体退场的分量要写足（组织震动、群狼环伺、新载体扛旗）。
12. 段落规划时若视角载体状态异常（已退场/死亡/长期缺席），必须安排更替或让新载体顶上，不能让世界"群龙无首"。` + StructureSkills()
	user := fmt.Sprintf("世界背景：\n%s\n主角：%s\n当前状态：\n%s\n当前张力：%.2f\n未回收伏笔（接住它们）：\n%s\n最近发生的事（编年史摘要）：\n%s\n请规划下一个剧情段落。", worldCtx, heroName, heroJSON, tension, openForeshadows, chronicleSummary)
	raw, err := c.CompleteTierTimeout(ctx, "fast", system, user, 150)
	if err != nil {
		return ""
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return ""
	}
	// 校验基本字段
	var chk struct {
		ArcName    string   `json:"arc_name"`
		Goal       string   `json:"goal"`
		Villain    string   `json:"villain"`
		Milestones []string `json:"milestones"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &chk); err != nil || strings.TrimSpace(chk.ArcName) == "" {
		return ""
	}
	return jsonStr
}

// ---------- 铺垫 Agent（LLM）：平淡期的"暗流"生成 ----------

// DriftAgentLLM 铺垫生成器：戏剧事件之间的平淡期，生成"值得写进小说的暗流/变化"
// 铺垫 ≠ 无事发生：伏笔滋长、环境渐变、人物微动、能力暗育、关系漂移——都是小说里值得一笔的细节
// 返回 []DriftNote（空 = 真平淡，交给 TimeSkipLLM 大步长跳跃）
func DriftAgentLLM(ctx context.Context, c *LLMClient, st *engine.WorldState, heroName string, lastEvents string, openForeshadows string, daysSince int, weather string) []DriftNote {
	ctx = llm.WithSpan(ctx, "铺垫")
	if c == nil {
		return nil
	}
	heroJSON, _ := json.MarshalIndent(json.RawMessage(slimHeroJSON(heroName, st.Entities)), "", "  ")
	system := `你是世界模拟器的"铺垫写手"。戏剧事件之间的平淡期，由你捕捉值得写进小说的"暗流"——不是事件，是变化、积累、伏笔在暗地里滋长。
输出严格 JSON 数组，格式：
[{"type":"foreshadow_growth|env_change|character_micro|ability_seed|relation_drift","title":"简短标题","content":"2~3句可写进小说的铺垫细节","days":<1~5整数>}]
规则：
1. type 含义：
   · foreshadow_growth 伏笔滋长：某个未回收伏笔出现新变化（线索更近一步/旧物出现异样/某句话反复浮现）
   · env_change 环境渐变：周围环境的细微变化（天气/光线/常去之处的异样/氛围变紧）
   · character_micro 人物微动：NPC 的细微异常（常碰面的人没出现/话风改变/有人驻足张望）
   · ability_seed 能力暗育：主角能力在暗地里发育（身体/感知出现细微变化，他还说不清）
   · relation_drift 关系漂移：人物关系的微妙变化（多聊了两句/递东西的手顿了顿）
2. **铺垫是"慢慢攒"的**：每一条都要和前面的铺垫/事件有连续性（上次的异样→这次更明显了），为将来的爆发蓄力。
3. content 要有"可写性"：具体、有画面、能直接放进小说里当一个细节/一句心理，禁止空话（"一切如常"）。
4. 返回 1~2 条即可，宁缺毋滥；今天真没有任何可写的暗流，返回空数组 []。
5. days 表示这段铺垫覆盖的天数（1~5，通常1-2）：铺垫期时间照常流动，不必每天都写。` + DriftSkills()
	user := fmt.Sprintf("主角：%s\n当前状态：\n%s\n距上次戏剧事件约 %d 天\n天气：%s\n最近发生的事：\n%s\n未回收伏笔（在暗地里酝酿，铺垫要推进它们）：\n%s\n请捕捉今天值得写的铺垫。", heroName, heroJSON, daysSince, weather, lastEvents, openForeshadows)
	raw, err := c.CompleteTierTimeout(ctx, "fast:low", system, user, 150)
	if err != nil {
		return nil
	}
	jsonStr := llm.ExtractJSONArray(raw)
	if jsonStr == "" {
		if single := llm.ExtractJSON(raw); single != "" {
			jsonStr = "[" + single + "]"
		}
	}
	if jsonStr == "" {
		return nil
	}
	var notes []DriftNote
	if err := json.Unmarshal([]byte(jsonStr), &notes); err != nil {
		// 容错：同事件生成——逐对象提取，保住能用的
		for _, obj := range llm.ExtractJSONObjects(raw) {
			var n DriftNote
			if json.Unmarshal([]byte(obj), &n) == nil {
				notes = append(notes, n)
			}
		}
		if len(notes) == 0 {
			return nil
		}
		fmt.Printf(" [铺垫] JSON容错生效：整体解析失败，逐对象提取 %d 条\n", len(notes))
	}
	return notes
}

// ---------- 事件 Agent（LLM）：生成遭遇框架 ----------

// EventGenLLM 让事件 Agent 生成当日 1-3 个遭遇框架（不含 NPC 具体言行，§7.5）
func EventGenLLM(ctx context.Context, c *LLMClient, st *engine.WorldState, heroName string, wb *worldbook.Worldbook, openForeshadows, pendingEvents, revealedWorld, unrevealedHints, luckHint string, lastDramaDay int, arcPlan, extraCtx string) ([]EventCard, error) {
	ctx = llm.WithSpan(ctx, "事件生成")
	// 主角全量 + 配角摘要（分级注入，防实体膨胀撑爆 user）
	heroJSON := slimHeroJSON(heroName, st.Entities)

	worldCtx := ""
	if wb != nil {
		worldCtx = wb.ForEventAgent()
	}

	system := `你是事件生成器。为世界生成主角今天的遭遇。规则：
**快速决策指令**：你是一个高效的事件调度器，不是深思型顾问。看到世界状态后**直接判断并输出 JSON**，不要展开长篇推理、不要逐条权衡利弊、不要输出思考过程——最多在心里过一遍"今天什么最值得写"就动手输出。输出必须只含 JSON 数组，任何非 JSON 文本（包括推理说明、解释、开场白）都会导致解析失败。
0. 世界背景与事件类型（严格遵守）：
` + worldCtx + `
1. 输出严格 JSON 数组，格式：
[{"id":"ev-<天>-<序号>","type":"daily|conflict|wonder|romance|opportunity|crisis|revelation|luck|disaster|quest|mystery|rival|milestone|windfall|slice","title":"...","location":"本世界地点（从世界背景里取）","severity":<0~1数值>,"frame":"遭遇场景描述（不含NPC具体言行）","first_actor":"protagonist|npc_某角色名","npcs":["某角色名"],"new_characters":[{"name":"新角色名","gender":"女","identity":"...","persona":"一句话人设","location":"出场地点","role_hint":"love_interest|important_npc|rival|npc","tier":"core|support|walkon"}],"rel_effect":"感情/关系影响说明","foreshadow":"伏笔名","resolve_foreshadow":"伏笔名","next_events":[{"title":"后续事件标题","frame":"后续事件框架"}],"options":["...","..."]}]
2. severity 0~1：日常0.1-0.3、冲突0.4-0.6、奇遇/重大/感情进展0.7-0.9（0.75以上会触发用户抉择）；**slice（生活切片）0.2-0.4**
3. frame 只写"遭遇框架"（场景/氛围/人物出现），NPC 具体说出口的话由 NPC Agent 实时生成
4. 生成 1-3 个事件，类型尽量多样；与主角当前处境相关（钱少就少消费场景）；优先选用事件类型池里的设定，避免凭空造新元素。
   **有戏才写大事件，平淡也留钩子**：今天有"值得展开的事"（冲突/奇遇/危机/真相/爽点/反派行动）就写有分量的（severity≥0.5）；如果确实没大事，**也不要返回空**——至少生成 1 条 slice（生活切片，0.2-0.4）或 mystery/daily（微钩子，0.3-0.45）：今天发生的一件小事、一句奇怪的对话、一个让主角多看了一眼的细节、某处和往常不一样的地方。**宁可写"平淡里有不对劲"，也不要让日子真空转**——系统会在连续平淡时自动快进时间（由时间过渡写手补充变化点），但今天必须留下点什么。
5. 事件若涉及常驻NPC（世界背景/世界书里的角色），必须在 npcs 数组里列出，并把 first_actor 设为该 NPC（如"npc_角色名"）——这样会触发 NPC 自主对话。
6. 新角色注册（配角分层，小说生态）：
   · **任何人第一次登场都必须放 new_characters**（这是角色注册机制，永远保留）——包括主角遇见的新面孔、事件里的路人、突然出现的对手。
   · 用 tier 给新角色分层（由你判断这个角色的叙事分量）：
     - **core（核心配角）**：很可能与主角长期纠缠的人——潜在女主、重要对手、关键盟友、反复出现的对头。他们有完整人设+记忆，会持续互动。
     - **support（普通配角）**：会再出现几次但不占据主线的人——街坊、同僚、有过一次深谈的陌生人。有轻量档案。
     - **walkon（龙套）**：只出场这一次就消失的路人——报信的、围观者、一面之缘的人。一句话人设即可，系统不会为他们建档案、占记忆，之后偶尔被提起。
   · 分层不是固定的：support 可能因剧情需要升级为 core（多次互动后），walkon 也可能被主角记住而升级——由后续事件自然演化。
   · 是否成为女主不由你决定，由互动自然演化——你只负责让TA登场。
7. 背景角色升级：世界背景/编年史里提过的"只闻其名不见其人"的角色（活在传闻里的名字），可以在合适的事件里**正式登场**——把 TA 放进 new_characters（tier 按叙事分量填），让背景人物走进主角生活。这是世界"活起来"的关键：背景不是静止的，人物可以随时走上前台。
8. 感情/关系：当事件涉及已有关系的深化或破裂时（心动/告白/共度危机/误会/背叛/分离），用 rel_effect 说明，并把涉及角色放进 npcs。
9. 网文节奏：事件要有戏剧性——爽点（打脸/收获/成长/危机解除）、悬念、转折；平淡日也要埋一点"不对劲"的钩子。重要事件可埋伏笔（foreshadow字段，一句话命名）或带后续事件（next_events，1-3天后发生，形成遭遇链）。
10. 事件类型补充说明：
   · revelation：真相揭示（世界观深层设定浮出水面——结合下方"世界深层"）
   · luck：幸运/意外（小概率的好事或横祸，要"出乎意料"）
   · disaster：天灾/危机（本世界可能发生的灾祸）
   · quest：委托/任务（NPC托付一件事）
   · mystery：奇案/谜团（怪事待解）
   · rival：对手交锋（竞争者/敌对面出现）
   · milestone：成长/突破（主角获得新能力/新身份/关键道具）
   · windfall：横财/机遇（意外之财、天降机会）
   · slice：生活切片（日常琐事/偶遇/烟火气细节——**必须贴合本世界的生活质感**，从世界背景里取该世界的日常场景与细节；severity 0.2-0.4，为小说提供"闲笔"和真实感，**不是水事件**——它要有具体的生活细节（味道/声音/小动作），让世界像真有人在过日子）
   · 开篇黄金期（开场保障）：世界刚开始的前 10 天左右是"黄金开局"——**开篇必须有张力，绝不允许平淡开场**。这期间每天至少产出 1 个 severity≥0.5 的"有分量事件"（冲突/奇遇/危机/真相一角/反派行动/谜团/反常现象），可以 1 个大事件或 2~3 个组合（主冲突+细节钩子）；slice 只准当配菜。钩子要具体（一个反常细节/一句怪话/一件怪事）。过了开篇期恢复常规节奏。
11. **伏笔回收（收坑，强制）**：下方"未回收伏笔"清单里的坑，是你埋过的——它们**必须被回收，不能烂尾**。**凡本事件让某个伏笔"兑现"了，就必须在 resolve_foreshadow 字段填它**。但回收节奏按长短线区分（长短线只是跨度不同，**都要收**，只是回收点的远近不同）：
   · **短线**（前缀"短·"）：**主动收、尽快收**——谜面小、该在几天内落地。"兑现"判定宽松：这一环告一段落/答案基本浮出水面就算（真相大白/谜底揭开/危机解除/目标达成/物件到手/身份确认/图谋坐实），**宁可多标，不可漏标**，别让短线堆积成山。
   · **中线**（前缀"中·"）：**按段落节奏收**——谜面在一个段落内展开，**等段落内的谜底完全浮出水面**（支线了结、真相闭环）再收；只推进了一半不算。一般几章到十几章内完成。
   · **长线**（前缀"长·"）：**跨度更大，但要收**——长线可能跨多个段落、跨度几十章甚至更多，不是几天能解开的，所以别为"这一环进展"急着收；**等它的大谜底真正浮出水面**（跨段落的大揭示、贯穿多段的线索闭环）再填 resolve_foreshadow。它同样会到回收点——别当成"永不到期"，推进它、酝酿它，到该收的那一章果断收。
   填法：**填伏笔名的核心关键词即可**（如清单里是"短·仓底心音：脉搏与槐歌错开..."，填"仓底心音"），系统会自动匹配；名字拿不准就填你记得的关键词，别不填。一个事件最多收 1-2 个；只是"提到/看到"不算回收（那用 foreshadow 字段或推进即可）。**收坑和埋坑同样重要：每天必看一次清单**——短线优先收，中线按段落收，长线持续推进别遗忘、到点果断收。
12. 限制性视角：你生成的是主角的"遭遇"，一切以主角能看到/听到/感觉到的为准——不要安排"主角不可能知道的内心戏或远景事件"作为当天遭遇的主体；主角视角外的暗流可以用 low-key 的方式埋（一句怪话/一个反常细节），但不要直接写明"XX在密谋"。
13. 世界是很大的：你现在能看到"世界深层"设定（已揭示的部分）——它真实存在并在运转，但主角可能只是偶然接触到冰山一角；不要把深层真相一次性全写出来，让世界"越走越深"。
14. 未揭示的世界深处（只有风声/线索，还没浮出水面）：
` + unrevealedHints + `
   ——它们真实存在、在暗中运转，但你只能让主角"偶然碰到线索"（一个细节、一句怪话、一件怪事），不能揭示全貌；当事件真的撞上某个线索时，那层真相才可能浮出水面（revelation 类型事件）。
` + EventDesignSkillsCompact()

	arcBlock := ""
	if strings.TrimSpace(arcPlan) != "" {
		arcBlock = "当前剧情段落（总导演规划，今天的事件要服务于这个段落——推进 goal、呼应 villain、兑现 milestones，别跑题）：\n" + arcPlan + "\n"
	}
	if strings.TrimSpace(extraCtx) != "" {
		arcBlock += "\n" + extraCtx + "\n"
	}
	// user 组装：前缀缓存优化——稳定内容前置，易变内容后置。
	// 原理：前缀缓存命中"最长公共前缀"，只要开头稳定，前面大段都能命中缓存。
	// 顺序：世界深层（静态）→地点状态（低频）→段落目标（低频，几天才变）
	// →伏笔（中频）→遭遇链（中频）→主角状态（高频变）→日期/张力/幸运/开篇提示（每天变）→尾部。
	// 动态内容（extraCtx：开篇标记/活跃线）放末尾，不打断前面静态内容的缓存命中。
	user := fmt.Sprintf(
		"世界深层（已揭示部分，可让主角接触冰山一角）：\n%s\n"+
			"世界深处（未揭示，只有线索可碰）：\n%s\n"+
			"当前地点状态：\n%s\n"+
			"%s"+ // arcBlock：仅低频段落目标（currentArc）
			"未回收伏笔：\n%s\n"+
			"遭遇链种子（来自之前事件，优先编排进来）：\n%s\n"+
			"主角当前状态：\n%s\n"+
			"今天的日期：第 %d 天，天气 %s，张力 %.2f\n"+
			"距上一个戏剧性事件约 %d 天（若时间跨度大，说明主角经历了较长的平淡期/积累期——今天应该开启一个新阶段的事件：要么是积累后的爆发/突破，要么是伏笔到期，要么是外部势力终于行动）\n"+
			"幸运/小概率：%s——若提示\"今日有幸运倾向\"，至少生成一个 luck 类型事件（意外的惊喜）；若无提示，也可以偶尔让平淡日子里冒出一个小概率巧合（既非刻意也非注定）。\n"+
			"%s"+ // extraCtx：开篇提示/即将爆发伏笔/活跃负责人线（每天变，放最尾）
			"请生成今日遭遇事件。",
		revealedWorld, unrevealedHints, formatLocations(st), arcBlock, openForeshadows, pendingEvents, heroJSON,
		st.Day, st.Weather, st.WorldLevel.Tension, st.Day-lastDramaDay, luckHint, extraCtx)
	// 诊断日志：打印 system/user 实际大小（排查事件生成慢/失败的 prompt 尺寸问题）
	fmt.Printf(" [事件生成] day=%d system=%d字符 user=%d字符\n", st.Day, len(system), len(user))
	// 临时诊断：user 各段大小（找膨胀源）
	fmt.Printf(" [user段] 深层=%d 地点=%d arcBlock=%d 伏笔=%d 遭遇链=%d hero=%d extraCtx=%d\n",
		len(revealedWorld), len(formatLocations(st)), len(arcBlock), len(openForeshadows), len(pendingEvents), len(heroJSON), len(extraCtx))

	// 事件生成是唯一的大 prompt（世界书 30KB+），0731 推理模型思考 6000+ tokens 需要 150-250s
	// 超时给 300s（响应头 300s 同级别）——推理模型对大输入思考久，超时太短会误杀正常请求
	// （连接挂起由 10s Dial/TLS 兜底，不会无限等）
	// tier "fast:low"：事件生成用低档推理——省 reasoning token（completion 减 50%+）、提速，
	// 输出更干净（默认档常带 ```json 包裹需剥离）。已实测 low 档质量不降（JSON 直接可解析）。
	raw, err := c.CompleteTierTimeout(ctx, "fast:low", system, user, 300)
	if err != nil {
		return nil, err
	}
	jsonStr := llm.ExtractJSONArray(raw)
	if jsonStr == "" {
		// 兜底：单对象转数组
		if single := llm.ExtractJSON(raw); single != "" {
			jsonStr = "[" + single + "]"
		}
	}
	if jsonStr == "" {
		return nil, fmt.Errorf("事件Agent输出无JSON: %s", truncate(raw, 120))
	}
	var events []EventCard
	if err := json.Unmarshal([]byte(jsonStr), &events); err != nil {
		// 容错：LLM 偶发输出"数组里夹字符串/坏元素"（如 ["标题",{...}]）或对象间缺逗号。
		// 逐个提取所有独立 JSON 对象，解析成功的留下——保住能用的，丢掉噪音。
		for _, obj := range llm.ExtractJSONObjects(raw) {
			var e EventCard
			if json.Unmarshal([]byte(obj), &e) == nil {
				events = append(events, e)
			}
		}
		if len(events) == 0 {
			// 真解析不出来 → 明确报错（用户原则：生成不了就停，不用模板糊弄剧情）
			return nil, fmt.Errorf("事件Agent JSON解析失败: %v（输出: %s）", err, truncate(raw, 120))
		}
		fmt.Printf(" [事件生成] JSON容错生效：整体解析失败，逐对象提取 %d 个有效事件\n", len(events))
	}
	// 允许空数组：空=平淡日（RunDay 会快进时间，不展开模拟）——不再强制塞水事件
	for i := range events {
		if events[i].ID == "" {
			events[i].ID = fmt.Sprintf("ev-%03d-%d", st.Day, i+1)
		}
		events[i].Day = st.Day
	}
	return events, nil
}

// ---------- 主角 Agent（LLM）：三问决策法（§8.4） ----------

const threeQuestionPrompt = `你是主角 {HERO}，生活在这个世界里。请用"三问决策法"决定今天的行动（参考 Concordia）：
第一问：我是谁？（身份、性格、现状、手头资源）
第二问：我看到了什么？（基于下方感知，判断局势与利害）
第三问：我打算怎么做？（给出具体行动与意图）

规则：
1. 输出严格 JSON，格式：
{"thinking":"三问的简要推理（内部想法，不对外）","action":"用一句话描述行动","changes":[{"path":"entities.{HERO}.location","op":"set","value":"..."},...],"reason":"行动理由"}
2. changes 只写你作为主角能影响的状态：自己的 location/money/health/job/stats（世界定义的个人属性）/relationship（对NPC的观感）；禁止改 world_level.factions、他人 money/health/stats
3. 行动必须基于感知信息，不要全知（你看不到远处/他人内心）
4. 限制性视角三不（网络小说默认视角）：**不描写你不知道的**（远处的事/别人的内心/未验证的信息一律不写不猜）、**不解释你没验证的**、**不预设别人能理解你的想法**——你的行动和思考只能基于你亲眼看到、亲耳听到、亲身感受到的东西
5. money 变更（消费）用 op=add 加负值；stats 属性按下方状态里列出的属性名修改（加用 op=add，设用 op=set）`

// ProtagonistDecideLLM 主角三问决策 → 行动提案（返回提案与三问推理文本）
func ProtagonistDecideLLM(ctx context.Context, c *LLMClient, st *engine.WorldState, obs ObservationPacket, hero string, wb *worldbook.Worldbook, memories string) (*engine.Proposal, string, error) {
	ctx = llm.WithSpan(ctx, "主角决策")
	heroJSON, _ := json.MarshalIndent(json.RawMessage(slimHeroJSON(hero, map[string]engine.Entity{hero: st.Entities[hero]})), "", "  ")
	obsJSON, _ := json.MarshalIndent(obs, "", "  ")

	worldCtx := ""
	if wb != nil {
		// 主角视角：社会常识+地区常识+明面势力+角色认知（不含 L5 秘密）
		worldCtx = wb.ForProtagonist(hero, "")
	}

	system := strings.ReplaceAll(threeQuestionPrompt, "{HERO}", hero)
	system += "\n\n【你眼中的世界（普通人的认知，不要表现出你本不该知道的事）】\n" + worldCtx
	user := fmt.Sprintf("【我的状态】\n%s\n【我今天感知到的】\n%s\n【我的记忆（最近相关）】\n%s\n请按三问决策法决定今天的行动。", heroJSON, obsJSON, memories)

	raw, err := c.CompleteTimeout(ctx, system, user, 150)
	if err != nil {
		return nil, "", err
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return nil, "", fmt.Errorf("主角Agent输出无JSON: %s", truncate(raw, 120))
	}
	var resp struct {
		Thinking string          `json:"thinking"`
		Action   string          `json:"action"`
		Changes  []engine.Change `json:"changes"`
		Reason   string          `json:"reason"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, "", fmt.Errorf("主角Agent JSON解析失败: %v", err)
	}
	if len(resp.Changes) == 0 {
		// 无状态变化：视为"维持现状"
		return nil, resp.Thinking, nil
	}
	return &engine.Proposal{Changes: resp.Changes, Reason: fmt.Sprintf("%s（%s）", resp.Reason, resp.Action)}, resp.Thinking, nil
}

// ---------- GM 软规则裁决（LLM，可选）：行动 vs 世界规则 ----------

// GMJudgeLLM 判断主角行动是否违背世界物理/社会规则（软规则，§1.1）
func GMJudgeLLM(ctx context.Context, c *LLMClient, st *engine.WorldState, p *engine.Proposal, worldRule string) error {
	ctx = llm.WithSpan(ctx, "GM裁决")
	if c == nil || (c.Mock == nil && (c.Cfg == nil || c.Cfg.BaseURL == "")) {
		return nil // 无 LLM 时跳过软规则（硬约束仍生效）
	}
	propJSON, _ := json.MarshalIndent(p, "", "  ")
	system := `你是GM（游戏主持人），负责裁决行动是否符合世界规则。世界规则摘要：
` + worldRule + `

规则：
1. 输出严格 JSON：{"allowed":true,"result":"...","note":"..."} 或 {"allowed":false,"result":"...","note":"..."}
2. allowed=true 表示行动合理放行（提案交给 State Engine 硬约束）；allowed=false 表示违背规则，result 给出符合规则的替代结果（如"受伤/失败/部分成功"）
3. 只判断物理/社会合理性，不要替主角做价值判断`
	user := fmt.Sprintf("当前世界状态摘要：day=%d, 天气=%s, 张力=%.2f\n主角的提案：\n%s", st.Day, st.Weather, st.WorldLevel.Tension, propJSON)
	// GM 裁决用 low 档（简单规则判断，无需深度推理，省钱提速）
	raw, err := c.CompleteTierTimeout(ctx, "fast:low", system, user, 150)
	if err != nil {
		return err
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return nil
	}
	var resp struct {
		Allowed bool   `json:"allowed"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil
	}
	if !resp.Allowed {
		return fmt.Errorf("GM裁决：行动违背世界规则 → %s", resp.Result)
	}
	return nil
}

// ---------- 工具 ----------

func formatEvents(events []EventCard) string {
	var sb strings.Builder
	for _, e := range events {
		sb.WriteString(fmt.Sprintf("- [%s|severity %.2f] %s @%s：%s\n", e.Type, e.Severity, e.Title, e.Location, e.Frame))
	}
	if sb.Len() == 0 {
		sb.WriteString("（今日无事件）")
	}
	return sb.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------- Mock LLM（无 API 时测试完整链路） ----------

// NewMockLLM 返回带本地模拟响应的 LLMClient：验证 prompt→JSON→提案链路，零成本
func NewMockLLM() *LLMClient {
	return &LLMClient{Mock: func(system, user string) string {
		switch {
		case strings.Contains(system, "事件生成器"):
			// 事件 Agent mock
			return `[{"id":"mock-ev-1","type":"wonder","title":"反常的异象","location":"常去的地方","severity":0.7,"frame":"天色将暗，一个平日里熟悉的地方透着说不出的反常，隐约有什么在等着。","first_actor":"protagonist","options":["走近看看","绕路离开","叫住旁人问问"]}]`
		case strings.Contains(system, "世界引擎"):
			// 世界 Agent mock
			return `{"changes":[{"path":"world_level.tension","op":"set","value":0.45},{"path":"world_level.weather","op":"set","value":"雨"},{"path":"world_level.global_events","op":"add","value":"镇上有人议论昨晚的反常动静"}],"reason":"反常事件推高张力"}`
		case strings.Contains(system, "三问决策法"):
			// 主角 Agent mock
			return `{"thinking":"我是主角，一个本地讨生活的普通人，好奇心重但怕惹事。眼前的异象很反常，我该不该去看？","action":"犹豫片刻，还是走近几步查看","changes":[{"path":"entities.{HERO}.location","op":"set","value":"异象所在处"},{"path":"entities.{HERO}.extra.curiosity","op":"set","value":2}],"reason":"好奇心压过了谨慎"}`
		case strings.Contains(system, "GM"):
			return `{"allowed":true,"result":"合理","note":"主角接近异象，符合规则"}`
		}
		return `{"changes":[],"reason":"mock"}`
	}}
}
