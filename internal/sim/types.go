// Package sim 实现 WorldSim 的模拟调度层（设计文档 §5/§7/§9）：
//   - 宏观循环：世界推进 → 事件生成 → 感知分发 → 主角决策 → GM裁决 → 记录 → 回灌
//   - Agent 接口：世界/主角/事件/NPC/记录，每个 agent 可接 LLM 或 dry-run 模板
//   - Observation Packet 感知分发（§4.4）
//   - 编年史 [FACT]/[SAID]/[STATE] 三分 + visibility（§9.10）
package sim

import (
	"context"
	"time"

	"worldsim/internal/engine"
)

// ---------- 事件卡（§3.2/§7） ----------

type EventCard struct {
	ID         string   `json:"id"`          // ev-{day}-{n}
	Day        int      `json:"day"`
	Type       string   `json:"type"`        // daily | conflict | wonder | romance | opportunity | crisis
	Title      string   `json:"title"`
	Location   string   `json:"location"`
	Severity   float64  `json:"severity"`    // 0~1，决定是否暂停（≥0.75）
	NPCs       []string `json:"npcs,omitempty"`
	Frame      string   `json:"frame"`       // 遭遇框架（含时序占位，不含NPC具体言行）
	FirstActor string   `json:"first_actor"` // 谁先行动（时序占位）
	Options    []string `json:"options,omitempty"`
	NewChars   []NewCharacter `json:"new_characters,omitempty"` // 本事件引入的新角色（自然产生）
	RelEffect  string   `json:"rel_effect,omitempty"` // 感情/关系事件说明（如"与苏婉共度危机，关系升温"）
	Foreshadow         string        `json:"foreshadow,omitempty"`          // 本事件埋/推进的伏笔名（伏笔账本）
	ResolveForeshadow  string        `json:"resolve_foreshadow,omitempty"`   // 本事件正式揭晓/结算的伏笔名（与未回收清单同名，收坑）
	NextEvents         []EventCard   `json:"next_events,omitempty"`          // 遭遇链：本事件触发的后续事件（1-3天后出现）
}

// ---------- Observation Packet（§4.4） ----------

type Observation struct {
	ID            string  `json:"id"`
	Source        string  `json:"source"`          // 亲眼|听说|媒体|短信|推断
	Content       string  `json:"content"`
	VisibleTo     string  `json:"visible_to"`      // 可见对象范围
	LocationScope string  `json:"location_scope"`  // 地点与传播范围
	Credibility   float64 `json:"credibility"`     // 可信度
	ArrivalDay    int     `json:"arrival_day"`     // 到达时间
	WritableToMem bool    `json:"writable_to_memory"`
}

// ObservationPacket 是分发给单个角色的感知包
type ObservationPacket struct {
	Recipient   string        `json:"recipient"`
	Day         int           `json:"day"`
	Observations []Observation `json:"observations"`
}

// ---------- 编年史（§9.10） ----------

// ChronicleEntry 编年史条目（写手素材源）
// Weight=戏剧权重（0~1）：写手按此选材——≥0.55进场景，0.3~0.55进背景压缩，<0.3 一句话带过
// Tags=角色/地点/伏笔/冲突标记（供"本章焦点"统计主线）
type ChronicleEntry struct {
	Day         int      `json:"day"`
	Kind        string   `json:"kind"` // FACT | SAID | STATE
	Time        string   `json:"time"`
	Content     string   `json:"content"`
	Visibility  string   `json:"visibility"` // public | restricted
	Source      string   `json:"source,omitempty"`
	Credibility float64  `json:"credibility,omitempty"`
	Weight      float64  `json:"weight,omitempty"` // 戏剧权重（旧数据无此字段=0，加载时按规则补默认）
	Tags        []string `json:"tags,omitempty"`   // 角色/地点/伏笔/冲突标记
}

// ---------- Agent 接口 ----------

type Agent interface {
	Name() string
	// Act 返回该 agent 本轮的状态变更提案（可为空切片 = 无提案）
	Act(ctx context.Context, s *StepContext) ([]engine.Proposal, error)
}

// StepContext 是单轮模拟中传给 agent 的上下文
type StepContext struct {
	Day           int
	State         *engine.WorldState
	Events        []EventCard
	Observation   ObservationPacket // 该 agent 的感知包
	Rules         engine.Rules
	GM            *engine.StateEngine
	ProtagonistPlan *ProtagonistPlan // 主角默认策略（Skip用）
}

// ProtagonistPlan 主角默认策略（§5.2，Skip 快进用）
type ProtagonistPlan struct {
	Horizon       int      `json:"horizon"`
	DefaultRoutine []string `json:"default_routine"`
	Conditional   string   `json:"conditional"`
	InterruptIf   []string `json:"interrupt_if"`
}

// DriftNote 铺垫条目：平淡期的暗流/变化（伏笔滋长/环境渐变/人物微动/能力暗育/关系漂移）
type DriftNote struct {
	Type    string `json:"type"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Days    int    `json:"days"` // 这段铺垫覆盖的天数（1~5）
}

// DynamicAgent 动态创建的"负责人 Agent"（公司里的新岗位，随剧情需要招聘）
// 例子：GM 规划出某条剧情线→ 自动创建"该线负责人"；重要角色登场 → 创建"角色专属Agent"
type DynamicAgent struct {
	ID         string `json:"id"`
	Name       string `json:"name"` // 岗位名，如"某剧情线的负责人"
	Type       string `json:"type"` // faction_line | character_line | plot_line | world_line
	Focus      string `json:"focus"` // 负责的剧情对象/角色（如"对主角的持续行动"）
	State      string `json:"state"` // 当前状态/下一步动作（持续更新，注入其他Agent）
	CreatedDay int    `json:"created_day"`
	UpdatedDay int    `json:"updated_day"`
}

// ---------- 模拟结果 ----------

type DayResult struct {
	Day       int               `json:"day"`
	Events    []EventCard       `json:"events"`
	Chronicle []ChronicleEntry  `json:"chronicle"`
	Proposals []engine.Proposal `json:"proposals"`
	Dialogue  []DialogueTurn    `json:"dialogue,omitempty"` // NPC对话（§5.1）
	Paused    bool              `json:"paused"` // 是否触发暂停（抉择点）
	PauseMsg  string            `json:"pause_msg,omitempty"`
	Mode      string            `json:"mode"` // scene | summary | skip
	Skipped   bool              `json:"skipped,omitempty"` // 平淡日快进（事件驱动：未展开模拟）
}

func now() string { return time.Now().Format("15:04") }