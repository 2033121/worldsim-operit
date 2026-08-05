// Package worldbook 实现世界书的加载与分层注入（设计文档 §3.3 / §4.5）
//
// 世界书 = 世界的"宪法"。按 Part A-D 组织，按 L1-L5 知识层分发：
//   - 主角/NPC 只能拿到"角色认知视角"的知识（不能全知）
//   - 世界 Agent / GM 持有全部（含 L5 系统秘密）
//   - 小说化 Agent 额外持有叙事约束（Part C）与伏笔清单（B4）
package worldbook

import (
	"fmt"
	"os"
	"strings"
)

type Worldbook struct {
	Title       string
	A1Worldview string // 世界观（含隐藏真相部分）
	A2Physics   string // 物理/超自然规则（L1）
	A3Society   string // 社会结构（L2）
	A4Geography string // 地理（L3）
	A5Factions  string // 势力速览（明面部分）
	A6GoalChain    string // 主角目标链（长期/阶段/即时，网文引擎）
	A7PowerSys     string // 能力成长体系（等级/解锁/升级时刻，网文引擎）
	A8Villain      string // 反派行动线（谁在动/怎么压迫，网文引擎）
	A9GoldenFinger string // 金手指设计（稀缺性/代价性/成长性+展示五步，网文DNA）
	A10PayoffRhythm string // 爽点循环规划（四类爽点交替+密度表+首次爽点时机，网文DNA）
	A11MapProgression string // 地图阶梯（2~4阶段+每阶段境界门槛+爽点重置，网文DNA）
	A12FaceSlapCycle string // 打脸周期表（被压迫→打脸→展示 的周期安排，网文DNA）
	B1Secrets   string // 世界秘密（L5）
	B2EventPool string // 事件类型池（导演内部）
	B3ArcPlan   string // 全书弧线建议（导演内部）
	B4Foreshadows string // 隐藏伏笔清单（导演内部）
	B5EventPool   string // 事件谱（本世界会发生的事，事件生成器的弹药库）
	CNarrative  string // 叙事约束（小说化专属）
	DSafety     string // 内容安全边界
	Raw         string
	// 深层世界观层（E段：世界一开始就很大，随时间渐进揭示——冰山理论）
	DeferredLayers []DeferredLayer
	pendingMarker  string // 解析中的E段标题触发标记（临时）
}

// DeferredLayer 深层世界观层：世界一开始就很大，只透露一部分
// Trigger: "day"=日期到点自动浮出（保底）| "event"=靠事件/探索触发（更自然）
type DeferredLayer struct {
	Title     string `json:"title"`
	RevealDay int    `json:"reveal_day"` // 最早解锁天数（0=仅事件触发）
	Trigger   string `json:"trigger"`    // day | event
	EventHint string `json:"event_hint"` // 事件型的触发线索（注入事件Agent）
	Content   string `json:"content"`
	Revealed  bool   `json:"revealed"`
}

// Load 从 Markdown 文件加载世界书（按 "## A1 标题" 等二级标题切分）
func Load(path string) (*Worldbook, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(string(raw)), nil
}

// Parse 解析世界书文本（Markdown 二级标题切分）
func Parse(raw string) *Worldbook {
	w := &Worldbook{Raw: raw}
	lines := strings.Split(raw, "\n")
	var current []string
	var prevSec string // 上一个普通 section 名（A1~A12/B1~B5/C/D）
	var eSec, eMarker string
	var eBody []string
	// flushE 把已收集的E段正文压入 DeferredLayers
	flushE := func() {
		if eSec == "" || len(eBody) == 0 {
			eSec, eMarker, eBody = "", "", nil
			return
		}
		layer := DeferredLayer{Title: eSec, Content: strings.TrimSpace(strings.Join(eBody, "\n")), Trigger: "day"}
		marker := eMarker
		if strings.HasPrefix(marker, "事件触发") {
			layer.Trigger = "event"
			if c := strings.Index(marker, "："); c >= 0 {
				layer.EventHint = strings.TrimSpace(marker[c+len("："):])
			} else if c := strings.Index(marker, ":"); c >= 0 {
				layer.EventHint = strings.TrimSpace(marker[c+1:])
			}
		} else {
			var day int
			if _, err := fmt.Sscanf(marker, "Day%d", &day); err == nil {
				layer.RevealDay = day
			}
		}
		// 未指定天数：按 E 序号递增（E1=Day10, E2=Day20...）
		if layer.RevealDay == 0 && layer.Trigger == "day" {
			n := 0
			fmt.Sscanf(eSec[1:], "%d", &n)
			if n > 0 {
				layer.RevealDay = n * 10
			}
		}
		w.DeferredLayers = append(w.DeferredLayers, layer)
		eSec, eMarker, eBody = "", "", nil
	}
	collect := func(section string) {
		if len(current) == 0 {
			return
		}
		body := strings.TrimSpace(strings.Join(current, "\n"))
		switch section {
		case "A1":
			w.A1Worldview = body
		case "A2":
			w.A2Physics = body
		case "A3":
			w.A3Society = body
		case "A4":
			w.A4Geography = body
		case "A5":
		w.A5Factions = body
	case "A6":
		w.A6GoalChain = body
	case "A7":
		w.A7PowerSys = body
	case "A8":
		w.A8Villain = body
	case "A9":
		w.A9GoldenFinger = body
	case "A10":
		w.A10PayoffRhythm = body
	case "A11":
		w.A11MapProgression = body
	case "A12":
		w.A12FaceSlapCycle = body
	case "B1":
			w.B1Secrets = body
		case "B2":
			w.B2EventPool = body
		case "B3":
			w.B3ArcPlan = body
		case "B4":
			w.B4Foreshadows = body
		case "B5":
			w.B5EventPool = body
		case "C":
			w.CNarrative = body
		case "C1":
			w.CNarrative = body
		case "D":
			w.DSafety = body
		}
	}

	// isSectionCode 判断是否为合法的世界书章节码：
	//   A1~A12（两位数以内）、B1~B5、C/C1、D、E1~E9
	//   注意：A10/A11/A12 是 3 字符，不能用 len==2 判断
	isSectionCode := func(s string) bool {
		if s == "" {
			return false
		}
		switch s[0] {
		case 'A', 'B', 'E':
			if len(s) < 2 {
				return false
			}
			for i := 1; i < len(s); i++ {
				if s[i] < '0' || s[i] > '9' {
					return false
				}
			}
			return true
		case 'C', 'D':
			return s == "C" || s == "C1" || s == "D"
		}
		return false
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// 匹配 "## A1 世界观" / "## A2 物理与超自然规则" / "## B1 ..." / "## E1 ..." 等
		if strings.HasPrefix(trimmed, "## ") {
			parts := strings.SplitN(strings.TrimPrefix(trimmed, "## "), " ", 2)
			sec := strings.ToUpper(strings.TrimSpace(parts[0]))
			if isSectionCode(sec) {
				if sec[0] == 'E' {
					// E段独立收集：flush上一个E段，开始新E段
					flushE()
					eSec = sec
					eMarker = ""
					if len(parts) > 1 {
						head := parts[1]
						if idx := strings.Index(head, "【"); idx >= 0 {
						rest := head[idx:]
						if end := strings.Index(rest, "】"); end > 0 {
							eMarker = strings.TrimSpace(rest[len("【"):end])
						}
					}
					}
					continue
				}
				// 普通段：先把已收集的正文归到上一个 section，再开始新 section
				flushE()
				collect(prevSec)
				prevSec = sec
				current = []string{}
				if w.Title == "" && sec == "A1" {
					w.Title = strings.TrimSpace(parts[1])
				}
				continue
			}
		}
		if eSec != "" {
			eBody = append(eBody, line)
			continue
		}
		if strings.HasPrefix(trimmed, "# ") && w.Title == "" {
			w.Title = strings.TrimPrefix(trimmed, "# ")
		}
		if current != nil {
			current = append(current, line)
		}
	}
	// 文件末尾：flush残留的E段 + 把最后一段正文归到最后一个 section
	flushE()
	collect(prevSec)
	return w
}

// CheckDayReveals 日期型层：到点自动浮出（保底机制），返回新揭示内容
func (w *Worldbook) CheckDayReveals(currentDay int) string {
	if w == nil || len(w.DeferredLayers) == 0 {
		return ""
	}
	var newSb strings.Builder
	for i := range w.DeferredLayers {
		l := &w.DeferredLayers[i]
		if l.Trigger == "day" && l.RevealDay > 0 && currentDay >= l.RevealDay && !l.Revealed {
			l.Revealed = true
			newSb.WriteString(fmt.Sprintf("【世界深层·%s】\n%s\n", l.Title, l.Content))
		}
	}
	return strings.TrimSpace(newSb.String())
}

// TriggerByEvent 事件型层：被事件/探索命中时触发揭示（返回新揭示内容）
func (w *Worldbook) TriggerByEvent(match string) string {
	if w == nil || len(w.DeferredLayers) == 0 || match == "" {
		return ""
	}
	var newSb strings.Builder
	for i := range w.DeferredLayers {
		l := &w.DeferredLayers[i]
		if l.Trigger != "event" || l.Revealed {
			continue
		}
		hit := strings.Contains(match, l.Title)
		if !hit {
			// hint 关键词拆解：支持 "|" 或 "、" 分隔的多关键词，任一命中即触发
			sep := func(s string) []string {
				for _, sp := range []string{"|", "、", "，", ",", "；", ";", " "} {
					if strings.Contains(s, sp) {
						return strings.Split(s, sp)
					}
				}
				return []string{s}
			}
			for _, kw := range sep(l.EventHint) {
				kw = strings.TrimSpace(kw)
				if kw != "" && strings.Contains(match, kw) {
					hit = true
					break
				}
			}
		}
		if hit {
			l.Revealed = true
			newSb.WriteString(fmt.Sprintf("【世界深层·%s】\n%s\n", l.Title, l.Content))
		}
	}
	return strings.TrimSpace(newSb.String())
}

// UnrevealedHints 尚未揭示的事件型层的线索（存在感：世界很大，有些东西还没浮出水面）
func (w *Worldbook) UnrevealedHints() string {
	if w == nil || len(w.DeferredLayers) == 0 {
		return ""
	}
	var sb strings.Builder
	for i := range w.DeferredLayers {
		l := &w.DeferredLayers[i]
		if !l.Revealed {
			if l.Trigger == "event" && l.EventHint != "" {
				sb.WriteString(fmt.Sprintf("· %s（线索：%s）\n", l.Title, l.EventHint))
			} else if l.Trigger == "day" {
				sb.WriteString(fmt.Sprintf("· %s（有风声，但真相还没浮出水面）\n", l.Title))
			}
		}
	}
	return strings.TrimSpace(sb.String())
}

// RevealedAll 返回所有已揭示的深层层（给事件Agent当全貌背景，主角只能接触冰山一角）
func (w *Worldbook) RevealedAll() string {
	if w == nil || len(w.DeferredLayers) == 0 {
		return ""
	}
	var sb strings.Builder
	for i := range w.DeferredLayers {
		l := &w.DeferredLayers[i]
		if l.Revealed {
			sb.WriteString(fmt.Sprintf("【%s】%s\n", l.Title, l.Content))
		}
	}
	return strings.TrimSpace(sb.String())
}

// ForProtagonist 主角视角：社会常识+地区常识+明面势力；物理规则用"角色认知视角"
func (w *Worldbook) ForProtagonist(heroName, heroProfile string) string {
	var sb strings.Builder
	sb.WriteString("【世界·你眼中的样子（普通人的认知）】\n")
	if w.A3Society != "" {
		sb.WriteString("· 社会：\n" + w.A3Society + "\n")
	}
	if w.A4Geography != "" {
		sb.WriteString("· 地理：\n" + w.A4Geography + "\n")
	}
	if w.A5Factions != "" {
		sb.WriteString("· 你听过的势力：\n" + w.A5Factions + "\n")
	}
	// L1 角色认知视角：只描述"你作为普通人相信什么"，真值不透露（按世界书的普通人视角描述）
	if w.A1Worldview != "" {
		sb.WriteString("· 关于这个世界，你作为普通人的认知：\n" + w.A1Worldview + "\n")
	} else {
		sb.WriteString("· 关于这个世界，你只相信自己亲眼见过、亲耳听过的事。\n")
	}
	if heroProfile != "" {
		sb.WriteString("· 你自己：" + heroProfile + "\n")
	}
	return sb.String()
}

// ForWorldAgent 世界/GM 视角：全部知识（含 L5 秘密）
func (w *Worldbook) ForWorldAgent() string {
	var sb strings.Builder
	sb.WriteString("【世界真相（你是唯一知情人，严格保密）】\n")
	sb.WriteString("A1 世界观：\n" + w.A1Worldview + "\n")
	sb.WriteString("A2 物理/超自然规则：\n" + w.A2Physics + "\n")
	if w.B1Secrets != "" {
		sb.WriteString("B1 世界秘密（绝不泄露给角色）：\n" + w.B1Secrets + "\n")
	}
	if w.B3ArcPlan != "" {
		sb.WriteString("B3 弧线建议（导演意图）：\n" + w.B3ArcPlan + "\n")
	}
	if w.B4Foreshadows != "" {
		sb.WriteString("B4 伏笔清单：\n" + w.B4Foreshadows + "\n")
	}
	return sb.String()
}

// ForGM 总导演视角：世界观+规则+目标+能力+反派+网文DNA（金手指/爽点/地图/打脸）+弧线+伏笔
// 总导演需要完整的网文蓝图来规划"目标→行动→收获→展示"四步循环
func (w *Worldbook) ForGM() string {
	var sb strings.Builder
	sb.WriteString("【总导演蓝图（你是CEO，按网文方法论规划剧情段落）】\n")
	sb.WriteString("A1 世界观：\n" + w.A1Worldview + "\n")
	if w.A2Physics != "" {
		sb.WriteString("A2 世界规则：\n" + w.A2Physics + "\n")
	}
	if w.A6GoalChain != "" {
		sb.WriteString("A6 主角目标链：\n" + w.A6GoalChain + "\n")
	}
	if w.A7PowerSys != "" {
		sb.WriteString("A7 能力成长体系：\n" + w.A7PowerSys + "\n")
	}
	if w.A8Villain != "" {
		sb.WriteString("A8 反派行动线：\n" + w.A8Villain + "\n")
	}
	if w.A9GoldenFinger != "" {
		sb.WriteString("A9 金手指设计（规划段落时安排金手指展示五步：存在→用法→代价→实战→升级）：\n" + w.A9GoldenFinger + "\n")
	}
	if w.A10PayoffRhythm != "" {
		sb.WriteString("A10 爽点循环规划（段落规划必须含爽点节奏：四类交替+密度控制+憋放结合）：\n" + w.A10PayoffRhythm + "\n")
	}
	if w.A11MapProgression != "" {
		sb.WriteString("A11 地图阶梯（段落规划配合地图阶段：当前地图的爽点周期是否该收尾、是否该切新地图）：\n" + w.A11MapProgression + "\n")
	}
	if w.A12FaceSlapCycle != "" {
		sb.WriteString("A12 打脸周期表（段落规划安排打脸周期：被压迫→打脸→展示，别一直憋也别一直打）：\n" + w.A12FaceSlapCycle + "\n")
	}
	if w.B3ArcPlan != "" {
		sb.WriteString("B3 弧线建议：\n" + w.B3ArcPlan + "\n")
	}
	if w.B4Foreshadows != "" {
		sb.WriteString("B4 伏笔清单：\n" + w.B4Foreshadows + "\n")
	}
	return sb.String()
}

// ForEventAgent 事件 Agent 视角：A + 事件类型池 + 事件谱 + 网文DNA（本世界弹药库）
func (w *Worldbook) ForEventAgent() string {
	var sb strings.Builder
	sb.WriteString("【世界背景与事件类型】\n")
	sb.WriteString("A1 世界观：\n" + w.A1Worldview + "\n")
	if w.A2Physics != "" {
		sb.WriteString("A2 世界规则：\n" + w.A2Physics + "\n")
	}
	if w.A9GoldenFinger != "" {
		sb.WriteString("A9 金手指设计（事件里要让金手指有存在感：展示/用法/代价/实战/升级，按当前阶段安排对应节奏）：\n" + w.A9GoldenFinger + "\n")
	}
	if w.A10PayoffRhythm != "" {
		sb.WriteString("A10 爽点循环规划（事件要配合爽点节奏：连憋几天后必须安排释放，四类爽点交替别单一）：\n" + w.A10PayoffRhythm + "\n")
	}
	if w.A12FaceSlapCycle != "" {
		sb.WriteString("A12 打脸周期表（有人看不起主角→主角碾压→围观震惊，按周期安排打脸事件）：\n" + w.A12FaceSlapCycle + "\n")
	}
	if w.B2EventPool != "" {
		sb.WriteString("B2 事件类型池（你的弹药库）：\n" + w.B2EventPool + "\n")
	}
	if w.B5EventPool != "" {
		sb.WriteString("B5 本世界事件谱（优先从这里挑事件，落到本世界的具体样子）：\n" + w.B5EventPool + "\n")
	}
	return sb.String()
}

// ForNovelist 小说化 Agent 视角：A + 伏笔 + 事件谱 + 叙事约束 + 网文DNA
func (w *Worldbook) ForNovelist() string {
	var sb strings.Builder
	sb.WriteString("【小说化用设定】\n")
	sb.WriteString("A1 世界观：\n" + w.A1Worldview + "\n")
	if w.A6GoalChain != "" {
		sb.WriteString("A6 主角目标链（主角的欲望驱动，写手必须让主角为目标行动，不能被动挨打）：\n" + w.A6GoalChain + "\n")
	}
	if w.A7PowerSys != "" {
		sb.WriteString("A7 能力成长体系（主角能力要有等级感和成长节奏，本章该不该升级、怎么解锁）：\n" + w.A7PowerSys + "\n")
	}
	if w.A8Villain != "" {
		sb.WriteString("A8 反派行动线（反派要主动出击，给主角持续压力，压迫→对抗→打脸）：\n" + w.A8Villain + "\n")
	}
	if w.A9GoldenFinger != "" {
		sb.WriteString("A9 金手指设计（本章金手指处于展示五步的哪个阶段：存在/用法/代价/实战/升级——写到对应阶段的表现）：\n" + w.A9GoldenFinger + "\n")
	}
	if w.A10PayoffRhythm != "" {
		sb.WriteString("A10 爽点循环规划（本章该不该给爽点、给哪类爽点：打脸/收获/装逼/情感——按节奏表来，别乱给也别憋太久）：\n" + w.A10PayoffRhythm + "\n")
	}
	if w.A11MapProgression != "" {
		sb.WriteString("A11 地图阶梯（当前在哪个地图阶段、距离下一个地图还有多远——地图切换=新挑战+新资源+爽点重置）：\n" + w.A11MapProgression + "\n")
	}
	if w.A12FaceSlapCycle != "" {
		sb.WriteString("A12 打脸周期表（本章是否处于「被压迫」期还是「打脸」期——打脸章要写足反差和围观反应）：\n" + w.A12FaceSlapCycle + "\n")
	}
	if w.B4Foreshadows != "" {
		sb.WriteString("B4 伏笔清单（小说层编排）：\n" + w.B4Foreshadows + "\n")
	}
	if w.B5EventPool != "" {
		sb.WriteString("B5 本世界事件谱（这个世界会发生的事——写手的弹药库：日常/冲突/奇遇都从这里取质感，闲笔和伏笔才有方向）：\n" + w.B5EventPool + "\n")
	}
	if w.CNarrative != "" {
		sb.WriteString("C 叙事约束：\n" + w.CNarrative + "\n")
	}
	return sb.String()
}

// ForWorldBrief 精简世界背景（角色人设生成/生活质感推导用：世界观+时代+生活气息）
func (w *Worldbook) ForWorldBrief() string {
	var sb strings.Builder
	if w.A1Worldview != "" {
		sb.WriteString(w.A1Worldview)
	}
	if w.A2Physics != "" {
		sb.WriteString("\n规则：" + w.A2Physics)
	}
	return strings.TrimSpace(sb.String())
}

// WorldRule 物理规则（L1 真值）——GM 软规则裁决用
func (w *Worldbook) WorldRule() string {
	if w.A2Physics == "" {
		return "世界的规则以世界书设定为准；生命有极限、资源有约束、行为有后果。"
	}
	return w.A2Physics
}

// Safety 内容安全边界（所有 Agent 的行为约束）
func (w *Worldbook) Safety() string {
	return w.DSafety
}