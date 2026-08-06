package sim

// ---------- 玩家介入层（Phase 1：玩家指令注入） ----------
//
// 玩法：玩家（读者/用户）在长跑任意时刻向世界发出一条指令（如"让主角去调查
// 那件怪事""想办法拜入宗门"），下一个事件生成日会把未消费的指令注入事件 Agent
// 的 extraCtx，世界自然回应指令；消费后把当日事件摘要写回指令回执，玩家可查看。
//
// 设计要点：
//   - 不打断长跑：指令排队，事件日自然消费，模拟零阻塞
//   - 不硬控世界：prompt 只抽象描述"来自外界的指令"，是否呼应由事件 Agent 判断
//   - 持久化：worldDir/player_intents.json，重启不丢

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PlayerIntent 一条玩家指令（持久化结构）
type PlayerIntent struct {
	ID          string `json:"id"`           // 指令唯一 ID
	Intent      string `json:"intent"`       // 指令内容（玩家说什么）
	Target      string `json:"target"`       // 目标（角色/地点/组织，可空）
	Status      string `json:"status"`       // pending | consumed
	CreatedAt   string `json:"created_at"`   // 入队时间
	ConsumedDay int    `json:"consumed_day"` // 被消费的模拟日（0=未消费）
	Response    string `json:"response"`     // 消费回执（事件 Agent 的回应摘要，消费后写入）
}

func (s *Simulator) playerFile() string { return filepath.Join(s.worldDir, "player_intents.json") }

// loadPlayerIntents 启动时加载历史指令（重启不丢）
func (s *Simulator) loadPlayerIntents() {
	data, err := os.ReadFile(s.playerFile())
	if err != nil {
		return // 无文件=无历史，正常
	}
	var list []PlayerIntent
	if json.Unmarshal(data, &list) != nil {
		return
	}
	s.playerMu.Lock()
	s.playerIntents = list
	s.playerMu.Unlock()
}

// savePlayerIntents 持久化指令列表（原子写）
func (s *Simulator) savePlayerIntents() {
	s.playerMu.Lock()
	list := append([]PlayerIntent(nil), s.playerIntents...)
	s.playerMu.Unlock()
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	tmp := s.playerFile() + ".tmp"
	if os.WriteFile(tmp, b, 0600) != nil {
		return
	}
	_ = os.Rename(tmp, s.playerFile())
}

// QueuePlayerIntent 玩家发指令入队（任意时刻调用，零阻塞）
func (s *Simulator) QueuePlayerIntent(intent, target string) (string, error) {
	intent = strings.TrimSpace(intent)
	if intent == "" {
		return "", fmt.Errorf("指令内容为空")
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	id := "p" + now + "-" + randSuffix(4)
	s.playerMu.Lock()
	s.playerIntents = append(s.playerIntents, PlayerIntent{
		ID: id, Intent: intent, Target: strings.TrimSpace(target),
		Status: "pending", CreatedAt: now,
	})
	s.playerMu.Unlock()
	s.savePlayerIntents()
	return id, nil
}

// PlayerIntents 查询全部指令（副本，供状态面板）
func (s *Simulator) PlayerIntents() []PlayerIntent {
	s.playerMu.Lock()
	defer s.playerMu.Unlock()
	return append([]PlayerIntent(nil), s.playerIntents...)
}

// pendingPlayerIntents 取走所有未消费指令（RunDay 事件生成前调用，消费后由回执更新）
func (s *Simulator) pendingPlayerIntents() []PlayerIntent {
	s.playerMu.Lock()
	defer s.playerMu.Unlock()
	var out []PlayerIntent
	for i := range s.playerIntents {
		if s.playerIntents[i].Status == "pending" {
			out = append(out, s.playerIntents[i])
		}
	}
	return out
}

// settlePlayerIntents 事件生成成功后写回执（指令→consumed + 事件摘要）
func (s *Simulator) settlePlayerIntents(ids []string, day int, summary string) {
	if len(ids) == 0 {
		return
	}
	s.playerMu.Lock()
	for i := range s.playerIntents {
		for _, id := range ids {
			if s.playerIntents[i].ID == id && s.playerIntents[i].Status == "pending" {
				s.playerIntents[i].Status = "consumed"
				s.playerIntents[i].ConsumedDay = day
				s.playerIntents[i].Response = summary
			}
		}
	}
	s.playerMu.Unlock()
	s.savePlayerIntents()
}

// playerIntentPrompt 组装注入事件 Agent 的指令段（抽象描述，零污染）
func playerIntentPrompt(ints []PlayerIntent) string {
	if len(ints) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("【来自外界的指令】一位观察者向这个世界发出了指令，世界应当自然回应它（呼应、部分呼应或延后均可，由剧情逻辑决定，不必生硬照做；若指令与当前局势冲突，可让世界以自己的方式消化）：\n")
	for i, it := range ints {
		sb.WriteString("- 指令")
		if i > 0 {
			sb.WriteString("（追加）")
		}
		sb.WriteString("：")
		sb.WriteString(it.Intent)
		if it.Target != "" {
			sb.WriteString("（涉及目标：")
			sb.WriteString(it.Target)
			sb.WriteString("）")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("回应方式不限：可以是人物行动、局势变化、机缘/阻碍，让指令在剧情里留下痕迹。\n")
	return sb.String()
}

// randSuffix 生成 n 位随机字符后缀（指令 ID 用）
func randSuffix(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

// playerIntentIDs 提取指令 ID 列表（结算回执用）
func playerIntentIDs(ints []PlayerIntent) []string {
	ids := make([]string, 0, len(ints))
	for _, it := range ints {
		ids = append(ids, it.ID)
	}
	return ids
}

// playerEventSummary 把当日事件压缩成玩家回执摘要（标题+简述，最多 3 条）
func playerEventSummary(evs []EventCard) string {
	if len(evs) == 0 {
		return "当日事件已生成（无摘要）"
	}
	n := len(evs)
	if n > 3 {
		n = 3
	}
	var sb strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteString("；")
		}
		sb.WriteString("Day")
		sb.WriteString(fmt.Sprintf("%d", evs[i].Day))
		sb.WriteString("「")
		sb.WriteString(evs[i].Title)
		sb.WriteString("」")
		if evs[i].Location != "" {
			sb.WriteString("（")
			sb.WriteString(evs[i].Location)
			sb.WriteString("）")
		}
	}
	return sb.String()
}
