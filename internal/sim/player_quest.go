package sim

// ---------- Phase 3：任务系统（段落里程碑 → 玩家任务卡） ----------
//
// 当前剧情段落的里程碑直接映射为"任务"：玩家可查看/接取（接取=发指令让世界推进），
// 任务完成状态由段落进度（arcDone）驱动，零 LLM 零成本、零污染。
// 段落收尾（arcDone 达里程碑数）后整组任务标记完成，新段落自动带来新任务。

import "strings"

// Quest 一张任务卡
type Quest struct {
	ID      string `json:"id"`
	Title   string `json:"title"`   // 里程碑原文（世界数据）
	Status  string `json:"status"`  // done | active | upcoming
	ArcName string `json:"arc_name"`
	Index   int    `json:"index"`   // 在里程碑中的序号（0起）
	Total   int    `json:"total"`   // 本段落里程碑总数
}

// Quests 生成当前段落的任务卡列表（arcDone 驱动完成状态）
func (s *Simulator) Quests() []Quest {
	var qs []Quest
	for i := len(s.arcBook) - 1; i >= 0; i-- {
		a := s.arcBook[i]
		if a.Status != "open" {
			continue
		}
		total := len(a.Milestones)
		if total == 0 {
			return qs
		}
		for idx, m := range a.Milestones {
			st := "upcoming"
			if idx < s.arcDone {
				st = "done" // 已推进的里程碑
			} else if idx == s.arcDone {
				st = "active" // 当前目标
			}
			qs = append(qs, Quest{
				ID:      "q-" + a.ArcName + "-" + itoa(idx+1),
				Title:   strings.TrimSpace(m),
				Status:  st,
				ArcName: a.ArcName,
				Index:   idx,
				Total:   total,
			})
		}
		return qs
	}
	return qs
}

// QuestPrompt 把当前任务目标拼进行动/指令上下文（抽象驱动，不进 LLM 也可用）
func (s *Simulator) QuestPrompt() string {
	qs := s.Quests()
	var act []string
	for _, q := range qs {
		if q.Status == "active" {
			act = append(act, q.Title)
		}
	}
	if len(act) == 0 {
		return ""
	}
	return "当前任务目标：" + strings.Join(act, "；")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}