package sim

import (
	"fmt"
	"strings"
)

// ---------- 主角更替（通用传承机制） ----------
// 适用场景：任何"视角载体会随剧情更替"的世界——家族世代传承、宗门继位、王朝更迭、轮回转世。
// 设计原则：完全通用，不含任何世界特定设定（家族/修仙/老祖等词一律不出现）。
// 核心：视角主体是"组织/传承线"而非个人；个人只是当前载体；组织记忆/目标/恩怨跨载体延续。

// Succession 执行主角更替：旧主角退场 → 视角切到新主角 → 传承记忆注入 → 编年史记录传承时刻。
//
// 参数：
//   - newHero:    新主角实体名（必须已存在于 entities，通常是剧情里成长起来的下一代）
//   - legacyText: 传承记忆文本（组织历史/当前目标/恩怨/资源——由调用方（LLM 判断或世界设定）提供，
//     会注入新主角的记忆库，让 TA "知道自己继承了谁、要继承什么"，但不知道旧主角的私人想法）
//   - reason:     更替原因（如"寿元耗尽坐化""飞升离去""战死传位"——编年史记录用，纯描述）
//
// 返回值：更替是否成功。失败场景：新主角不存在 / 已是当前主角 / 旧主角不存在。
func (s *Simulator) Succession(newHero, legacyText, reason string) bool {
	if newHero == "" || newHero == s.heroName {
		return false
	}
	st := s.engine.State()
	if st == nil {
		return false
	}
	oldHero := s.heroName
	oldEnt, oldOK := st.Entities[oldHero]
	if !oldOK {
		return false
	}
	newEnt, newOK := st.Entities[newHero]
	if !newOK {
		return false
	}
	// 1. 旧主角标记退场（保留实体与关系，只改状态；世界知道 TA 曾存在）
	oldEnt.Status = "departed"
	oldEnt.Alive = false
	st.Entities[oldHero] = oldEnt
	// 2. 视角切换
	s.SetHeroName(newHero)
	// 3. 新主角状态激活（如果之前是路人/候补，转正为主角视角）
	newEnt.Status = "active"
	newEnt.Alive = true
	st.Entities[newHero] = newEnt
	// 4. 传承记忆注入：写入新主角的记忆库（高重要性，供决策/写手引用）
	//    ——只注入"传承线"的知识（历史/目标/恩怨/资源），不复制旧主角的私人记忆（限知视角不破）
	if legacyText != "" && s.mem != nil {
		s.mem.AddDay(newHero, fmt.Sprintf("传承记忆·%s：%s", reason, legacyText), "event", 1.0, s.day)
	}
	// 5. 编年史记录传承时刻（世界历史的重大节点）
	s.chronicle = append(s.chronicle, ChronicleEntry{
		Day: s.day, Kind: "STATE", Time: now(),
		Content:    fmt.Sprintf("视角更替：%s退场（%s），%s成为新的视角载体。%s", oldHero, reason, newHero, legacyText),
		Visibility: "public", Source: "系统",
		Weight: 0.9, Tags: []string{"传承", "视角更替"},
	})
	return true
}

// SuccessionCandidates 返回"当前可能的下一代主角候选"（活跃实体中，非当前主角、非路人的实体）。
// 供上层（GM/LLM 判断）选择谁接替视角。通用：任何世界都能用。
func (s *Simulator) SuccessionCandidates() []string {
	var names []string
	for name, ent := range s.engine.State().Entities {
		if name == s.heroName || !ent.Alive || ent.Status == "departed" {
			continue
		}
		if tier, _ := ent.Extra["tier"].(string); tier == "walkon" {
			continue // 龙套不接替视角
		}
		names = append(names, name)
	}
	return names
}

// HeroLegacySummary 生成"传承线现状摘要"（供 GM/事件 Agent 判断是否该更替、传给谁）。
// 返回当前视角载体的：身份 / 关键关系 / 当前组织目标（从状态里取），纯通用。
func (s *Simulator) HeroLegacySummary() string {
	st := s.engine.State()
	if st == nil {
		return ""
	}
	var sb strings.Builder
	if ent, ok := st.Entities[s.heroName]; ok {
		sb.WriteString(fmt.Sprintf("当前视角载体：%s（%s）", s.heroName, ent.Job))
		if ident, _ := ent.Extra["identity"].(string); ident != "" {
			sb.WriteString(fmt.Sprintf("，身份：%s", ident))
		}
		sb.WriteString("\n")
	}
	// 组织级信息（如果有）：从世界级事件里找"传承/组织/家族/宗门"相关条目
	for _, ev := range st.WorldLevel.GlobalEvents {
		if strings.Contains(ev, "传承") || strings.Contains(ev, "组织") || strings.Contains(ev, "家族") || strings.Contains(ev, "宗门") {
			sb.WriteString(ev + "\n")
		}
	}
	return strings.TrimSpace(sb.String())
}
