// Package novel 实现 WorldSim 的小说化联通（设计文档 §8/§9.10）：
//
//	编年史（FACT/SAID/STATE）+ 主角内心 → 小说章节 → 全书导出
//	核心原则：素材全部转化为叙事（对话化/场景化/心理化），限知视角不剧透。
package novel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"worldsim/internal/config"
	"worldsim/internal/engine"
	"worldsim/internal/llm"
	"worldsim/internal/sim"
	"worldsim/internal/worldbook"
)

// ---------- 章节计划 ----------

type ChapterPlan struct {
	Num      int    `json:"num"`
	Title    string `json:"title"`
	DayStart int    `json:"day_start"`
	DayEnd   int    `json:"day_end"`
	Days     []int  `json:"days"`
	Status   string `json:"status"` // pending | done
	Path     string `json:"path,omitempty"`
}

// Writer 小说化写手（把模拟成果写成可阅读的小说）
type Writer struct {
	APICfg     *config.APIConfig
	BookTitle  string
	BookDir    string // storys/{书名}/chapters/
	WB         *worldbook.Worldbook
	DaysPerCh  int           // 每章包含的模拟天数（默认3）
	ChapterLen string        // 章节字数档位：short(1500) | normal(2500) | long(4000)
	Material   *MaterialBank // 描写素材库（真人大神示范，825条）
	HeroName   string        // 主角名（写手必须用模拟主角名，不得自造）
	// 跨章记忆（长文一致性：每章独立请求，靠注入"前情提要+伏笔"防遗忘/防断头）
	PrevSummary string // 前面所有章节的一句话摘要（累积，最近优先）
	Foreshadows string // 未回收伏笔清单（模拟层伏笔账本，写手可推进/回收）
	Decisions   string // 本章涉及剧情岔口与已定方向（用户改选优先，否则 AI 代决；写手必须照此方向写）
}

// NewWriter 创建小说化写手
func NewWriter(apiCfg *config.APIConfig, bookTitle, bookDir string, wb *worldbook.Worldbook, heroName string) *Writer {
	os.MkdirAll(filepath.Join(bookDir, "chapters"), 0755)
	// 素材库：程序目录 material/（可放自定义素材）
	materialDir := filepath.Join(bookDir, "..", "..", "material")
	return &Writer{
		APICfg:     apiCfg,
		BookTitle:  bookTitle,
		BookDir:    bookDir,
		WB:         wb,
		DaysPerCh:  3,
		HeroName:   heroName,
		ChapterLen: "normal",
		Material:   LoadMaterialBank(materialDir),
	}
}

// SetHeroName 覆盖主角名（初始化时同步，写手必须用模拟主角名）
func (w *Writer) SetHeroName(name string) {
	if name != "" {
		w.HeroName = name
	}
}

// SetMaterialDir 覆盖素材库目录（供外部指定）
func (w *Writer) SetMaterialDir(dir string) { w.Material = LoadMaterialBank(dir) }

// ---------- 章节计划 ----------

// PlanChapters 事件驱动分章：每章至少包含 DaysPerCh 天且 ≥2 个"戏剧日"（有真实事件/对话/抉择的天）
// 平淡日（时间流逝）自动并入相邻章当背景——时间不再是分章主角，事件才是
func (w *Writer) PlanChapters(chronicle []sim.ChronicleEntry, thinkings map[int]string) []ChapterPlan {
	if len(chronicle) == 0 {
		return nil
	}
	// 收集有记录的 day，排序
	daySet := map[int]bool{}
	for _, e := range chronicle {
		daySet[e.Day] = true
	}
	for d := range thinkings {
		daySet[d] = true
	}
	var days []int
	for d := range daySet {
		days = append(days, d)
	}
	sort.Ints(days)
	if len(days) == 0 {
		return nil
	}
	// 戏剧日判定：当天有真实事件（FACT·事件源）、对话（SAID）或抉择（thinkings）
	dramaDay := func(d int) bool {
		if thinkings[d] != "" {
			return true
		}
		for _, e := range chronicle {
			if e.Day == d && (e.Kind == "SAID" || (e.Kind == "FACT" && e.Source == "事件")) {
				return true
			}
		}
		return false
	}

	var plans []ChapterPlan
	num := 1
	var chunk []int
	chunkDrama := 0
	for _, d := range days {
		chunk = append(chunk, d)
		if dramaDay(d) {
			chunkDrama++
		}
		// 成章条件：≥2 个戏剧日且覆盖 ≥DaysPerCh 天；或天数超 2×DaysPerCh 强制成章（防超长）
		if (chunkDrama >= 2 && len(chunk) >= w.DaysPerCh) || len(chunk) >= w.DaysPerCh*2 {
			title := w.pickChapterTitle(chronicle, chunk)
			plans = append(plans, ChapterPlan{
				Num: num, Title: title,
				DayStart: chunk[0], DayEnd: chunk[len(chunk)-1],
				Days: chunk, Status: "pending",
			})
			num++
			chunk = nil
			chunkDrama = 0
		}
	}
	if len(chunk) > 0 {
		title := w.pickChapterTitle(chronicle, chunk)
		plans = append(plans, ChapterPlan{
			Num: num, Title: title,
			DayStart: chunk[0], DayEnd: chunk[len(chunk)-1],
			Days: chunk, Status: "pending",
		})
	}
	return plans
}

// pickChapterTitle 从该章天数范围内的事件条目选标题
func (w *Writer) pickChapterTitle(chronicle []sim.ChronicleEntry, days []int) string {
	daySet := map[int]bool{}
	for _, d := range days {
		daySet[d] = true
	}
	// 从事件型 FACT 条目提取（Source=事件 且 content 像标题的）
	for _, e := range chronicle {
		if !daySet[e.Day] {
			continue
		}
		if e.Kind == "FACT" && e.Source == "事件" {
			t := strings.TrimSpace(e.Content)
			if t != "" && len([]rune(t)) <= 20 {
				return t
			}
		}
	}
	// 兜底：任意 FACT
	for _, e := range chronicle {
		if daySet[e.Day] && e.Kind == "FACT" {
			t := strings.TrimSpace(e.Content)
			if t != "" {
				r := []rune(t)
				if len(r) > 14 {
					return string(r[:14]) + "…"
				}
				return t
			}
		}
	}
	return fmt.Sprintf("第%d至%d天", days[0], days[len(days)-1])
}

// ---------- 叙事规划层（两段式管线第①段） ----------

// NarrativePlan 叙事规划：把编年史素材翻译成"怎么讲"的章节剧本
type NarrativePlan struct {
	OpeningHook   string `json:"opening_hook"`   // 开场钩子：用什么场景/悬念开场
	MiddleDevelop string `json:"middle_develop"` // 中段展开：哪些事件写足、哪些压缩
	Climax        string `json:"climax"`         // 本章高潮/爽点：最该写足的那个时刻
	ClosingHook   string `json:"closing_hook"`   // 收尾钩子：结尾留什么具体悬念
	POVNote       string `json:"pov_note"`       // 视角决策：主角知道什么/不知道什么
	PayoffType    string `json:"payoff_type"`    // 本章爽点类型（打脸/收获/装逼/情感/无）
	Pacing        string `json:"pacing"`         // 节奏：fast(快节奏推进) / slow(蓄力铺垫) / mixed(张弛交替)
	WordBudget    string `json:"word_budget"`    // 字数策略：哪些段写足哪些段省略
}

// planChapterNarrative 叙事规划层：拿编年史素材+世界书+前情，让 LLM 规划"这一章怎么讲"
// 这是两段式管线的第①段——编年史是"发生了什么"，叙事规划决定"怎么讲"
func (w *Writer) planChapterNarrative(ctx context.Context, p ChapterPlan, material string) string {
	ctx = llm.WithSpan(ctx, "叙事规划")
	if w.APICfg == nil {
		return ""
	}

	worldCtx := ""
	if w.WB != nil {
		worldCtx = w.WB.ForNovelist()
	}

	system := `你是网文小说的"叙事规划师"。你的任务不是写正文，而是拿到模拟世界的编年史素材后，规划"这一章怎么讲"——把日志变成剧本大纲。
输出严格 JSON，格式：
{"opening_hook":"开场用什么场景/悬念直接切入（具体到画面和动作，禁止氛围铺垫开场）","middle_develop":"中段怎么展开：哪些事件写成完整场景（写足）、哪些事件压缩成一句过渡、事件之间怎么衔接","closing_hook":"结尾留什么具体悬念（具体到谁/什么/在哪，禁止空泛的预感式结尾）","pov_note":"视角决策：主角本章知道什么/不知道什么/他以为的真相和实际的差距","payoff_type":"本章爽点类型（打脸/收获/装逼/情感/无——如果本章不该给爽点就填'无'）","pacing":"节奏（fast=快节奏冲突推进/slow=蓄力铺垫/mixed=张弛交替）","word_budget":"字数策略：哪些段写足（高潮/冲突/爽点）、哪些段省略（过渡/背景）、预估本章该长该短"}
规则：
1. 素材是"原料"，你是"厨师"——决定怎么切、怎么炒、怎么摆盘。平淡的日子不用硬写，戏剧性的场景要放大。
2. 开场必须直接进冲突/异常/悬念，禁止天气/环境开场。
3. 收尾必须留具体钩子，让读者想看下一章。
4. 视角严格限知：主角不知道的绝对不能写。
5. 爽点要按节奏来——不是每章都要给爽点，憋着的章节要标注"蓄力"，释放的章节标注"爆发"。
6. 如果素材里有打脸/收获/装逼/情感的机会，标注出来让写手写足。
7. 只输出 JSON，不要其他文字。`

	user := fmt.Sprintf("章节信息：第%d章（模拟第%d~%d天）\n\n世界设定：\n%s\n\n", p.Num, p.DayStart, p.DayEnd, worldCtx)
	if w.PrevSummary != "" {
		user += "前情提要：\n" + w.PrevSummary + "\n\n"
	}
	if w.Foreshadows != "" {
		user += "未回收伏笔：\n" + w.Foreshadows + "\n\n"
	}
	user += "本章素材（编年史剪辑）：\n" + material + "\n\n请规划这一章怎么讲。"

	raw, err := llm.CallAPITierSync(ctx, w.APICfg, "fast", system, user)
	if err != nil {
		return ""
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return ""
	}
	var plan NarrativePlan
	if err := json.Unmarshal([]byte(jsonStr), &plan); err != nil {
		return ""
	}
	// 格式化成写手可读的叙事大纲
	var sb strings.Builder
	sb.WriteString("\n【叙事规划（本章剧本大纲，写手必须照此结构写）】\n")
	sb.WriteString("开场钩子：" + plan.OpeningHook + "\n")
	sb.WriteString("中段展开：" + plan.MiddleDevelop + "\n")
	sb.WriteString("本章高潮：" + plan.Climax + "\n")
	sb.WriteString("收尾钩子：" + plan.ClosingHook + "\n")
	sb.WriteString("视角注意：" + plan.POVNote + "\n")
	sb.WriteString("爽点类型：" + plan.PayoffType + "\n")
	sb.WriteString("节奏定位：" + plan.Pacing + "\n")
	sb.WriteString("字数策略：" + plan.WordBudget + "\n")
	return sb.String()
}

// ---------- 章节写作 ----------

// WriteChapter 生成一章小说正文（LLM），保存到 chapters/NNN_标题.md，返回正文
func (w *Writer) WriteChapter(ctx context.Context, p ChapterPlan, chronicle []sim.ChronicleEntry, thinkings map[int]string, entities map[string]engine.Entity) (string, error) {
	ctx = llm.WithSpan(ctx, "小说写手")
	material := w.buildChapterMaterial(p, chronicle, thinkings)
	// 两段式管线第①段：叙事规划层——先规划"这一章怎么讲"，再让写手按大纲写
	narrativePlan := w.planChapterNarrative(ctx, p, material)
	// 小说化用设定：A1世界观 + B4伏笔清单 + C文风（世界书驱动，文风随题材走）
	worldCtx := ""
	if w.WB != nil {
		worldCtx = w.WB.ForNovelist()
		if w.HeroName != "" {
			worldCtx += "\n主角：" + w.HeroName + "（" + w.characterIntro(entities) + "）"
		}
	}

	lengthRule := "2200~3200 字"
	minLen := 2000
	switch w.ChapterLen {
	case "short":
		lengthRule = "1400~1800 字"
		minLen = 1200
	case "long":
		lengthRule = "3500~4500 字"
		minLen = 3200
	}

	// 小说写手用更大的 token 预算（长文），clone 配置避免影响其他调用
	cfg := *w.APICfg
	if cfg.MaxTokens < 8192 {
		cfg.MaxTokens = 8192
	}

	system := `你是` + w.BookTitle + `的小说作家。
直接写小说正文，第一行是"第N章·标题"，然后正文。不要输出任何分析、复述、解释，正文必须是纯小说。正文结束后另起一行写【本章摘要】+100字左右剧情概括（供下一章作者衔接，不属于正文）。

文风与题材基调（必须严格遵守，来自世界书设定）：
` + worldCtx + `

网文节奏铁律（这是网文，不是散文，读者要看得爽）：
1. 每章必须有【实打实的进展】：主角做出选择 / 遭遇冲突 / 得到关键线索 / 关系升温或破裂 / 能力或身份变化——至少一件，别整章只写氛围。
2. 开头直接进场景或冲突：第一段就要有画面或钩子，禁止用大段天气/环境铺垫开场（"雾很大""天很黑"不超过一句）。
3. 对话要有交锋和个性：角色说话带性格（毒舌的呛人、温柔的话软、沉默的惜字如金），别一问一答干巴巴；一句话能带出信息量。
4. 主角要主动：他思考、判断、行动、反击，别全程被动接收怪事；他的每一次选择都要推动事情变化。
5. 情绪要有起伏：紧张、好奇、警惕、一丝暖意、一点爽——别从头平到尾。
6. 结尾悬念要具体：新威胁出现 / 秘密露出一角 / 主角面临抉择 / 伏笔被推进——禁止用"雾还在涨"这种空泛结尾。

去AI味铁律（真人编辑方法论，违反=本章不合格）：
1. **AI高频词禁用**：突然/猛然/顿时/缓缓/微微/轻轻/默默/静静/似乎/然而/然后/非常/十分/仿佛/终于——这些词每出现1次，用具体动作或口语替代（用动作细节、具体感官、更准的词代替抽象修饰）。本章出现超过5次判为AI味超标。
2. **每场景≥4种感官**：视觉必有（含反常细节）+听觉+触觉+嗅觉，关键场景加"第六感"（后脖颈发凉/胃里翻涌/汗毛竖起）。感官是信息不是装饰。
3. **限制性视角三不**：不描写主角不知道的（别人的内心/远处的事）、不解释主角没验证的、不预设读者能理解的。视角一乱，代入感崩塌。
4. **对话要有毛刺**：人不把话说全——插话、打岔、欲言又止、答非所问、口头禅；禁止一问一答教科书式对白；对话要带性格和潜台词。
5. **留白与混沌**：每章至少留1个"不解释"的细节（真实生活里很多事就是没道理）；允许角色偶尔做出"不符合人设"的举动（人是复杂的）；情绪要混着来（开心时一丝怅然，愤怒时一丝无力），别单线平铺。
6. **人味公式**：人味 = 独特性×混沌感×情绪毛刺 ÷（完美度+工整度+正确度）。写得太工整太"正确"的地方，主动制造一点偏差。

网文段落形态铁律（网文 = 短段落 + 动词推进 + 高信息密度）：
1. 段落要短：每段最多 2~3 行，动作、对话、反应**各自独立成段**；一句话能说完的绝不用两句。
2. 动词优先：多用"推门、攥紧、蹲下、抬头、转身"这类动作动词推进画面，少用形容词堆叠（抽象修饰每章限用）。
3. 环境信息化：环境描写不超过全章 1/10，且必须带信息或异常感（"灯管闪了两下"→ 暗示异常），禁止纯风景抒情段落。
4. 砍掉 80% 铺垫：场景切换直接"切"，不用过渡句铺垫；背景信息在对话和动作里带出来，禁止大段背景介绍。
5. 对话占比要高：每章对话至少占 40% 篇幅，对话就是情节（带出信息、推进冲突、暴露性格）。

章节骨架（每章按这个四拍走）：
· 第1拍【钩子开场】1~2 段：冲突/异常/悬念直接砸脸，主角立刻处于"有事发生"的状态。
· 第2拍【推进交锋】主角为解决问题主动行动，遭遇阻碍，与 NPC 对话交锋，信息逐层揭开。
· 第3拍【小高潮/爽点】主角有收获：发现关键线索 / 反杀 / 能力或局面升级 / 打脸 / 关系突破——让读者爽一下。
· 第4拍【新钩子收尾】结尾一句具体悬念：某样东西出现 / 某人说出惊人的话 / 主角发现自己在局中——具体到"谁/什么/在哪"，禁止"他感觉有大事要发生"。

叙事方法铁律（这是网文，不是散文——用动词推进，别用形容词堆砌）：
· 散文腔（❌ 禁止）：大段心理铺陈、命运感慨、环境抒情开场、绕圈子的铺垫——删掉，直接写动作/对话/画面。
· 网文腔（✅ 必须）：动作直接、对话带刺、段落短、信息密度高——一句话能说完的绝不用两句。
· 环境信息当钩子用：写到环境时，让它承载异常/信息（灯管闪了两下→暗示异常），禁止纯风景抒情。
7. 爽点意识（世界书题材决定爽点形态）：底层翻身、能力觉醒的爽、打脸反转、危机解除——有合适机会就给读者一个爽点。

 写作纪律：
1. 只写主角` + w.HeroName + `亲眼所见、亲耳所闻、心中所想（限知视角）。绝不写他不知道的事——别人的过去、世界的秘密、任何"里层真相"，主角不知道就不许写。
2. 素材必须全部转化为叙事，禁止照搬条目：
·【经历】里的"事件"→ 场景描写（五感、环境、氛围）
·【对话】→ 引号对话，带说话人的动作和神态
·【内心】→ 心理活动（"他想着……"或直接内心独白）
3. 章节字数必须达到 ` + lengthRule + `，**未达标视为不合格**。分段自然，对话单独成段。用足素材，把关键素材写成完整场景，场景与场景之间用时间流转衔接。

戏剧化改编权（你是导演，不是记录员）：
· 素材是"原料"，不是"剧本"——你有权压缩、合并、强化、改编：平淡的日子一句带过或直接跳过，戏剧性场景（冲突/对峙/发现/升级）写足放大。
· 主角主动性：素材里主角如果只是"被怪事找上门"，你要主动给他安排行动——他去查、他去问、他做选择，让读者看到他在推动故事。
· 能力成长：按 A7 能力体系给本章的能力状态定位，素材里能力相关的现象要写成能力在运作（感知到什么、有什么用、代价是什么），该升级的章节写出升级时刻。
· 反派存在感：按 A8 反派行动线，每 2~3 章安排反派动一次，主角要有应对，压迫→对抗→打脸要有完整的爽点闭环。
· 强化爽点：发现关键线索、反杀、打脸、能力升级、关系突破——这些时刻宁可写足不要一笔带过（读者等的就是这些）。
4. 输出纯小说文本：开头一行写"第N章·标题"（用章号），正文分段。禁止 JSON、禁止"本章完"之外的解说，禁止使用 markdown 标题符号。
5. 正文结束后，**另起一行**写摘要块，格式严格为：【本章摘要】+100字左右的剧情概括（本章人物状态变化/关键事件/伏笔推进/留下的悬念，供下一章作者衔接，**不属于正文，不要写进故事里**）。
6. 详略铁律（§9.9 叙事时间控制）：
   · 【场景素材】→ 完整场景，写足戏剧张力
   · 【背景素材】→ 章首一句过渡带过（用一句概括时间流逝的话），**严禁展开成完整场景**
   · 没有任何事发生的时段 → **直接跳过**，不要为凑字数写流水账
   · 宁可章节稍短，也不要注水；平淡章写短，高潮章写足` + sim.WritingCraftSkills()

	// 两段式管线：注入叙事规划层产出的大纲（写手按此结构写，不再"翻译"编年史）
	if narrativePlan != "" {
		system += narrativePlan
	}

	// 跨章记忆注入：前情提要（防遗忘）+ 未回收伏笔（防断头）
	if w.PrevSummary != "" {
		system += `

【前情提要（前面章节已发生的事，保持连贯：别重复写、别写漏、人物关系与状态沿用）】
` + w.PrevSummary
	}
	if w.Foreshadows != "" {
		system += `

【未回收伏笔（前文埋下的钩子，本章自然推进或回收，别忘掉）】
` + w.Foreshadows
	}
	// 岔口决策注入：本章已定方向（用户改选优先，否则 AI 代决）——写手必须照此方向推进
	if w.Decisions != "" {
		system += `

【本章剧情方向（已定，写手必须严格执行：主角按"采用方向"行动，不得另起炉灶或跳过）】
` + w.Decisions
	}

	// 素材库注入：抽真实网文段落当"形态示范"（学段落长短/对话节奏/动作推进，禁止抄袭）
	if w.Material != nil {
		if ref := w.Material.PickFor(material, 3, 8); ref != "" {
			system += "\n\n" + ref
		}
	}

	// user 末尾强制指令：直接写正文（思考走模型自带 reasoning 通道，自动另存）
	material = strings.TrimSpace(material) + "\n\n【最后指令】现在直接写第" + fmt.Sprintf("%d", p.Num) + "章正文。第一行写'第" + fmt.Sprintf("%d", p.Num) + "章·标题'——标题必须是你起的网文章节名（要有悬念/冲突/画面感，2~8个字，禁止用素材条目名）。然后写正文，正文结束另起一行写【本章摘要】。"

	res, err := llm.CallAPITierSyncResult(ctx, &cfg, "premium", system, material)
	if err != nil {
		return "", fmt.Errorf("章节生成失败: %w", err)
	}
	text := strings.TrimSpace(res.Content)
	reasoning := strings.TrimSpace(res.ReasoningContent) // 模型自带思考通道（若有），正文之外另存
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)

	// 字数校验：不足则自动续写补齐（用当前正文继续写，直到达标）
	curLen := utf8.RuneCountInString(text)
	if curLen < minLen {
		contPrompt := `继续写这个章节（这是上一部分，接着写下去，不要重复已有内容）。
要求：接着写后续场景（可以是第二天/新的遭遇/更多对话与内心），把章节补足到总字数 ` + lengthRule + `。保持同样的文风和视角，结尾留悬念。
已写内容（前面部分，不要重复）：
` + truncateRunes(text, 2000)
		cont, err2 := llm.CallAPITierSync(ctx, &cfg, "premium", system, contPrompt)
		if err2 == nil {
			cont = strings.TrimSpace(cont)
			cont = strings.TrimPrefix(cont, "```")
			cont = strings.TrimSuffix(cont, "```")
			if utf8.RuneCountInString(cont) > 100 {
				text = text + "\n\n" + cont
			}
		}
	}

	// 从正文提取标题（第一行"第N章·XXX"），用于文件名
	fileTitle := p.Title
	if idx := strings.Index(text, "\n"); idx > 0 {
		first := strings.TrimSpace(text[:idx])
		first = strings.TrimPrefix(first, "#")
		first = strings.TrimSpace(first)
		if dot := strings.Index(first, "·"); dot >= 0 && dot+1 < len(first) {
			fileTitle = strings.TrimSpace(first[dot+1:])
		}
	}

	fname := fmt.Sprintf("%03d_%s.md", p.Num, sanitize(fileTitle))
	path := filepath.Join(w.BookDir, "chapters", fname)
	os.MkdirAll(filepath.Dir(path), 0755) // 兜底：目录被删时重建
	// 正文与摘要分离（AI 返回：正文 + 【本章摘要】；思考走 reasoning_content 通道自动另存）
	notes, body, summary := splitSummary(text)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		return "", fmt.Errorf("章节保存失败: %w", err)
	}
	// 创作思路：优先用模型自带思考通道（reasoning_content），兜底用 content 里【创作思路】块
	if notes == "" {
		notes = reasoning
	}
	if notes != "" {
		w.saveNotes(p, notes)
	}
	// 摘要持久化（summary.jsonl）+ 累积前情提要（下一章注入）
	if summary != "" {
		w.saveSummary(p, summary)
	} else {
		// 兜底：AI 没写摘要块时，取正文首句（尽力而为）
		if i := strings.Index(body, "\n"); i > 0 {
			w.saveSummary(p, strings.TrimSpace(body[:i]))
		}
	}
	return body, nil
}

// splitSummary 把 AI 输出拆成（创作思路, 正文, 摘要）
// 输出格式：可选【创作思路】+【正文】+【本章摘要】；正文必须在【正文】之后
func splitSummary(text string) (notes, body, summary string) {
	bodyMark := "【正文】"
	sumMark := "【本章摘要】"
	sumIdx := strings.LastIndex(text, sumMark)
	if sumIdx > 0 {
		summary = strings.TrimSpace(text[sumIdx+len(sumMark):])
		summary = strings.TrimPrefix(summary, "本章完")
		summary = strings.TrimSpace(summary)
		text = text[:sumIdx]
	}
	bodyIdx := strings.Index(text, bodyMark)
	if bodyIdx >= 0 {
		notes = strings.TrimSpace(text[:bodyIdx])
		body = strings.TrimSpace(text[bodyIdx+len(bodyMark):])
		if body == "" {
			body = strings.TrimSpace(text) // 兜底
		}
		return notes, body, summary
	}
	// 没有【正文】标记：整段视为正文（去掉可能的思路开头）
	body = strings.TrimSpace(text)
	return "", body, summary
}

// saveNotes 创作思路存盘（notes.jsonl，AI构思过程，不属于正文）
func (w *Writer) saveNotes(p ChapterPlan, notes string) {
	notesPath := filepath.Join(w.BookDir, "notes.jsonl")
	os.MkdirAll(w.BookDir, 0755)
	line := fmt.Sprintf(`{"num":%d,"title":%s,"notes":%s}`, p.Num, jsonQuote(p.Title), jsonQuote(notes))
	f, err := os.OpenFile(notesPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		f.WriteString(line + "\n")
		f.Close()
	}
}

// saveSummary 摘要持久化到 summary.jsonl + 更新前情提要（保留最近8章）
func (w *Writer) saveSummary(p ChapterPlan, summary string) {
	sumPath := filepath.Join(w.BookDir, "summary.jsonl")
	os.MkdirAll(w.BookDir, 0755)
	line := fmt.Sprintf(`{"num":%d,"title":%s,"summary":%s}`, p.Num, jsonQuote(p.Title), jsonQuote(summary))
	f, err := os.OpenFile(sumPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		f.WriteString(line + "\n")
		f.Close()
	}
	// 更新内存前情提要
	w.updatePrevSummary(p.Num, p.Title, summary)
}

// updatePrevSummary 累积前情提要（每章一行，保留最近8章）
func (w *Writer) updatePrevSummary(num int, title, summary string) {
	sum := fmt.Sprintf("第%d章《%s》：%s", num, title, truncateRunes(summary, 120))
	if w.PrevSummary == "" {
		w.PrevSummary = sum
		return
	}
	w.PrevSummary = w.PrevSummary + "\n" + sum
	lines := strings.Split(w.PrevSummary, "\n")
	if len(lines) > 8 {
		w.PrevSummary = strings.Join(lines[len(lines)-8:], "\n")
	}
}

// LoadSummaries 从 summary.jsonl 恢复前情提要（重启/重新生成小说时用，实现跨章记忆断点）
func (w *Writer) LoadSummaries() {
	sumPath := filepath.Join(w.BookDir, "summary.jsonl")
	data, err := os.ReadFile(sumPath)
	if err != nil {
		return
	}
	w.PrevSummary = ""
	for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var s struct {
			Num     int    `json:"num"`
			Title   string `json:"title"`
			Summary string `json:"summary"`
		}
		if json.Unmarshal([]byte(ln), &s) == nil && s.Summary != "" {
			w.updatePrevSummary(s.Num, s.Title, s.Summary)
		}
	}
}

// jsonQuote 简单 JSON 字符串转义（标题/摘要含引号时）
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "……"
}

// buildChapterMaterial 剪辑层：按戏剧权重把本章编年史剪成"剧本包"（导演剪辑，不是素材堆）
//
//	【本章焦点】tags 统计主线（谁在追什么/什么伏笔该收）——写手先知道本章任务
//	【场景素材】weight≥0.55 或对话/主角内心：按天原样给（本章血肉，完整场景）
//	【背景素材】0.3≤weight<0.55（时间过渡/状态变化/涟漪）：压缩成"这段时间的变化"（章首时光流转）
//	【时间流逝】<0.3 或空天：一行带过（不喂进 prompt）
//
// token 控制：只喂高权重场景 + 背景压缩，砍掉 79% 的 STATE 水条目
func (w *Writer) buildChapterMaterial(p ChapterPlan, chronicle []sim.ChronicleEntry, thinkings map[int]string) string {
	daySet := map[int]bool{}
	for _, d := range p.Days {
		daySet[d] = true
	}
	dayEntries := map[int][]sim.ChronicleEntry{}
	tagCount := map[string]int{}
	for _, e := range chronicle {
		if daySet[e.Day] {
			dayEntries[e.Day] = append(dayEntries[e.Day], e)
			for _, t := range e.Tags {
				tagCount[t]++
			}
		}
	}
	var days []int
	for d := range dayEntries {
		days = append(days, d)
	}
	sort.Ints(days)

	// ① 本章焦点：tags 统计主线（过滤流程标签，留角色/地点/伏笔）
	var keySb, bgSb strings.Builder
	keySb.WriteString("【本章焦点】\n")
	focus := topTags(tagCount, 3)
	if len(focus) > 0 {
		keySb.WriteString("主线：" + strings.Join(focus, "、") + "（本章围绕它们展开，别写散）\n")
	} else {
		keySb.WriteString("主线：本章以主角行动与事件推进为主（无突出标签，写实推进即可）\n")
	}
	keySb.WriteString("\n【场景素材】（必须写成完整场景：环境/对话/心理/动作）\n")
	bgSb.WriteString("【背景素材】（章首一两句交代这段时间的变化，不要展开成场景）\n")

	keyCount, bgCount := 0, 0
	for _, d := range days {
		entries := dayEntries[d]
		var dayKey, dayBg []string
		for _, e := range entries {
			line := "· " + e.Content
			// 场景：SAID 对话 / 高权重（事件/铺垫/伏笔/关系/揭示/里程碑）
			if e.Kind == "SAID" || e.Weight >= 0.55 {
				dayKey = append(dayKey, line)
			} else if e.Weight >= 0.3 {
				// 背景：时间过渡/状态变化/涟漪等，压缩
				dayBg = append(dayBg, line)
			}
			// weight<0.3：天气/张力微调等水条目，直接丢弃（不进 prompt，省 token）
		}
		// 主角内心（重点）
		if th, ok := thinkings[d]; ok && th != "" {
			dayKey = append(dayKey, "· "+w.HeroName+"内心："+th)
		}
		if len(dayKey) > 0 {
			keySb.WriteString(fmt.Sprintf("\nDay%d：\n%s\n", d, strings.Join(dayKey, "\n")))
			keyCount++
		}
		if len(dayBg) > 0 {
			bgSb.WriteString(fmt.Sprintf("Day%d：%s\n", d, strings.Join(dayBg, "；")))
			bgCount++
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("请写第 %d 章（对应模拟第 %d ~ %d 天）：\n\n", p.Num, p.DayStart, p.DayEnd))
	sb.WriteString("详略规则：\n")
	sb.WriteString("· 【场景素材】必须写成完整场景（这是本章血肉）\n")
	sb.WriteString("· 【背景素材】用一句过渡带过（如\"这段时间，日子照旧，但有些东西在变\"），绝不展开\n\n")
	if keyCount > 0 {
		sb.WriteString(keySb.String())
		sb.WriteString("\n")
	}
	if bgCount > 0 {
		sb.WriteString(bgSb.String())
		sb.WriteString("\n")
	}
	// 完全没场景？提示过渡章
	if keyCount == 0 {
		sb.WriteString("\n（本章无重大事件，请写成过渡章：篇幅可缩短，重点写时间流逝与主角状态变化）\n")
	}
	return sb.String()
}

// topTags 取出现次数最多的 N 个"主线标签"（过滤流程性标签，留角色/地点/伏笔/冲突）
func topTags(tagCount map[string]int, n int) []string {
	ban := map[string]bool{"系统": true, "导演": true, "段落": true, "事件": true, "对话": true, "状态": true, "观察": true, "反思": true, "铺垫": true, "时间过渡": true, "快进": true, "降级": true, "受阻": true, "涟漪": true, "世界揭示": true, "里程碑": true, "张力": true}
	type kv struct {
		k string
		v int
	}
	var arr []kv
	for k, v := range tagCount {
		if ban[k] {
			continue
		}
		arr = append(arr, kv{k, v})
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].v > arr[j].v })
	var out []string
	for i := 0; i < len(arr) && i < n; i++ {
		out = append(out, arr[i].k)
	}
	return out
}

// characterIntro 生成角色简介（供写手参考）
func (w *Writer) characterIntro(entities map[string]engine.Entity) string {
	var sb strings.Builder
	sb.WriteString(w.HeroName + "：这个世界里的一名普通人，有自己的日子要过。")
	if ent, ok := entities[w.HeroName]; ok {
		if j, ok := ent.Extra["profile"].(string); ok && j != "" {
			sb.Reset()
			sb.WriteString(w.HeroName + "：" + j + "。")
		}
	}
	// 其他角色：从实体取 job/profile（任何世界通用，不写死角色名）
	for name, ent := range entities {
		if name == w.HeroName {
			continue
		}
		j, _ := ent.Extra["job"].(string)
		if j == "" {
			j = ent.Job
		}
		if p, ok := ent.Extra["profile"].(string); ok && p != "" {
			sb.WriteString(fmt.Sprintf("%s：%s（%s）。", name, j, p))
		} else if j != "" {
			sb.WriteString(fmt.Sprintf("%s：%s。", name, j))
		}
	}
	return sb.String()
}

// ---------- 全书导出 ----------
// ExportBook 把已生成章节拼成全书（txt + md 双格式），返回文件路径列表
func (w *Writer) ExportBook() ([]string, error) {
	chaptersDir := filepath.Join(w.BookDir, "chapters")
	files, err := os.ReadDir(chaptersDir)
	if err != nil {
		return nil, err
	}
	var chFiles []string
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".md") && !f.IsDir() {
			chFiles = append(chFiles, f.Name())
		}
	}
	sort.Strings(chFiles)
	if len(chFiles) == 0 {
		return nil, fmt.Errorf("还没有已生成的章节")
	}

	var body strings.Builder
	body.WriteString(w.BookTitle + "\n")
	body.WriteString(strings.Repeat("=", len([]rune(w.BookTitle))) + "\n\n")
	for _, f := range chFiles {
		data, err := os.ReadFile(filepath.Join(chaptersDir, f))
		if err != nil {
			continue
		}
		body.WriteString(strings.TrimSpace(string(data)))
		body.WriteString("\n\n---\n\n")
	}

	txtPath := filepath.Join(w.BookDir, w.BookTitle+".txt")
	mdPath := filepath.Join(w.BookDir, w.BookTitle+".md")
	if err := os.WriteFile(txtPath, []byte(body.String()), 0644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(mdPath, []byte(body.String()), 0644); err != nil {
		return nil, err
	}
	return []string{txtPath, mdPath}, nil
}

// ExportWebNovel 网文平台格式导出（起点/番茄可直接粘贴）：简介 + 分卷 + 章节 + 字数统计
func (w *Writer) ExportWebNovel() (string, error) {
	chaptersDir := filepath.Join(w.BookDir, "chapters")
	files, err := os.ReadDir(chaptersDir)
	if err != nil {
		return "", err
	}
	var chFiles []string
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".md") && !f.IsDir() {
			chFiles = append(chFiles, f.Name())
		}
	}
	sort.Strings(chFiles)
	if len(chFiles) == 0 {
		return "", fmt.Errorf("还没有已生成的章节")
	}

	var sb strings.Builder
	sb.WriteString("《" + w.BookTitle + "》\n\n")
	sb.WriteString("【简介】\n" + w.BookTitle + "：一个由世界模拟器自动生成的多Agent世界，主角在其中生活、遭遇、抉择，命运因每一次选择而改变。\n\n")
	sb.WriteString("【字数统计】\n")
	totalChars := 0
	var vols []string
	volIdx := 1
	for i, f := range chFiles {
		data, _ := os.ReadFile(filepath.Join(chaptersDir, f))
		chars := len([]rune(string(data)))
		totalChars += chars
		title := strings.TrimSuffix(f[4:], ".md")
		// 每5章一卷
		if i%5 == 0 {
			if i > 0 {
				vols = append(vols, fmt.Sprintf("第%d卷（共%d章）：第%d至%d章", volIdx, min(5, len(chFiles)-i+1), i, i+4))
			}
			volIdx++
		}
		_ = title
	}
	sb.WriteString(fmt.Sprintf("全书共 %d 章，约 %d 字。\n", len(chFiles), totalChars))
	sb.WriteString("分卷：\n" + strings.Join(vols, "\n") + "\n\n")
	sb.WriteString("========================================\n\n")

	// 正文（分卷排版）
	volNum := 1
	for i, f := range chFiles {
		if i%5 == 0 {
			sb.WriteString(fmt.Sprintf("—— 第%d卷 ——\n\n", volNum))
			volNum++
		}
		data, _ := os.ReadFile(filepath.Join(chaptersDir, f))
		sb.WriteString(strings.TrimSpace(string(data)))
		sb.WriteString("\n\n")
	}

	path := filepath.Join(w.BookDir, w.BookTitle+"_网文版.txt")
	if err := os.WriteFile(path, []byte(sb.String()), 0644); err != nil {
		return "", err
	}
	return path, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// sanitize 文件名安全化
func sanitize(s string) string {
	s = strings.TrimSpace(s)
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\n', '\r', '\t':
			sb.WriteRune('_')
		default:
			sb.WriteRune(r)
		}
	}
	out := sb.String()
	if out == "" {
		out = "untitled"
	}
	if len([]rune(out)) > 24 {
		r := []rune(out)
		out = string(r[:24])
	}
	return out
}
