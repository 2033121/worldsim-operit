package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"worldsim/internal/engine"
	"worldsim/internal/llm"
	"worldsim/internal/logx"
	"worldsim/internal/worldbook"
)

// Simulator 是模拟调度器：驱动宏观状态机（§5.1）
//
//	IDLE → 世界推进 → 事件生成 → 感知分发 → 主角决策 → GM裁决 → 记录 → 下一单元
//
// MVP 阶段：agents 用 dry-run 模板（确定性、零 LLM），验证数据流与状态机；
// 后续把 Agent 换成 LLMAgent（包装 internal/llm）即可接入真实决策。
type Simulator struct {
	engine    *engine.StateEngine
	worldDir  string
	day       int
	mode      string // scene | summary | skip
	chronicle []ChronicleEntry
	events    []EventCard
	plan      *ProtagonistPlan
	seq       int // command_id 序号
	heroName  string // 主角实体名（state.Entities 中第一个）
	llm       *LLMClient // 非 nil 时走 LLM Agent；nil 走 dry-run 模板
	lastThinking string  // 主角最近一次三问推理（可展示）
	thinkings map[int]string // 每日主角三问推理（小说化素材）
	mem       *MemoryStore // 角色记忆库（§4.6：独立记忆流）
	modeAuto  bool         // 张力引擎：auto 模式（Scene/Summary/Skip 自适应）
	lowTensionDays int     // 张力 <0.3 连续天数（Skip 判定）
	quietDays int          // 连续平淡天数（事件驱动：平淡期快进，时间跨度由剧情定）
	lastDramaDay int       // 上次戏剧性事件发生的 day（用于告诉事件Agent"距上次事件多久"）
	currentArc  string     // 总导演规划的当前剧情段落（JSON，注入事件Agent约束方向）
	arcDone     int        // 当前段落已完成的关键节点数（里程碑进度）
	dynamicAgents []DynamicAgent // 动态创建的"负责人 Agent"（公司新岗位，随剧情招聘）
	arcBook     []ArcEntry // 段落大纲账本（导演 outline，落盘防遗忘/防情节丢失）
	currentArcNum int      // 当前 open 段落编号
	lastConsolidateDay int  // 上次记忆睡眠巩固日（事件驱动下大步长跳跃会跳过"day%30"触发点，改用间隔触发）
	pending   map[int][]EventCard // 遭遇链种子（未来事件）
	foreshadows map[string]Foreshadow // 伏笔账本
	wb        *worldbook.Worldbook // 世界书（分层注入用）
	decisions *DecisionStore      // 岔口决策队列（AI 代决 + 用户可干预，零阻塞）
}

// EnableLLM 启用 LLM Agent（配置后主角/世界/事件用真实决策）
func (s *Simulator) EnableLLM(c *LLMClient) {
	s.llm = c
	if s.mem != nil {
		s.mem.SetCompressor(c) // 记忆压缩用 LLM（月度摘要）
	}
}

// SetWorldbook 设置世界书（§3.3/§4.5：各 Agent 按分层注入）
func (s *Simulator) SetWorldbook(wb *worldbook.Worldbook) { s.wb = wb }

func (s *Simulator) LastThinking() string { return s.lastThinking }

// Thinkings 返回每日主角三问推理（小说化素材，day→thinking）
func (s *Simulator) Thinkings() map[int]string { return s.thinkings }

func NewSimulator(se *engine.StateEngine, worldDir string) *Simulator {
	hero := ""
	// 主角识别：优先找 extra.role=="protagonist" 的实体（初始化时打了标记）
	for name, e := range se.State().Entities {
		if role, ok := e.Extra["role"].(string); ok && role == "protagonist" {
			hero = name
			break
		}
	}
	if hero == "" { // fallback：第一个实体（不应发生，防御）
		for name := range se.State().Entities {
			hero = name
			break
		}
	}
	s := &Simulator{
		engine:   se,
		worldDir: worldDir,
		day:      se.State().Day,
		mode:     "scene",
		heroName: hero,
		mem:      NewMemoryStore(filepath.Join(worldDir, "agents_memory.json")),
		decisions: NewDecisionStore(worldDir),
	}
	// 断点续传（§14.1）：从磁盘恢复编年史与每日思考（小说化素材）
	s.loadChronicle()
	// 导演层恢复：段落大纲/部门负责人/伏笔账本/时间锚点（防重启牛头不对马嘴）
	s.loadPlans()
	return s
}

// HeroName 返回主角名
func (s *Simulator) HeroName() string { return s.heroName }

// SetHeroName 设置主角名（初始化/恢复时同步，防止视角漂移）
func (s *Simulator) SetHeroName(name string) {
	if name != "" {
		s.heroName = name
	}
}

// ---------- 编年史持久化（§14.1：断点续传） ----------

func (s *Simulator) chroniclePath() string { return filepath.Join(s.worldDir, "chronicle.jsonl") }
func (s *Simulator) thinkingsPath() string { return filepath.Join(s.worldDir, "thinkings.json") }
func (s *Simulator) plansPath() string     { return filepath.Join(s.worldDir, "plans.json") }

// ArcEntry 段落大纲账本（导演的 outline：像写小说时的大纲，落盘防遗忘/防牛头不对马嘴）
type ArcEntry struct {
	Num             int      `json:"num"`
	Day             int      `json:"day"` // 开段日
	ArcName         string   `json:"arc_name"`
	Goal            string   `json:"goal"`
	Villain         string   `json:"villain"`
	ForeshadowFocus string   `json:"foreshadow_focus,omitempty"`
	Milestones      []string `json:"milestones"`
	Payoff          string   `json:"payoff,omitempty"`
	TimeHint        string   `json:"time_hint,omitempty"`
	Status          string   `json:"status"` // open | done
	DoneDay         int      `json:"done_day,omitempty"`
}

// PlansFile 导演层持久化：段落大纲 + 部门负责人 + 伏笔账本 + 时间锚点
type PlansFile struct {
	LastDramaDay  int                    `json:"last_drama_day"`
	ArcBook       []ArcEntry             `json:"arc_book"`
	DynamicAgents []DynamicAgent         `json:"dynamic_agents"`
	Foreshadows   map[string]Foreshadow  `json:"foreshadows"`
	CurrentArcNum int                    `json:"current_arc_num"` // 当前 open 段落编号（0=无）
	ArcDone       int                    `json:"arc_done"`        // 当前段落已完成里程碑数
}

// rawJSON 把段落还原成 GM 原始 JSON 格式（供 currentArc 注入事件 Agent）
func (e ArcEntry) rawJSON() string {
	raw, _ := json.Marshal(map[string]any{
		"arc_name": e.ArcName, "goal": e.Goal, "villain": e.Villain,
		"foreshadow_focus": e.ForeshadowFocus, "milestones": e.Milestones,
		"payoff": e.Payoff, "time_hint": e.TimeHint,
	})
	return string(raw)
}

// savePlans 导演层全部状态落盘（RunDay 每次模拟后调用，防重启丢失情节/大纲/伏笔）
func (s *Simulator) savePlans() {
	pf := PlansFile{
		LastDramaDay:  s.lastDramaDay,
		ArcBook:       s.arcBook,
		DynamicAgents: s.dynamicAgents,
		Foreshadows:   s.foreshadows,
		CurrentArcNum: s.currentArcNum,
		ArcDone:       s.arcDone,
	}
	if data, err := json.MarshalIndent(pf, "", "  "); err == nil {
		_ = os.WriteFile(s.plansPath(), data, 0644)
	}
}

// loadPlans 重启后恢复导演层状态（大纲/部门/伏笔/时间锚点——防牛头不对马嘴）
func (s *Simulator) loadPlans() {
	data, err := os.ReadFile(s.plansPath())
	if err != nil {
		return
	}
	var pf PlansFile
	if json.Unmarshal(data, &pf) != nil {
		return
	}
	s.lastDramaDay = pf.LastDramaDay
	s.arcBook = pf.ArcBook
	s.dynamicAgents = pf.DynamicAgents
	s.foreshadows = pf.Foreshadows
	s.arcDone = pf.ArcDone
	// 恢复当前 open 段落（最后一条 status=open 的）
	s.currentArcNum = pf.CurrentArcNum
	for i := len(s.arcBook) - 1; i >= 0; i-- {
		if s.arcBook[i].Status == "open" {
			s.currentArc = s.arcBook[i].rawJSON()
			break
		}
	}
}

// legacyWeight 旧编年史条目（无 weight 字段）按 kind/source/关键词补默认戏剧权重
func legacyWeight(e ChronicleEntry) float64 {
	if e.Kind == "SAID" {
		return 0.6
	}
	switch e.Source {
	case "对话":
		return 0.6
	case "铺垫":
		return 0.7
	case "快进":
		return 0.5
	case "反思":
		return 0.7
	case "导演":
		return 0.85
	}
	low := e.Content
	for _, kw := range []string{"关系", "涟漪", "伏笔", "抉择", "感情", "心动", "告白", "分手", "决裂", "离开", "登场", "揭示", "浮出水面"} {
		if strings.Contains(low, kw) {
			return 0.7
		}
	}
	if e.Kind == "FACT" {
		return 0.55 // 旧事件/观察条目视为可写素材
	}
	return 0.3 // 其余 STATE 按日常处理
}

// loadChronicle 重启后从磁盘恢复编年史与 thinkings
func (s *Simulator) loadChronicle() {
	if data, err := os.ReadFile(s.chroniclePath()); err == nil {
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		for _, ln := range lines {
			ln = strings.TrimSpace(ln)
			if ln == "" {
				continue
			}
			var e ChronicleEntry
			if json.Unmarshal([]byte(ln), &e) == nil {
				if e.Weight <= 0 {
					e.Weight = legacyWeight(e) // 旧数据无权重：按 kind/source/关键词补默认
				}
				s.chronicle = append(s.chronicle, e)
			}
		}
	}
	if data, err := os.ReadFile(s.thinkingsPath()); err == nil {
		m := map[int]string{}
		if json.Unmarshal(data, &m) == nil && len(m) > 0 {
			s.thinkings = m
			// 恢复 lastThinking（最近一天的）
			var maxDay int
			for d := range m {
				if d > maxDay {
					maxDay = d
				}
			}
			s.lastThinking = m[maxDay]
		}
	}
	if len(s.chronicle) > 0 {
		fmt.Printf(" [模拟] 已恢复编年史 %d 条 / 思考 %d 天（day=%d）\n", len(s.chronicle), len(s.thinkings), s.day)
	}
}

// saveChronicle 把编年史与 thinkings 全量落盘（每次模拟后调用）
func (s *Simulator) saveChronicle() {
	if len(s.chronicle) == 0 {
		return
	}
	var sb strings.Builder
	for _, e := range s.chronicle {
		if b, err := json.Marshal(e); err == nil {
			sb.Write(b)
			sb.WriteByte('\n')
		}
	}
	if err := os.WriteFile(s.chroniclePath(), []byte(sb.String()), 0644); err != nil {
		fmt.Printf(" [模拟] 警告：编年史保存失败: %v\n", err)
	}
	if len(s.thinkings) > 0 {
		if b, err := json.MarshalIndent(s.thinkings, "", "  "); err == nil {
			if err := os.WriteFile(s.thinkingsPath(), b, 0644); err != nil {
				fmt.Printf(" [模拟] 警告：thinkings 保存失败: %v\n", err)
			}
		}
	}
	// 导演层状态一并落盘（段落大纲/部门负责人/伏笔账本/时间锚点——防重启丢情节）
	s.savePlans()
	// 环节级 token 用量落盘（观察"钱花在哪个 Agent"）
	if b, err := json.MarshalIndent(llm.SpanSummary(), "", "  "); err == nil {
		if err := os.WriteFile(filepath.Join(s.worldDir, "token_stats.json"), b, 0644); err != nil {
			fmt.Printf(" [模拟] 警告：token_stats 保存失败: %v\n", err)
		}
	}
}

// MemoryStore 返回角色记忆库（供外部查看）
func (s *Simulator) MemoryStore() *MemoryStore { return s.mem }

func (s *Simulator) Chronicle() []ChronicleEntry { return s.chronicle }
func (s *Simulator) Mode() string                { return s.mode }

func (s *Simulator) nextCmd(actor string) string {
	s.seq++
	return fmt.Sprintf("cmd-%s-%04d", actor, s.seq)
}

// RunDay 执行一天（Scene 全模拟 / Summary 轻模拟 / Skip 快进，§5.1/§5.2）
func (s *Simulator) RunDay(ctx context.Context) (*DayResult, error) {
	s.day++
	res := &DayResult{Day: s.day, Mode: s.mode}

	// ---------- 0. 张力引擎：自适应粒度决策（§5：auto 模式） ----------
	if s.modeAuto {
		s.decideMode(ctx)
	}
	res.Mode = s.mode

	// ---------- Skip 快进（§5.2：沿主角默认策略批量推进，1次LLM/块） ----------
	if s.mode == "skip" {
		return s.runSkip(ctx, res)
	}

	// ---------- 1. 事件生成（EventAgent：LLM 优先，dry-run 兜底） ----------
	s.events = nil
	if s.llm != nil {
		// 总导演 GM：当前段落结束或从未规划 → 规划下一个剧情段落（CEO 定战略方向）
		if s.currentArc == "" {
			if arc := GMAgentLLM(ctx, s.llm, s.engine.State(), s.wb, s.heroName,
				formatRecentEvents(s.chronicle, 8), s.OpenForeshadows(), s.engine.State().WorldLevel.Tension); arc != "" {
				s.currentArc = arc
				s.arcDone = 0
				// 段落名记入编年史（导演的镜头语言）
				var arcMeta struct {
					ArcName string `json:"arc_name"`
					Goal    string `json:"goal"`
				}
				if json.Unmarshal([]byte(arc), &arcMeta) == nil && arcMeta.ArcName != "" {
					s.chronicle = append(s.chronicle, ChronicleEntry{
						Day: s.day, Kind: "STATE", Time: now(),
						Content:    "导演开新段落【" + arcMeta.ArcName + "】" + arcMeta.Goal,
						Visibility: "public", Source: "导演",
					
						Weight: 0.85, Tags: []string{"导演", "段落"},})
					// 动态招聘：段落里的反派/主线 → 注册"负责人线"（公司新岗位）
					var arcFull struct {
						Villain string `json:"villain"`
						Goal    string `json:"goal"`
					}
					if json.Unmarshal([]byte(arc), &arcFull) == nil && strings.TrimSpace(arcFull.Villain) != "" {
						s.RegisterDynamicAgent(arcMeta.ArcName+"·主线负责人", "plot_line", arcMeta.Goal, arcFull.Villain)
					}
					// 段落大纲落盘（导演 outline：开段日/目标/反派/里程碑/爽点/时长——防牛头不对马嘴）
					var arcAll struct {
						Goal, Villain, ForeshadowFocus, Payoff, TimeHint string
						Milestones                                        []string
					}
					_ = json.Unmarshal([]byte(arc), &arcAll)
					s.currentArcNum++
					s.arcBook = append(s.arcBook, ArcEntry{
						Num: s.currentArcNum, Day: s.day, ArcName: arcMeta.ArcName,
						Goal: arcAll.Goal, Villain: arcAll.Villain, ForeshadowFocus: arcAll.ForeshadowFocus,
						Milestones: arcAll.Milestones, Payoff: arcAll.Payoff, TimeHint: arcAll.TimeHint,
						Status: "open",
					})
				}
			}
		}
		// 世界渐进揭示：E段深层设定浮出水面
	//   · day型：日期到点自动浮出（保底）
	//   · event型：靠下方事件命中线索触发（在事件生成后检查）
	revealedNow := ""
	if s.wb != nil {
		revealedNow = s.wb.CheckDayReveals(s.day)
	}
	revealedAll := ""
	if s.wb != nil {
		revealedAll = s.wb.RevealedAll()
	}
	unrevealed := ""
	if s.wb != nil {
		unrevealed = s.wb.UnrevealedHints()
	}
	luckHint := "今日无特殊幸运倾向"
	if rand.Intn(100) < 15 { // 15% 幸运日
		luckHint = "今日有幸运倾向"
	}
	// 事件 Agent 额外上下文：即将爆发的伏笔 + 活跃的负责人线（各部门在行动）
		extraCtx := ""
		if ripe := s.RipeForeshadows(); ripe != "" {
			extraCtx += "即将爆发的伏笔（酝酿成熟，今天该安排回收/爆发了）：" + ripe + "\n"
		}
		if da := s.DynamicAgentsState(); da != "" {
			extraCtx += "活跃的负责人线（各部门在行动，事件要呼应它们）：\n" + da
		}
		if evs, err := EventGenLLM(ctx, s.llm, s.engine.State(), s.wb, s.OpenForeshadows(), formatPendingEvents(s, s.day), revealedAll, unrevealed, luckHint, s.lastDramaDay, s.currentArc, extraCtx); err == nil && len(evs) > 0 {
		s.events = evs
		logx.M().RecordEvent(true, s.day)
		logx.Get("").Debug("模拟", "Day%d 事件生成成功 %d 条", s.day, len(evs))
	} else {
		if err != nil {
			logx.M().RecordEvent(false, s.day)
			logx.M().RecordDryRun(s.day)
			logx.Get("").Warn("模拟", "Day%d 事件生成失败(%v)，走 dry-run 兜底", s.day, logx.Trunc(err.Error(), 120))
			s.events = s.dryRunEvents()
		}
	}
	// 事件型深层层：事件命中线索 → 真相浮出水面
	if s.wb != nil && len(s.events) > 0 {
		var match strings.Builder
		for _, e := range s.events {
			match.WriteString(e.Title)
			match.WriteString(" ")
			match.WriteString(e.Frame)
			for _, nc := range e.NewChars {
			match.WriteString(" ")
			match.WriteString(nc.Persona)
		}
		}
		if trig := s.wb.TriggerByEvent(match.String()); trig != "" {
			revealedNow += trig
		}
	}
	// 新揭示 → 编年史 + 主角记忆（世界变大了）
	if revealedNow != "" {
		s.chronicle = append(s.chronicle, ChronicleEntry{
			Day: s.day, Kind: "STATE", Time: now(),
			Content:    "世界深处浮出水面：" + revealedNow,
			Visibility: "restricted", Source: "系统",
		
			Weight: 0.85, Tags: []string{"世界揭示"},})
	}
	}
	// ---------- 1.1 事件驱动：平淡期处理（铺垫 或 时间跳跃） ----------
	// 空事件或全是低价值事件 → 平淡期。不展开决策/NPC/世界推进，两层处理：
	//   ① 铺垫 Agent：捕捉平淡期的"暗流/变化"（伏笔滋长/环境渐变/人物微动/能力暗育/关系漂移）写入编年史
	//   ② 真无事：TimeSkipLLM 决定跳过多久 + 生成浓缩过渡段（时间跨度由剧情定，几天到半年都行）
	if s.isDullDay() {
		s.quietDays++
		// 平淡日 ≠ 无事发生：slice 生活切片（烟火气/日常细节）先落编年史——它们是写手"闲笔"的素材源，不能丢
		for _, ev := range s.events {
			if ev.Type == "slice" || ev.Severity < 0.4 {
				s.chronicle = append(s.chronicle, ChronicleEntry{
					Day: s.day, Kind: "FACT", Time: now(),
					Content:    "生活·" + ev.Title + "：" + ev.Frame,
					Visibility: "public", Source: "生活",
					
					Weight: 0.4, Tags: []string{"生活", ev.Type},})
			}
		}
		if s.quietDays == 1 {
			drifts := []DriftNote{}
			if s.llm != nil {
				drifts = DriftAgentLLM(ctx, s.llm, s.engine.State(), s.heroName,
					formatRecentEvents(s.chronicle, 3), s.OpenForeshadows(),
					s.day-s.lastDramaDay, s.engine.State().Weather)
			}
			if len(drifts) > 0 {
				// 铺垫日：暗流写进编年史（时间照常流动，轻量模拟——不展开决策/NPC）
				maxDays := 1
				for _, d := range drifts {
					if d.Days > maxDays {
						maxDays = d.Days
					}
					// 伏笔滋长类铺垫 → 推进该伏笔的酝酿度（慢慢攒，到0.8即将爆发）
					if d.Type == "foreshadow_growth" && strings.TrimSpace(d.Title) != "" {
						s.AdvanceForeshadowMaturity(d.Title, 0.15)
					}
					if strings.TrimSpace(d.Content) == "" {
						continue
					}
					s.chronicle = append(s.chronicle, ChronicleEntry{
						Day: s.day, Kind: "STATE", Time: now(),
						Content:    "铺垫·" + d.Title + "：" + d.Content,
						Visibility: "public", Source: "铺垫",
					
						Weight: 0.7, Tags: []string{"铺垫"},})
				}
				s.day += maxDays - 1 // RunDay 开头已 day++，这里再补 maxDays-1
			} else {
		// 真平淡：大步长跳跃 + 浓缩过渡段（网文式时间跳跃，带变化/积累/伏笔；跳跃量按世界时间尺度自由决定）
		skipText, skipDays := "", 30
		if s.llm != nil {
			skipText, skipDays = TimeSkipLLM(ctx, s.llm, s.engine.State(), s.heroName,
				formatRecentEvents(s.chronicle, 3), s.OpenForeshadows(), s.wb)
		}
		if skipText == "" {
			skipText = fmt.Sprintf("接下来的日子，%s照常活着，但有些东西在暗处一点点变化。", s.heroName)
				}
				s.chronicle = append(s.chronicle, ChronicleEntry{
					Day: s.day, Kind: "STATE", Time: now(),
					Content:    "时间过渡：" + skipText,
					Visibility: "public", Source: "系统",
				
					Weight: 0.5, Tags: []string{"时间过渡"},})
				s.day += skipDays - 1
			}
		}
		s.engine.State().Day = s.day
		if err := s.engine.Save(filepath.Join(s.worldDir, "world_state.json")); err != nil {
			return nil, fmt.Errorf("保存世界状态失败: %w", err)
		}
		s.saveChronicle()
		res.Events = nil
		res.Skipped = true
		return res, nil
	}
	s.quietDays = 0
	s.lastDramaDay = s.day
	// 里程碑计数：戏剧事件 → 当前段落进度+1；里程碑完成 → 段落结束（下一天总导演重新规划）
	if s.currentArc != "" {
		for _, ev := range s.events {
			if ev.Severity >= 0.5 || ev.Foreshadow != "" || len(ev.NewChars) > 0 {
				s.arcDone++
				break
			}
		}
		var arcMeta struct {
			Milestones []string `json:"milestones"`
		}
		milestoneN := 3
		if json.Unmarshal([]byte(s.currentArc), &arcMeta) == nil && len(arcMeta.Milestones) > 0 {
			milestoneN = len(arcMeta.Milestones)
		}
		if s.arcDone >= milestoneN {
			s.currentArc = "" // 段落收尾，下一天重新规划
			// 大纲账本标记完成（防重启后以为段落还开着）
			for i := len(s.arcBook) - 1; i >= 0; i-- {
				if s.arcBook[i].Status == "open" {
					s.arcBook[i].Status = "done"
					s.arcBook[i].DoneDay = s.day
					break
				}
			}
			s.chronicle = append(s.chronicle, ChronicleEntry{
				Day: s.day, Kind: "STATE", Time: now(),
				Content:    "本段剧情告一段落（里程碑完成，总导演将规划下一段）",
				Visibility: "public", Source: "导演",
			
				Weight: 0.8, Tags: []string{"导演", "里程碑"},})
		}
	}
	// 遭遇链：消费今天到期的种子事件 + 登记伏笔 + 排队后续事件
	if seeds := s.consumePendingEvents(s.day); len(seeds) > 0 {
		s.events = append(s.events, seeds...)
	}
	for _, ev := range s.events {
		if ev.Foreshadow != "" {
			s.RegisterForeshadow(ev.Foreshadow)
		}
		if ev.ResolveForeshadow != "" {
			s.ResolveForeshadow(ev.ResolveForeshadow, ev.Title)
		}
		if len(ev.NextEvents) > 0 {
			s.queuePendingEvents(s.day+1+(s.day%3), ev.NextEvents) // 1-3天后触发
		}
	}
	res.Events = s.events

	// ---------- 1.5 新角色注册（自然产生：女主/配角/对手由世界演化引入） ----------
	var regChanges []engine.Change
	for _, ev := range s.events {
		for _, nc := range ev.NewChars {
			regChanges = append(regChanges, s.RegisterCharacter(nc)...)
		}
	}
	// 角色档案：主角 + 已注册角色自动生成完整人设卡（灵魂化）——并行生成省时间
	if s.llm != nil {
		names := s.castNames()
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, name := range names {
			wg.Add(1)
			go func(n string) {
				defer wg.Done()
				changes := s.BuildCharacterSheet(ctx, n) // 只读state+LLM，结果统一提交
				mu.Lock()
				regChanges = append(regChanges, changes...)
				mu.Unlock()
			}(name)
		}
		wg.Wait()
	}
	if len(regChanges) > 0 {
		regProp := engine.Proposal{
			CommandID: s.nextCmd("cast"), ActorID: "world_agent",
			BaseRevision: s.engine.State().Revision, Type: "state_change",
			Changes: regChanges, Reason: "新角色登场（世界演化）",
		}
		if err := s.engine.Submit(ctx, &regProp); err == nil {
			res.Proposals = append(res.Proposals, regProp)
			for _, c := range regChanges {
				parts := strings.SplitN(c.Path, ".", 3)
				if len(parts) >= 2 && parts[0] == "entities" && parts[1] != s.heroName {
					s.mem.AddDay(s.heroName, fmt.Sprintf("Day%d：遇见了新面孔%s", s.day, parts[1]), "event", 0.6, s.day)
				}
			}
		}
	}
	// 事件关系影响（romance/crisis/conflict → 双向关系变化）
	var relChanges []engine.Change
	for _, ev := range s.events {
		if ev.RelEffect == "" {
			continue
		}
		for _, npc := range ev.NPCs {
			if npc == s.heroName {
				continue
			}
			relChanges = append(relChanges, s.UpdateRelation(s.heroName, npc, 0.12, 0.08, ev.RelEffect)...)
			break // 每事件主作用于第一个NPC
		}
	}
	if len(relChanges) > 0 {
		relProp := engine.Proposal{
			CommandID: s.nextCmd("rel"), ActorID: "world_agent",
			BaseRevision: s.engine.State().Revision, Type: "state_change",
			Changes: relChanges, Reason: "事件关系演化",
		}
		if err := s.engine.Submit(ctx, &relProp); err == nil {
			res.Proposals = append(res.Proposals, relProp)
		}
	}

	// ---------- 2. 世界推进（WorldAgent：LLM 优先，dry-run 兜底） ----------
	advance := engine.Proposal{
		CommandID:    s.nextCmd("world"),
		ActorID:      "world_agent",
		BaseRevision: s.engine.State().Revision,
		Type:         "state_change",
		Reason:       fmt.Sprintf("Day %d 世界推进", s.day),
	}
	if s.llm != nil {
		if p, err := WorldAdvanceLLM(ctx, s.llm, s.engine.State(), s.events, engine.Rules{}, s.wb, s.OpenForeshadows(), s.currentArc); err == nil && p != nil {
			advance.Changes = p.Changes
			if p.Reason != "" {
				advance.Reason = p.Reason
			}
		}
	}
	if len(advance.Changes) == 0 {
		advance.Changes = s.worldAdvanceChanges()
	}
	// 地点系统：首次初始化基础地点 + 缺感官的地点补感官档案（旧世界升级）
	advance.Changes = append(advance.Changes, s.SeedLocations()...)
	advance.Changes = append(advance.Changes, s.EnsureLocationSenses()...)
	if err := s.engine.Submit(ctx, &advance); err != nil {
		return nil, fmt.Errorf("世界推进失败: %w", err)
	}
	res.Proposals = append(res.Proposals, advance)

	// ---------- 3. 感知分发（§4.4：按主角位置/范围裁剪） ----------
	obs := s.buildObservation(s.events)

	// ---------- 4. 主角决策（ProtagonistAgent：三问决策法 LLM / dry-run） ----------
	var action *engine.Proposal
	if s.llm != nil {
		// 记忆注入（§4.6：按相关性召回主角记忆）
		memories := formatMemories(s.mem.Retrieve(s.heroName, "今天 近期 遭遇 关系 熟人 怪事 重要的事", 8))
		s.mem.StrengthenRetrieval(s.heroName, s.mem.Retrieve(s.heroName, "今天 近期 遭遇 关系 熟人 怪事 重要的事", 8)) // 检索强化（hippo）
		p, thinking, err := ProtagonistDecideLLM(ctx, s.llm, s.engine.State(), obs, s.heroName, s.wb, memories)
		if thinking != "" {
			s.lastThinking = thinking
			if s.thinkings == nil {
				s.thinkings = make(map[int]string)
			}
			s.thinkings[s.day] = thinking
		}
		if err != nil {
			// LLM 失败：记录并降级 dry-run
			res.Chronicle = append(res.Chronicle, ChronicleEntry{
				Day: s.day, Kind: "FACT", Time: now(),
				Content:    "主角决策调用失败（降级为模板行动）：" + err.Error(),
				Visibility: "public",
			
				Weight: 0.3, Tags: []string{"降级"},})
			action = s.protagonistAct(s.events)
		} else {
			action = p // p 可能为 nil（维持现状）
		}
	} else {
		action = s.protagonistAct(s.events)
	}
	if action != nil {
		action.CommandID = s.nextCmd("hero")
		action.ActorID = "protagonist"
		action.BaseRevision = s.engine.State().Revision
		action.Type = "state_change"
		if err := s.engine.Submit(ctx, action); err != nil {
			// 提案被硬约束/GM拒绝：记录为"行动受阻"，不中断循环
			res.Chronicle = append(res.Chronicle, ChronicleEntry{
				Day: s.day, Kind: "FACT", Time: now(),
				Content:    fmt.Sprintf("主角行动被规则拒绝：%v", err),
				Visibility: "public",
			
				Weight: 0.3, Tags: []string{"受阻"},})
		} else {
			res.Proposals = append(res.Proposals, *action)
			// ---------- 4.5 世界影响反馈（P0-3 蝴蝶效应：主角行动 → 世界变化） ----------
			if s.llm != nil {
				if impactChanges, impact := s.WorldImpactLLM(ctx, action.Reason); len(impactChanges) > 0 {
					impProp := engine.Proposal{
						CommandID: s.nextCmd("impact"), ActorID: "world_agent",
						BaseRevision: s.engine.State().Revision, Type: "state_change",
						Changes: impactChanges, Reason: "主角行动的世界影响" + impact,
					}
					if err := s.engine.Submit(ctx, &impProp); err == nil {
						res.Proposals = append(res.Proposals, impProp)
						if impact != "" {
							res.Chronicle = append(res.Chronicle, ChronicleEntry{
								Day: s.day, Kind: "STATE", Time: now(),
								Content:    "世界涟漪：" + impact,
								Visibility: "public",
							
								Weight: 0.6, Tags: []string{"涟漪"},})
						}
					}
				}
			}
		}
	}

	// ---------- 4.6 岔口决策入队（AI 代决零阻塞：主角行动即 AI 推荐方向；用户可事后改选，写手按最终方向写） ----------
	aiAction, aiReason := "", ""
	if action != nil {
		aiAction = action.Reason
		aiReason = "主角三问决策（AI 代决，用户可改选）"
	}
	s.captureDecisions(aiAction, aiReason)

	// ---------- 5. NPC 互动对话（§5.1：含 NPC 的事件触发，Init→Act→React） ----------
	var dialogue []DialogueTurn
	for _, ev := range s.events {
		if len(ev.NPCs) > 0 || strings.HasPrefix(ev.FirstActor, "npc_") {
			turns, prop, err := s.RunDialogue(ctx, ev)
			if err == nil && prop != nil {
				prop.CommandID = s.nextCmd("npc")
				prop.ActorID = "npc_" + ev.NPCs[0]
				prop.BaseRevision = s.engine.State().Revision
				prop.Type = "state_change"
				if err := s.engine.Submit(ctx, prop); err == nil {
					res.Proposals = append(res.Proposals, *prop)
				}
			}
			// 对话写入编年史（SAID：命题真假待定，可信度 0.6）
			for _, t := range turns {
				s.chronicle = append(s.chronicle, ChronicleEntry{
					Day: s.day, Kind: "SAID", Time: now(),
					Content:     fmt.Sprintf("%s说：%s", t.Speaker, t.Speech),
					Visibility:  "public",
					Source:      "对话",
					Credibility: 0.6,
				
					Weight: 0.6, Tags: []string{"对话", t.Speaker},})
			}
			dialogue = append(dialogue, turns...)
			break // MVP：每天最多一段对话
		}
	}
	res.Dialogue = dialogue

	// ---------- 6. GM 裁决 = engine.Submit 内软规则（已在上面执行） ----------

	// ---------- 7. 记录（§9.10：FACT/SAID/STATE） ----------
	s.recordChronicle(obs, res.Proposals, res.Events, res)

	// ---------- 8. 暂停检查（§10：severity≥0.75 → 抉择点） ----------
	for _, ev := range s.events {
		if ev.Severity >= 0.75 {
			res.Paused = true
			res.PauseMsg = fmt.Sprintf("重大事件：%s（severity %.2f）——请抉择", ev.Title, ev.Severity)
			break
		}
	}

	// 状态保存（先同步 day 再落盘，避免滞后）
	s.engine.State().Day = s.day
	logx.M().RecordSimDay(s.day)
	if err := s.engine.Save(filepath.Join(s.worldDir, "world_state.json")); err != nil {
		return nil, fmt.Errorf("保存世界状态失败: %w", err)
	}
	// 记忆写入（§4.6）：当日事件 → 主角记忆；世界变化 → 世界记忆
	s.recordMemories(res)

	// ---------- 关系系统：衰减 + 生命周期（"只见过几年"的悲欢）+ 配角淡出 ----------
	relChanges = nil
	relChanges = append(relChanges, s.DecayRelations(s.heroName, 7)...) // 每7天好感衰减
	relChanges = append(relChanges, s.CheckLifecycle()...)
	relChanges = append(relChanges, s.FadeOutCheck()...)
	if len(relChanges) > 0 {
		lifeProp := engine.Proposal{
			CommandID: s.nextCmd("life"), ActorID: "world_agent",
			BaseRevision: s.engine.State().Revision, Type: "state_change",
			Changes: relChanges, Reason: "关系演化/角色生命周期",
		}
		if err := s.engine.Submit(ctx, &lifeProp); err == nil {
			res.Proposals = append(res.Proposals, lifeProp)
		}
	}

// ---------- 记忆膨胀治理：按记忆量驱动 + 批量巩固（一次 LLM 整理多人，省 token） ----------
	if s.llm != nil && s.mem != nil {
		// 攒够 60 条的角色才巩固；一次调用批量整理、按人分发
		// 平淡期跳过了就不压（省算力），密集期攒得快就多压（防膨胀）
		actors := s.mem.Actors()
		if n := s.mem.BatchConsolidate(ctx, s.llm, actors, 60); len(n) > 0 {
			res.Chronicle = append(res.Chronicle, ChronicleEntry{
				Day: s.day, Kind: "STATE", Time: now(),
				Content:    fmt.Sprintf("记忆巩固：%d个角色的细碎记忆已批量浓缩（记忆量驱动，防膨胀）", len(n)),
				Visibility: "public", Source: "系统",
			
				Weight: 0.15, Tags: []string{"系统"},})
		}
	}

	// 编年史 + thinkings 落盘（§14.1：断点续传，小说化素材不丢）
	s.saveChronicle()
	if s.mem != nil {
		s.mem.Save()
	}
	return res, nil
}

// recordMemories 把当天模拟成果写入角色记忆流（双方记忆：参与交互的每个角色都记，杜绝配角失忆）
func (s *Simulator) recordMemories(res *DayResult) {
	if s.mem == nil {
		return
	}
	// ① 事件：主角记 + 事件涉及的 NPC 也记（各自视角）
	// 龙套（walkon）不占记忆：只记主角视角的"遇到"，不给龙套建独立记忆
	for _, ev := range res.Events {
		imp := 0.3 + ev.Severity*0.7
		s.mem.AddDay(s.heroName, fmt.Sprintf("第%d天：%s（%s）", ev.Day, ev.Title, ev.Frame), "event", imp, ev.Day)
		for _, npc := range ev.NPCs {
			if npc == "" || npc == s.heroName {
				continue
			}
			if s.isWalkon(npc) {
				continue // 龙套不建独立记忆
			}
			s.mem.AddDay(npc, fmt.Sprintf("第%d天：我卷进了这件事——%s", ev.Day, ev.Title), "event", imp*0.9, ev.Day)
		}
	}
	// ② 对话：双方记忆（说话人记自己的话，其他在场者记听到了什么）
	participants := []string{s.heroName}
	for _, t := range res.Dialogue {
		if t.Speaker != "" && t.Speaker != s.heroName && !s.isWalkon(t.Speaker) {
			participants = append(participants, t.Speaker)
		}
	}
	seen := map[string]bool{}
	var uniq []string
	for _, p := range participants {
		if p != "" && !seen[p] {
			seen[p] = true
			uniq = append(uniq, p)
		}
	}
	for _, t := range res.Dialogue {
		if t.Speaker == "" {
			continue
		}
		imp := 0.5
		s.mem.AddDay(t.Speaker, fmt.Sprintf("第%d天：我对在场的人说：%s", res.Day, t.Speech), "dialogue", imp, res.Day)
		for _, p := range uniq {
			if p != t.Speaker {
				s.mem.AddDay(p, fmt.Sprintf("第%d天：%s对我说：%s", res.Day, t.Speaker, t.Speech), "dialogue", imp*0.8, res.Day)
			}
		}
	}
	// ③ 状态变化 → 涉及者记忆（谁的状态变了给谁记）
	for _, p := range res.Proposals {
		for _, c := range p.Changes {
			if strings.Contains(c.Path, s.heroName) {
				s.mem.AddDay(s.heroName, fmt.Sprintf("第%d天：状态变化 %s %s %v", res.Day, c.Path, c.Op, c.Value), "state", 0.4, res.Day)
			}
		}
	}
	// ④ 定期反思（每5天）：主角 + 当天有互动的角色提炼高阶认知（批量一次调用，省 token）
	if s.llm != nil && res.Day%5 == 0 {
		reflActors := []string{s.heroName}
		for _, p := range uniq {
			if p != s.heroName {
				reflActors = append(reflActors, p)
			}
		}
		for actor, refl := range s.mem.BatchReflect(ctxTODO(), s.llm, reflActors) {
			s.chronicle = append(s.chronicle, ChronicleEntry{
				Day: res.Day, Kind: "STATE", Time: now(),
				Content:    actor + "反思：" + refl,
				Visibility: "private", Source: "反思",
			
				Weight: 0.7, Tags: []string{"反思"},})
		}
	}
}

// ctxTODO 占位：反思用独立上下文（后续可配置超时）
func ctxTODO() context.Context { return context.Background() }

// ---------- 1. 世界推进（dry-run：天数+1、天气轮换、张力微调） ----------

var weatherPool = []string{"晴", "多云", "雨", "暴雨", "雾", "晴", "阴"}

func (s *Simulator) worldAdvanceChanges() []engine.Change {
	w := weatherPool[s.day%len(weatherPool)]
	// 张力：平淡日微降，事件日微升（由事件 severity 决定）
	tension := s.engine.State().WorldLevel.Tension
	if len(s.events) > 0 {
		tension += 0.05 * float64(len(s.events))
	} else {
		tension -= 0.02
	}
	if tension < 0 {
		tension = 0
	}
	if tension > 1 {
		tension = 1
	}
	return []engine.Change{
		{Path: "world_level.tension", Op: "set", Value: round2(tension)},
		{Path: "world_level.weather", Op: "set", Value: w},
	}
}

// ---------- 2. 事件生成（dry-run：从模板池抽取） ----------

// isDullDay 平淡日判定：空事件，或所有事件都是低价值日常（severity<0.4 且无伏笔/新角色/关系/后续事件）
// 事件驱动核心：平淡日快进时间，不展开模拟，预算留给戏剧日
func (s *Simulator) isDullDay() bool {
	if len(s.events) == 0 {
		return true
	}
	for _, ev := range s.events {
		// 有任何一个"值得展开"的事件 → 不是平淡日
		if ev.Severity >= 0.4 || ev.Foreshadow != "" || len(ev.NewChars) > 0 || ev.RelEffect != "" || len(ev.NextEvents) > 0 {
			return false
		}
	}
	return true
}

// dryRunEvents 无 LLM 时的模板事件生成（确定性兜底）
func (s *Simulator) dryRunEvents() []EventCard {
	events := []EventCard{}
	for i := 0; i < 1+rand.Intn(2); i++ {
		ev := s.generateEvent()
		if ev != nil {
			events = append(events, *ev)
		}
	}
	return events
}

func (s *Simulator) generateEvent() *EventCard {
	// 通用 fallback 模板池（不含任何都市专属设定：地点/NPC 从世界状态取，任何世界都能用）
	type genericTpl struct {
		Type     string
		Title    string
		FrameFmt string // %s=地点，%s=NPC名
		Severity float64
		Options  []string
	}
	genericTemplates := []genericTpl{
		{Type: "slice", Title: "日复一日的活计", Severity: 0.25,
			FrameFmt: "%s，%s照旧忙着手里的活，看似平常的一天，总有些不对劲的细节。", Options: []string{"留意那点不对劲", "只当无事发生"}},
		{Type: "daily", Title: "欲言又止", Severity: 0.3,
			FrameFmt: "在%s，%s看见你，话到嘴边又咽了回去，欲言又止。", Options: []string{"主动问一句", "等对方先说"}},
		{Type: "conflict", Title: "来者不善", Severity: 0.55,
			FrameFmt: "在%s，有人拦住去路，来者不善，眼神里带着算计。", Options: []string{"正面应对", "先避开", "试探虚实"}},
		{Type: "wonder", Title: "一件怪事", Severity: 0.7,
			FrameFmt: "路过%s时，%s撞见一件怪事——说不清哪里不对，但就是不对劲。", Options: []string{"停下来查看", "记在心里先离开"}},
	}
	tpl := genericTemplates[rand.Intn(len(genericTemplates))]
	// 地点：主角所在地优先，否则世界第一个地点
	loc := s.engine.State().Entities[s.heroName].Location
	if loc == "" {
		for n := range s.engine.State().WorldLevel.Locations {
			loc = n
			break
		}
	}
	// NPC：第一个非主角的实体（由世界初始化决定）
	npcName := ""
	for n := range s.engine.State().Entities {
		if n != s.heroName {
			npcName = n
			break
		}
	}
	if npcName == "" {
		npcName = "熟人"
	}
	ev := &EventCard{
		ID:         fmt.Sprintf("ev-%03d-%d", s.day, len(s.events)+1),
		Day:        s.day,
		Type:       tpl.Type,
		Title:      tpl.Title,
		Location:   loc,
		Severity:   tpl.Severity,
		Frame:      fmt.Sprintf(tpl.FrameFmt, loc, npcName),
		FirstActor: "npc_" + npcName,
		Options:    tpl.Options,
	}
	if tpl.Type == "slice" || tpl.Type == "wonder" {
		ev.FirstActor = "protagonist"
		ev.NPCs = nil
	} else {
		ev.NPCs = []string{npcName}
	}
	return ev
}

// ---------- 3. 感知分发（§4.4） ----------

func (s *Simulator) buildObservation(events []EventCard) ObservationPacket {
	pkt := ObservationPacket{Recipient: "protagonist", Day: s.day}
	for _, ev := range events {
		// 亲眼：主角所在位置的事件
		if ev.Location == s.engine.State().Entities[s.heroName].Location || ev.Severity >= 0.5 {
			pkt.Observations = append(pkt.Observations, Observation{
				ID:            fmt.Sprintf("obs-%03d-%d", s.day, len(pkt.Observations)+1),
				Source:        "亲眼",
				Content:       ev.Frame,
				VisibleTo:     "在场者",
				LocationScope: ev.Location,
				Credibility:   0.95,
				ArrivalDay:    s.day,
				WritableToMem: true,
			})
		} else {
			// 听说/媒体：传播范围裁剪
			pkt.Observations = append(pkt.Observations, Observation{
				ID:            fmt.Sprintf("obs-%03d-%d", s.day, len(pkt.Observations)+1),
				Source:        "听说",
				Content:       "街坊议论：" + ev.Title,
				VisibleTo:     "本地居民",
				LocationScope: ev.Location,
				Credibility:   0.5,
				ArrivalDay:    s.day,
				WritableToMem: true,
			})
		}
	}
	return pkt
}

// ---------- 4. 主角决策（dry-run：随机选选项 → 状态变更提案） ----------

func (s *Simulator) protagonistAct(events []EventCard) *engine.Proposal {
	if len(events) == 0 {
		// 无事发生：日常微调（如买早餐扣钱）
		return &engine.Proposal{
			Changes: []engine.Change{
				{Path: "entities." + s.heroName + ".money", Op: "add", Value: -8.0},
			},
			Reason: "日常开销",
		}
	}
	ev := events[0]
	switch ev.Type {
	case "wonder":
		// 奇遇：决定"下车查看"（推动剧情）——选择第一个选项
		return &engine.Proposal{
			Changes: []engine.Change{
				{Path: "entities." + s.heroName + ".location", Op: "set", Value: ev.Location},
				{Path: "entities." + s.heroName + ".extra.curiosity", Op: "set", Value: 1},
			},
			Reason:       "面对" + ev.Title + "，选择了下车查看",
			SourceEvents: []string{ev.ID},
		}
	case "conflict":
		return &engine.Proposal{
			Changes: []engine.Change{
				{Path: "entities." + s.heroName + ".health", Op: "add", Value: -5.0},
			},
			Reason:       "在" + ev.Title + "中吃了点亏",
			SourceEvents: []string{ev.ID},
		}
	default:
		return &engine.Proposal{
			Changes: []engine.Change{
				{Path: "entities." + s.heroName + ".money", Op: "add", Value: -10.0},
			},
			Reason:       "在" + ev.Title + "中有日常消费",
			SourceEvents: []string{ev.ID},
		}
	}
}

// ---------- 6. 记录（§9.10） ----------

func (s *Simulator) recordChronicle(obs ObservationPacket, proposals []engine.Proposal, events []EventCard, res *DayResult) {
	// STATE：来自已提交提案
	for _, p := range proposals {
		for _, c := range p.Changes {
			s.chronicle = append(s.chronicle, ChronicleEntry{
				Day: s.day, Kind: "STATE", Time: now(),
				Content:    fmt.Sprintf("[%s] %s → %v", c.Path, c.Op, c.Value),
				Visibility: "public",
			
				Weight: 0.25, Tags: []string{"状态"},})
		}
	}
	// FACT：来自事件/感知
	for _, o := range obs.Observations {
		s.chronicle = append(s.chronicle, ChronicleEntry{
			Day: s.day, Kind: "FACT", Time: now(),
			Content:    o.Content,
			Visibility: "public",
			Source:     o.Source,
			Weight:     0.5,
			Tags:       []string{"观察"},
		})
	}
	for _, ev := range events {
		s.chronicle = append(s.chronicle, ChronicleEntry{
			Day: s.day, Kind: "FACT", Time: now(),
			Content:    fmt.Sprintf("【%s】%s：%s", ev.Type, ev.Title, ev.Frame),
			Visibility: "public",
		
			Weight: 0.3+ev.Severity*0.7, Tags: []string{"事件", ev.Type},})
	}
	res.Chronicle = s.chronicle
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}