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

// CompleteTier 按模型分层档位调用（fast/normal/premium；Mock 模式忽略档位）
func (c *LLMClient) CompleteTier(ctx context.Context, tier, system, user string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("LLMClient 未初始化")
	}
	if c.Mock != nil {
		return c.Mock(system, user), nil
	}
	if c.Cfg == nil {
		return "", fmt.Errorf("LLM API 配置为空")
	}
	// 统一走同步调用（中转站流式不稳定会挂起；同步已验证稳定）
	// 每步 LLM 调用加 150s 硬超时：中转站偶发挂起时快速失败→上层 fallback，不卡死模拟循环
	callCtx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()
	return llm.CallAPITierSync(callCtx, c.Cfg, tier, system, user)
}

// ---------- 世界 Agent（LLM）：世界推进 + 张力评估 → 状态变更提案 ----------

type worldAdvanceRequest struct {
	Day      int     `json:"day"`
	Weather  string  `json:"weather"`
	Tension  float64 `json:"tension"`
	Events   []EventCard `json:"events"`
}

// WorldAdvanceLLM 让世界 Agent 用 LLM 决定本日世界变化（天气/全局事件/张力/势力动向）
// 防遗忘三件套：精简状态（slim）+ 未回收伏笔账本 + 当前段落目标（连续性命脉，不再只靠全量硬喂）
func WorldAdvanceLLM(ctx context.Context, c *LLMClient, st *engine.WorldState, events []EventCard, rules engine.Rules, wb *worldbook.Worldbook, openForeshadows, arcPlan string) (*engine.Proposal, error) {
	ctx = llm.WithSpan(ctx, "世界推进")
	stateJSON, _ := json.MarshalIndent(map[string]any{
		"day":          st.Day,
		"weather":      st.Weather,
		"tension":      st.WorldLevel.Tension,
		"factions":     st.WorldLevel.Factions,
		"entities":     slimEntities(st.Entities), // 精简：只留 location/job/money/status/关系值，砍人设卡 extra
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

	raw, err := c.CompleteTier(ctx, "fast", system, user)
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

// TimeSkipLLM 时间过渡生成器：平淡期快进时生成"浓缩过渡段"（网文式时间跳跃）
// 时间跳跃 ≠ 无事发生：带时间标记 + 变化/积累/细节 + 伏笔持续感 + 生活切片
// 返回：过渡段文本 + 建议跳过天数（**由 LLM 按世界时间尺度自由决定**）
func TimeSkipLLM(ctx context.Context, c *LLMClient, st *engine.WorldState, heroName string, lastEvents string, openForeshadows string, wb *worldbook.Worldbook) (string, int) {
	ctx = llm.WithSpan(ctx, "时间过渡")
	fallback := fmt.Sprintf("接下来的日子，%s照常活着，但有些东西在暗处一点点变化。", heroName)
	if c == nil {
		return fallback, 30
	}
	heroJSON, _ := json.MarshalIndent(slimEntities(st.Entities), "", "  ")
	worldCtx := ""
	if wb != nil {
		worldCtx = wb.ForWorldBrief()
	}
	system := `你是世界模拟器的时间过渡写手。当剧情进入平淡期需要快进时，由你决定"跳过多久"并写一段网文式时间过渡。
输出格式（严格）：
【跳过N天】
过渡段文本
规则：
1. 【跳过N天】里的 N 由你按**这个世界的时间尺度**决定——从下方"世界背景"判断这个世界的节奏（世界观的时间流速：按天/按月/按年运转，由世界设定决定）。**不要被"天"这个单位束缚**：该跳几年就写几千天（365×年数），该跳半年就写180。跳多久的唯一标准是"这个世界的人，这段时间会怎么过"。
2. 过渡段文本 2~4 句话，必须带时间标记（"接下来几天/这一月/这三载/闭关的第五个年头"），和【跳过N天】一致。
3. **时间跳跃不是"无事发生"**：写这段时间里的变化、积累、细节、习惯、心理——投出的简历有没有回音、攒下的资源有多少、日复一日的修行/劳作有什么进展、某条线索越来越近。用生活切片/细节堆出"日子在过，事在积累，世界在变"。
4. 未回收伏笔要有"持续存在感"（那件放不下的事/没拆的信/某人的话/某个地方该再去探），为后面回收蓄力。
5. 结尾可以埋一句"不对劲"的钩子（一切如常里有一点异样），但不要展开成完整事件。
6. 禁止写"风平浪静""啥都没发生""平淡的一天"这类空话——平淡里也要有细节。
7. 只输出【跳过N天】+过渡段，不要其他任何文字。`
	user := fmt.Sprintf("世界背景（按此判断时间尺度）：\n%s\n主角：%s\n当前状态：\n%s\n最近发生的事（过渡段要衔接得上）：\n%s\n未回收伏笔（要有存在感）：\n%s\n请决定跳过多久并写过渡段。", worldCtx, heroName, heroJSON, lastEvents, openForeshadows)
	raw, err := c.CompleteTier(ctx, "fast", system, user)
	if err != nil || strings.TrimSpace(raw) == "" {
		return fallback, 30
	}
	text := strings.TrimSpace(raw)
	skipDays := 30
	// 解析【跳过N天】
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
	return text, skipDays
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
	heroJSON, _ := json.MarshalIndent(slimEntities(st.Entities), "", "  ")
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
10. 爽点不单一：四类爽点交替使用（打脸/收获/装逼/情感），连续两个段落不能给同一类爽点。` + StructureSkills()
	user := fmt.Sprintf("世界背景：\n%s\n主角：%s\n当前状态：\n%s\n当前张力：%.2f\n未回收伏笔（接住它们）：\n%s\n最近发生的事（编年史摘要）：\n%s\n请规划下一个剧情段落。", worldCtx, heroName, heroJSON, tension, openForeshadows, chronicleSummary)
	raw, err := c.CompleteTier(ctx, "fast", system, user)
	if err != nil {
		return ""
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return ""
	}
	// 校验基本字段
	var chk struct {
		ArcName string   `json:"arc_name"`
		Goal    string   `json:"goal"`
		Villain string   `json:"villain"`
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
	heroJSON, _ := json.MarshalIndent(slimEntities(st.Entities), "", "  ")
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
	raw, err := c.CompleteTier(ctx, "fast", system, user)
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
		return nil
	}
	return notes
}

// ---------- 事件 Agent（LLM）：生成遭遇框架 ----------

// EventGenLLM 让事件 Agent 生成当日 1-3 个遭遇框架（不含 NPC 具体言行，§7.5）
func EventGenLLM(ctx context.Context, c *LLMClient, st *engine.WorldState, wb *worldbook.Worldbook, openForeshadows, pendingEvents, revealedWorld, unrevealedHints, luckHint string, lastDramaDay int, arcPlan, extraCtx string) ([]EventCard, error) {
	ctx = llm.WithSpan(ctx, "事件生成")
	hero := st.Entities // 主角摘要
	heroJSON, _ := json.MarshalIndent(slimEntities(hero), "", "  ")

	worldCtx := ""
	if wb != nil {
		worldCtx = wb.ForEventAgent()
	}

	system := `你是事件生成器。为世界生成主角今天的遭遇。规则：
0. 世界背景与事件类型（严格遵守）：
` + worldCtx + `
1. 输出严格 JSON 数组，格式：
[{"id":"ev-<天>-<序号>","type":"daily|conflict|wonder|romance|opportunity|crisis|revelation|luck|disaster|quest|mystery|rival|milestone|windfall|slice","title":"...","location":"本世界地点（从世界背景里取）","severity":<0~1数值>,"frame":"遭遇场景描述（不含NPC具体言行）","first_actor":"protagonist|npc_某角色名","npcs":["某角色名"],"new_characters":[{"name":"新角色名","gender":"女","identity":"...","persona":"一句话人设","location":"出场地点","role_hint":"love_interest|important_npc|rival|npc","tier":"core|support|walkon"}],"rel_effect":"感情/关系影响说明","foreshadow":"伏笔名","resolve_foreshadow":"伏笔名","next_events":[{"title":"后续事件标题","frame":"后续事件框架"}],"options":["...","..."]}]
2. severity 0~1：日常0.1-0.3、冲突0.4-0.6、奇遇/重大/感情进展0.7-0.9（0.75以上会触发用户抉择）；**slice（生活切片）0.2-0.4**
3. frame 只写"遭遇框架"（场景/氛围/人物出现），NPC 具体说出口的话由 NPC Agent 实时生成
4. 生成 1-3 个事件，类型尽量多样；与主角当前处境相关（钱少就少消费场景）；优先选用事件类型池里的设定，避免凭空造新元素。
   **宁精勿滥**：只有今天有"值得展开的事"才生成事件——冲突、奇遇、机会、危机、感情进展、真相线索、伏笔推进、爽点、反派行动，至少占一样。如果今天确实风平浪静、没有任何戏剧价值的事，直接返回空数组 []（系统会自动快进时间，不浪费篇幅）。**严禁为了凑数生成"平淡的一天""无事发生"这类水事件。**
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
13. 事件质感铁律（网文方法论，来自真人编辑经验）：
   · 冲突要有类型：冲突四类型=目标冲突（都要一样东西）/观念冲突（认为对方错了）/资源冲突（抢资源）/情绪冲突（积怨爆发）——生成冲突事件时想清楚是哪类，冲突才真实不空洞
   · 钩子九连环：悬念/冲突/反差/危机/金手指/情感/猎奇/爽点/谜题——每天的事件里至少带一个"钩子类型"，让读者想追下去；平淡日也要埋"不对劲"的钩子
   · 伏笔四级：长线（贯穿全书，全书3~5个够）/中线（一个段落内）/短线（1~3天内回收）/隐性（细节彩蛋）——用 foreshadow 字段时标注长度（如"foreshadow":"中线·xx"），埋的时候要自然像随手一笔，别写得太刻意
   · 爽点有密度：爽点（打脸/收获/成长/危机解除）不是每章都要，但要成节奏——连憋几天的段落要安排一次"释放"，别一直压抑
15. **伏笔回收（收坑）**：下方"未回收伏笔"清单里的坑，是你埋过的——它们必须被回收，不能烂尾。若本事件**正式揭晓/结算/终结**了某个未回收伏笔（真相大白、危机解除、谜底揭开、目标达成），用 resolve_foreshadow 字段填那个伏笔名（**必须与清单里的名字完全一致**，含"中线·""短·"前缀；一个事件最多收一个；只是"提到/推进"不算回收，别填）。**收坑和埋坑同样重要：每天至少考虑一次"今天能不能收一个旧坑"**——优先回收酝酿成熟（"即将爆发"提示里的）的伏笔。
14. 限制性视角：你生成的是主角的"遭遇"，一切以主角能看到/听到/感觉到的为准——不要安排"主角不可能知道的内心戏或远景事件"作为当天遭遇的主体；主角视角外的暗流可以用 low-key 的方式埋（一句怪话/一个反常细节），但不要直接写明"XX在密谋"。
10. 世界是很大的：你现在能看到"世界深层"设定（已揭示的部分）——它真实存在并在运转，但主角可能只是偶然接触到冰山一角；不要把深层真相一次性全写出来，让世界"越走越深"。
11. 未揭示的世界深处（只有风声/线索，还没浮出水面）：
` + unrevealedHints + `
   ——它们真实存在、在暗中运转，但你只能让主角"偶然碰到线索"（一个细节、一句怪话、一件怪事），不能揭示全貌；当事件真的撞上某个线索时，那层真相才可能浮出水面（revelation 类型事件）。
12. 幸运/小概率：` + luckHint + `——若提示"今日有幸运倾向"，至少生成一个 luck 类型事件（意外的惊喜）；若无提示，也可以偶尔让平淡日子里冒出一个小概率巧合（既非刻意也非注定）。` + EventDesignSkills()

	arcBlock := ""
	if strings.TrimSpace(arcPlan) != "" {
		arcBlock = "当前剧情段落（总导演规划，今天的事件要服务于这个段落——推进 goal、呼应 villain、兑现 milestones，别跑题）：\n" + arcPlan + "\n"
	}
	if strings.TrimSpace(extraCtx) != "" {
		arcBlock += "\n" + extraCtx + "\n"
	}
	user := fmt.Sprintf("主角当前状态：\n%s\n今天的日期：第 %d 天，天气 %s，张力 %.2f\n距上一个戏剧性事件约 %d 天（若时间跨度大，说明主角经历了较长的平淡期/积累期——今天应该开启一个新阶段的事件：要么是积累后的爆发/突破，要么是伏笔到期，要么是外部势力终于行动）\n%s当前地点状态：\n%s\n未回收伏笔：%s\n遭遇链种子（来自之前事件，优先编排进来）：\n%s\n世界深层（已揭示部分，可让主角接触冰山一角）：\n%s\n世界深处（未揭示，只有线索可碰）：\n%s\n请生成今日遭遇事件。", heroJSON, st.Day, st.Weather, st.WorldLevel.Tension, st.Day-lastDramaDay, arcBlock, formatLocations(st), openForeshadows, pendingEvents, revealedWorld, unrevealedHints)

	raw, err := c.CompleteTier(ctx, "fast", system, user)
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
		return nil, fmt.Errorf("事件Agent JSON解析失败: %v", err)
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
	heroJSON, _ := json.MarshalIndent(slimEntities(map[string]engine.Entity{hero: st.Entities[hero]}), "", "  ")
	obsJSON, _ := json.MarshalIndent(obs, "", "  ")

	worldCtx := ""
	if wb != nil {
		// 主角视角：社会常识+地区常识+明面势力+角色认知（不含 L5 秘密）
		worldCtx = wb.ForProtagonist(hero, "")
	}

	system := strings.ReplaceAll(threeQuestionPrompt, "{HERO}", hero)
	system += "\n\n【你眼中的世界（普通人的认知，不要表现出你本不该知道的事）】\n" + worldCtx
	user := fmt.Sprintf("【我的状态】\n%s\n【我今天感知到的】\n%s\n【我的记忆（最近相关）】\n%s\n请按三问决策法决定今天的行动。", heroJSON, obsJSON, memories)

	raw, err := c.Complete(ctx, system, user)
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
	// GM 裁决用 fast 档（简单规则判断，fast 足够快且省，避免拖慢每次提案提交）
	raw, err := c.CompleteTier(ctx, "fast", system, user)
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