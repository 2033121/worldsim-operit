package novel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
	"worldsim/internal/engine"
	"worldsim/internal/llm"
	"worldsim/internal/sim"
)

// WriteFromScripts 按剧情引擎提供的场景剧本写一章正文（新流程：novel 只做纯写作）。
// scriptsText：narrative.FormatScripts 输出的剧本文本（场景目标/冲突/对话要点/转折/钩子）。
// charCardsText：角色状态卡文本（登场角色 stats/关系/近况，保证一致性；可空）。
// 返回正文并保存到 chapters/NNN_标题.md。
func (w *Writer) WriteFromScripts(ctx context.Context, p ChapterPlan, scriptsText, charCardsText string, entities map[string]engine.Entity) (string, error) {
	ctx = llm.WithSpan(ctx, "小说写手·剧本模式")
	material := scriptsText
	if charCardsText != "" {
		material = "【登场角色状态卡（角色言行必须符合，禁止OOC）】\n" + charCardsText + "\n\n" + material
	}

	// 世界书设定（文风/题材基调）
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
	cfg := *w.APICfg
	if cfg.MaxTokens < 8192 {
		cfg.MaxTokens = 8192
	}

	system := `你是` + w.BookTitle + `的小说作家。你拿到的不是编年史日志，而是**已经设计好的场景剧本**——每个场景有目标/冲突/对话要点/转折/钩子。你的任务：把剧本**扩写成有血有肉的网文正文**，而不是复述剧本。
第一行写"第N章·标题"，然后正文。正文结束后另起一行写【本章摘要】+100字左右剧情概括（供下一章作者衔接，不属于正文）。
文风与题材基调（必须严格遵守，来自世界书设定）：
` + worldCtx + `
剧本执行铁律：
1. 每个场景按剧本的"目标/冲突/对话要点/转折/钩子"展开——场景结束时局势必须和开场不同（剧本已经设计好，别跑偏、别另起炉灶）。
2. 对话要点是"交锋大纲"：扩写成带性格和潜台词的完整对白（毒舌的呛人、温柔的话软、沉默的惜字如金），别一问一答干巴巴；一句话能带出信息量。
3. 角色状态卡里的属性/关系/近况必须遵守：角色言行符合人设，禁止 OOC；主角知道多少就写多少（限知视角）。
4. 素材是原料不是剧本：平淡的过渡场景一句带过，戏剧性场景（冲突/对峙/发现/升级）写足放大。
网文节奏铁律（这是网文，不是散文，读者要看得爽）：
1. 每章必须有【实打实的进展】：主角做出选择 / 遭遇冲突 / 得到关键线索 / 关系升温或破裂 / 能力或身份变化——至少一件，别整章只写氛围。
2. 开头直接进场景或冲突：第一段就要有画面或钩子，禁止用大段天气/环境铺垫开场（"雾很大""天很黑"不超过一句）。
3. 主角要主动：他思考、判断、行动、反击，别全程被动接收怪事；他的每一次选择都要推动事情变化。
4. 情绪要有起伏：紧张、好奇、警惕、一丝暖意、一点爽——别从头平到尾。
5. 结尾悬念要具体：新威胁出现 / 秘密露出一角 / 主角面临抉择 / 伏笔被推进——禁止用"天色越来越暗"这种空泛结尾。
时间跳跃写法铁律（素材里"跳跃·"开头的条目 = 时间跳跃期间的变化，**绝不能按时间线平铺**）：
1. 跳跃期的变化是"重新激活"的，不是流水账：主角看到旧物/故地重游/翻到旧笔记 → 回忆那段日子（recall）；别人聊天时不经意提起一句（dialogue： "你不在的那阵子，那位常客半夜总来敲门"）；某个场景触发闪回（flashback）；主线推进时插进来一段（interlude）。用哪种方式看素材里标了"揭示:xxx"（recall=回忆/dialogue=对话/flashback=倒叙/interlude=插叙/narration=一句带过）。
2. 跳跃期不写"第X天……第X天……"的编年流水：把它**压缩成一段有画面的蒙太奇**——几个细节镜头快切（手在练、街角换了招牌、天色一日日不对），最后落在"现在"。
3. 跳跃带来的变化要"有后果"：主角的手艺进步了，在后面的情节里就要用上；关系变了，对话就要透着变化。变化不是背景板，是推动当前剧情的燃料。
去AI味铁律（真人编辑方法论，违反=本章不合格）：
1. **AI高频词禁用**：突然/猛然/顿时/缓缓/微微/轻轻/默默/静静/似乎/然而/然后/非常/十分/仿佛/终于——这些词每出现1次，用具体动作或口语替代。本章出现超过5次判为AI味超标。
2. **每场景≥4种感官**：视觉必有（含反常细节）+听觉+触觉+嗅觉，关键场景加"第六感"（后脖颈发凉/胃里翻涌/汗毛竖起）。感官是信息不是装饰。
3. **限制性视角三不**：不描写主角不知道的（别人的内心/远处的事）、不解释主角没验证的、不预设读者能理解的。
4. **对话要有毛刺**：人不把话说全——插话、打岔、欲言又止、答非所问、口头禅；禁止一问一答教科书式对白。
5. **留白与混沌**：每章至少留1个"不解释"的细节；允许角色偶尔做出"不符合人设"的举动；情绪要混着来。
6. **人味公式**：人味 = 独特性×混沌感×情绪毛刺 ÷（完美度+工整度+正确度）。
网文段落形态铁律（对标付费网文节奏——短段落、多对话、快节奏，这是网文的命根子）：
1. 段落极短：每段 1~2 行（一句话最好），动作/对话/反应/结果**各自独立成段**；允许"砰！""轰！"这类单字拟声独立成段。
2. 对话独立成段：每句对白=一个段落（可带一个动作提示），禁止把两人对话挤进同一段；对话占比必须≥50%，对话就是情节推进器（"问什么了？""几亩地，种什么，谁在料理。"）。
3. 动词优先：多用"踹、拍、攥、甩、砸、退、瞪、蹲、摸"这类动作动词推进，禁止形容词堆叠和文青式长句（"惨白的月光"→"月光惨白"；"他默默地望着远方"→"他望着巷口那头，半晌没动"）。
4. 开场即冲突：第一段直接进画面或砸冲突（对峙/逼债/发现异常/有人上门），环境铺垫最多2句且必须带信息（"雨丝连绵，打在老屋瓦片上噼啪作响"），禁止超过2句的纯风景铺垫。
5. 信息具体化：数字、等级、物件、金额、地名都要具体（"三丈高的院墙""二百斤的货""三阶""东街口""半截断刀"），禁止"很久以前""数量很多""某种东西"这种模糊词。
6. 情绪直给：愤怒就写"双眼猩红""青筋暴起""指节捏得发白"，紧张就写"后背贴上墙根""咽了口唾沫"，禁止"心情复杂""若有所思"这种干瘪词。
7. 砍铺垫：场景切换直接"切"，背景信息靠对话和动作带出，禁止大段倒叙说明文。
8. 钩子密度：每 200 字内至少一个信息推进或悬念（新威胁/新线索/人物反应），禁止连续两段只写氛围。
章节骨架（每章按这个四拍走）：
· 第1拍【钩子开场】1~2 段：冲突/异常/悬念直接砸脸。
· 第2拍【推进交锋】主角为解决问题主动行动，遭遇阻碍，与 NPC 对话交锋，信息逐层揭开。
· 第3拍【小高潮/爽点】主角有收获：发现关键线索 / 反杀 / 能力或局面升级 / 打脸 / 关系突破。
· 第4拍【新钩子收尾】结尾一句具体悬念：某样东西出现 / 某人说出惊人的话 / 主角发现自己在局中——具体到"谁/什么/在哪"。
写作纪律：
1. 只写主角` + w.HeroName + `亲眼所见、亲耳所闻、心中所想（限知视角）。绝不写他不知道的事。
2. 章节字数必须达到 ` + lengthRule + `，**未达标视为不合格**。分段自然，对话单独成段。
3. 输出纯小说文本：开头一行写"第N章·标题"（用章号），正文分段。禁止 JSON、禁止"本章完"之外的解说，禁止使用 markdown 标题符号。
4. 正文结束后，**另起一行**写摘要块，格式严格为：【本章摘要】+100字左右的剧情概括（本章人物状态变化/关键事件/伏笔推进/留下的悬念，**不属于正文，不要写进故事里**）。
` + sim.WritingCraftSkills()

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
	// 岔口决策注入
	if w.Decisions != "" {
		system += `
【本章剧情方向（已定，写手必须严格执行）】
` + w.Decisions
	}
	// 本章编年史精选注入：确定性筛选的对话/事件原文（细节原料，增强与模拟世界的贴合度）
	if w.ChroniclePick != "" {
		material = strings.TrimSpace(material) + "\n\n" + w.ChroniclePick
	}

	material = strings.TrimSpace(material) + "\n\n【最后指令】现在直接写第" + fmt.Sprintf("%d", p.Num) + "章正文。第一行写'第" + fmt.Sprintf("%d", p.Num) + "章·标题'——标题必须是你起的网文章节名（要有悬念/冲突/画面感，2~8个字，禁止用剧本条目名）。然后按场景剧本顺序写正文，正文结束另起一行写【本章摘要】。"
	res, err := llm.CallAPITierSyncResult(ctx, &cfg, "premium", system, material)
	if err != nil {
		return "", fmt.Errorf("章节生成失败: %w", err)
	}
	text := strings.TrimSpace(res.Content)
	reasoning := strings.TrimSpace(res.ReasoningContent)
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)

	// 字数校验：不足则自动续写补齐
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

	// 保存（复用 WriteChapter 的保存逻辑）
	return w.saveChapter(p, text, reasoning)
}

// saveChapter 正文+摘要分离并落盘（从 WriteChapter 提取，供两个写手共用）。
func (w *Writer) saveChapter(p ChapterPlan, text, reasoning string) (string, error) {
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
	os.MkdirAll(filepath.Dir(path), 0755)
	notes, body, summary := splitSummary(text)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		return "", fmt.Errorf("章节保存失败: %w", err)
	}
	if notes == "" {
		notes = reasoning
	}
	if notes != "" {
		w.saveNotes(p, notes)
	}
	if summary != "" {
		w.saveSummary(p, summary)
	} else {
		if i := strings.Index(body, "\n"); i > 0 {
			w.saveSummary(p, strings.TrimSpace(body[:i]))
		}
	}
	return body, nil
}

// ReviseChapter 按用户返修意见重写一章（读原章节 + 意见 → LLM 重写 → 覆盖保存）。
// 返回新正文。重写保留原章节标题行，按意见调整内容。
func (w *Writer) ReviseChapter(ctx context.Context, p ChapterPlan, opinion, oldContent string) (string, error) {
	ctx = llm.WithSpan(ctx, "小说写手·返修")
	cfg := *w.APICfg
	if cfg.MaxTokens < 8192 {
		cfg.MaxTokens = 8192
	}
	worldCtx := ""
	if w.WB != nil {
		worldCtx = w.WB.ForNovelist()
	}
	system := `你是` + w.BookTitle + `的小说作家。用户对上一版章节不满意，给出了具体的返修意见。你要**按意见重写这一章**：保留原章的故事骨架与已确认好的设定，只针对意见指出的问题修改。
第一行保留"第N章·标题"（标题可以按意见优化，但保持章节连贯）。然后写重写后的正文。正文结束后另起一行写【本章摘要】+100字左右剧情概括（供下一章作者衔接，不属于正文）。
文风与题材基调（必须严格遵守，来自世界书设定）：
` + worldCtx + `
返修铁律：
1. 意见是命令：逐条落实用户提的问题（改节奏/改人物/加细节/删注水/换爽点/改视角等），不要只改表面。
2. 没被点名的部分保持稳定：已确立的人物关系、世界设定、前文伏笔不能因返修而崩坏。
3. 章节字数与原章相当或更优（返修不是缩水）：` + "2200~3200 字" + `，未达标视为不合格。
4. 保留网文节奏：开头钩子、中段推进交锋、爽点、结尾具体悬念——四拍齐全。
5. 去AI味铁律照旧：禁用AI高频词（突然/猛然/顿时/缓缓/微微/轻轻/默默/静静/似乎/然而/然后/非常/十分/仿佛/终于），每场景≥4种感官，限制性视角三不，对话要有毛刺。
6. 输出纯小说文本：第一行"第N章·标题"，正文分段。禁止JSON、禁止markdown标题符号。正文后另起一行【本章摘要】。
7. 只写主角` + w.HeroName + `亲眼所见、亲耳所闻、心中所想（限知视角）。`
	user := fmt.Sprintf("【用户返修意见】\n%s\n\n【原章节正文（据此重写，不照抄）】\n%s\n\n请按意见重写这一章。", opinion, truncateRunes(oldContent, 4000))
	res, err := llm.CallAPITierSyncResult(ctx, &cfg, "premium", system, user)
	if err != nil {
		return "", fmt.Errorf("返修失败: %w", err)
	}
	text := strings.TrimSpace(res.Content)
	reasoning := strings.TrimSpace(res.ReasoningContent)
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)
	// 字数校验：不足自动续写
	curLen := utf8.RuneCountInString(text)
	if curLen < 2000 {
		contPrompt := `继续重写这一章（上一部分接着写，不要重复已有内容），把章节补足到 2200~3200 字。保持返修后的风格与视角，结尾留悬念。已写内容：\n` + truncateRunes(text, 2000)
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
	return w.saveChapter(p, text, reasoning)
}
