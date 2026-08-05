package sim

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------- 岔口决策队列（AI 主编：用户可干预，但零阻塞） ----------
// 设计原则：
//   - 世界/写作遇到剧情岔口（事件带 options）时自动入队，AI 立即代决（不卡流程）
//   - 用户有空时随时查看/改选；改选后写手按用户方向写，AI 代决的按 AI 方向写
//   - 所有岔口留痕：情境/选项/AI 推荐理由/最终走向，可回溯

type DecisionOption struct {
	ID     string `json:"id"`     // A / B / C
	Desc   string `json:"desc"`   // 选项描述
	Reason string `json:"reason"` // 该方向会导致什么（可选）
}

type DecisionEntry struct {
	ID         string           `json:"id"`          // dec-{day}-{n}
	Day        int              `json:"day"`         // 岔口发生日（模拟日）
	Type       string           `json:"type"`        // 事件类型（conflict/wonder/...）
	Title      string           `json:"title"`       // 岔口标题
	Context    string           `json:"context"`     // 情境简述（frame）
	Options    []DecisionOption `json:"options"`     // 2-4 个方向
	AIChoice   string           `json:"ai_choice"`   // AI 代决方向（主角行动）
	AIReason   string           `json:"ai_reason"`   // AI 选择理由
	Status     string           `json:"status"`      // pending | decided
	UserChoice string           `json:"user_choice"` // 用户选的方向（""=未干预）
	Note       string           `json:"note,omitempty"`
	CreatedAt  string           `json:"created_at"`
	ResolvedAt string           `json:"resolved_at,omitempty"`
}

// decisions 内存队列 + decisions.jsonl 落盘（重启不丢）
type DecisionStore struct {
	entries []DecisionEntry
	path    string
}

func NewDecisionStore(worldDir string) *DecisionStore {
	ds := &DecisionStore{path: filepath.Join(worldDir, "decisions.jsonl")}
	if data, err := os.ReadFile(ds.path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var e DecisionEntry
			if json.Unmarshal([]byte(line), &e) == nil {
				ds.entries = append(ds.entries, e)
			}
		}
	}
	return ds
}

func (ds *DecisionStore) save() {
	var sb strings.Builder
	for _, e := range ds.entries {
		if b, err := json.Marshal(e); err == nil {
			sb.Write(b)
			sb.WriteString("\n")
		}
	}
	_ = os.WriteFile(ds.path, []byte(sb.String()), 0644)
}

// Capture 入队一个岔口（AI 代决方向由调用方传入）
func (ds *DecisionStore) Capture(day int, typ, title, context string, opts []DecisionOption, aiChoice, aiReason string) DecisionEntry {
	e := DecisionEntry{
		ID:        fmt.Sprintf("dec-%d-%d", day, len(ds.entries)+1),
		Day:       day,
		Type:      typ,
		Title:     title,
		Context:   context,
		Options:   opts,
		AIChoice:  aiChoice,
		AIReason:  aiReason,
		Status:    "pending",
		CreatedAt: time.Now().Format("2006-01-02 15:04"),
	}
	ds.entries = append(ds.entries, e)
	ds.save()
	return e
}

// Resolve 用户改选（覆盖 AI 代决）
func (ds *DecisionStore) Resolve(id, choice string) bool {
	for i := range ds.entries {
		if ds.entries[i].ID == id {
			ds.entries[i].UserChoice = choice
			ds.entries[i].Status = "decided"
			ds.entries[i].ResolvedAt = time.Now().Format("2006-01-02 15:04")
			ds.save()
			return true
		}
	}
	return false
}

// All 全部岔口（最新在前）
func (ds *DecisionStore) All() []DecisionEntry {
	out := make([]DecisionEntry, len(ds.entries))
	copy(out, ds.entries)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ForDays 某段时间内的岔口（写手生成章节时注入"方向约束"）
func (ds *DecisionStore) ForDays(dayFrom, dayTo int) []DecisionEntry {
	var out []DecisionEntry
	for _, e := range ds.entries {
		if e.Day >= dayFrom && e.Day <= dayTo {
			out = append(out, e)
		}
	}
	return out
}

// EffectiveChoice 写手实际采用的方向：用户改过用用户的，否则用 AI 代决
func (e DecisionEntry) EffectiveChoice() string {
	if e.Status == "decided" && e.UserChoice != "" {
		return e.UserChoice
	}
	if e.AIChoice != "" {
		return e.AIChoice
	}
	return ""
}

// Direction 渲染成写手可读的方向说明
func (e DecisionEntry) Direction() string {
	who := "AI 代决"
	if e.Status == "decided" && e.UserChoice != "" {
		who = "用户选择"
	}
	eff := e.EffectiveChoice()
	optTxt := ""
	for _, o := range e.Options {
		mark := ""
		if strings.Contains(eff, o.Desc) || (o.Desc != "" && (eff == o.ID || strings.Contains(eff, o.ID))) {
			mark = " ←采用"
		}
		optTxt += fmt.Sprintf("  · %s：%s%s\n", o.ID, o.Desc, mark)
	}
	return fmt.Sprintf("【%s · %s】%s\n情境：%s\n选项：\n%s采用方向（%s）：%s",
		e.Title, who, e.Type, e.Context, optTxt, who, eff)
}

// FormatDirections 把一段时间内岔口渲染成写手 prompt 块
func FormatDirections(entries []DecisionEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("本章涉及的剧情岔口与已定方向（写手必须按「采用方向」推进，不得另起炉灶）：\n")
	for _, e := range entries {
		sb.WriteString(e.Direction())
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

// Simulator 侧：捕获事件岔口入队（事件带 options 时）
func (s *Simulator) captureDecisions(aiAction, aiReason string) {
	if s.decisions == nil || len(s.events) == 0 {
		return
	}
	for _, ev := range s.events {
		if len(ev.Options) == 0 {
			continue
		}
		opts := make([]DecisionOption, 0, len(ev.Options))
		for i, o := range ev.Options {
			opts = append(opts, DecisionOption{ID: fmt.Sprintf("%c", 'A'+i), Desc: o})
		}
		// 同一事件不重复入队
		dup := false
		for _, e := range s.decisions.All() {
			if e.Title == ev.Title && e.Day == s.day {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		s.decisions.Capture(s.day, ev.Type, ev.Title, ev.Frame, opts, aiAction, aiReason)
	}
}

// DecisionsFor 某段时间内的岔口（写手注入用）
func (s *Simulator) DecisionsFor(dayFrom, dayTo int) []DecisionEntry {
	if s.decisions == nil {
		return nil
	}
	return s.decisions.ForDays(dayFrom, dayTo)
}

// AllDecisions 全部岔口（接口展示用）
func (s *Simulator) AllDecisions() []DecisionEntry {
	if s.decisions == nil {
		return nil
	}
	return s.decisions.All()
}

// ResolveDecision 用户改选（覆盖 AI 代决）
func (s *Simulator) ResolveDecision(id, choice string) bool {
	if s.decisions == nil {
		return false
	}
	return s.decisions.Resolve(id, choice)
}
