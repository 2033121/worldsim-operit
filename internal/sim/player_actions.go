package sim

// ---------- Phase 4：玩家行动真实生效（行动点系统） ----------
//
// 玩家不再只是"发指令等回应"——行动直接写入世界状态（走 engine 原子提交，
// 与 LLM 世界推进同一条路径，受硬规则校验，可回滚）：
//   - 休整：健康 +8（有上限，满血不可重复刷）
//   - 精进：能力值（首个 ≤20 的数值型能力）+1
//   - 会晤：与目标角色关系 +0.05
//
// 行动点：上限 5，每模拟日恢复 1 点，每次行动消耗 1 点（防无限刷属性）。
// 零污染：不绑定任何具体属性名，只看数值范围/通用字段。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"worldsim/internal/engine"
)

const (
	maxPlayerAP    = 5
	restHealthGain = 8.0
	socialRelGain  = 0.05
)

// PlayerAP 行动点状态（持久化）
type PlayerAP struct {
	Points         int `json:"points"`
	LastRecoverDay int `json:"last_recover_day"`
}

// ActionResult 一次玩家行动的结果
type ActionResult struct {
	Kind     string  `json:"kind"`
	Message  string  `json:"message"`
	APLeft   int     `json:"ap_left"`
	Changed  string  `json:"changed"`  // 变更的字段路径
	NewValue float64 `json:"new_value,omitempty"`
}

func (s *Simulator) apFile() string { return filepath.Join(s.worldDir, "player_ap.json") }

func (s *Simulator) loadAP() *PlayerAP {
	ap := &PlayerAP{Points: maxPlayerAP}
	data, err := os.ReadFile(s.apFile())
	if err != nil {
		return ap
	}
	var p PlayerAP
	if json.Unmarshal(data, &p) == nil {
		ap = &p
	}
	return ap
}

func (s *Simulator) saveAP(ap *PlayerAP) {
	b, err := json.MarshalIndent(ap, "", "  ")
	if err != nil {
		return
	}
	tmp := s.apFile() + ".tmp"
	if os.WriteFile(tmp, b, 0600) != nil {
		return
	}
	_ = os.Rename(tmp, s.apFile())
}

// recoverAP 每模拟日恢复 1 行动点（上限 5），RunDay 每天调用
func (s *Simulator) recoverAP(day int) {
	ap := s.loadAP()
	if day <= ap.LastRecoverDay {
		return
	}
	if ap.Points < maxPlayerAP {
		ap.Points++
	}
	ap.LastRecoverDay = day
	s.saveAP(ap)
}

// PlayerAPBalance 当前行动点
func (s *Simulator) PlayerAPBalance() int {
	return s.loadAP().Points
}

// PlayerAction 执行一次玩家行动（消耗行动点，直接写入世界状态）
func (s *Simulator) PlayerAction(ctx context.Context, kind, target string) (*ActionResult, error) {
	ap := s.loadAP()
	if ap.Points <= 0 {
		return nil, fmt.Errorf("行动点不足（每日恢复1点，上限%d），明天再来", maxPlayerAP)
	}
	ents := s.engine.State().Entities
	heroEnt, ok := ents[s.heroName]
	if !ok || s.heroName == "" {
		return nil, fmt.Errorf("世界尚未初始化")
	}

	var changes []engine.Change
	msg := ""
	changed := ""
	newVal := 0.0

	switch kind {
	case "rest":
		cur := heroEnt.Health
		if cur >= 100-restHealthGain {
			return nil, fmt.Errorf("当前状态良好（健康 %.0f），无需休整", cur)
		}
		gain := restHealthGain
		if cur+gain > 100 {
			gain = 100 - cur
		}
		changes = append(changes, engine.Change{
			Path: "entities." + s.heroName + ".health", Op: "add", Value: gain,
		})
		msg = fmt.Sprintf("休整调养，健康 +%.0f", gain)
		changed = "entities." + s.heroName + ".health"
		newVal = cur + gain

	case "cultivate":
		attr, curVal, ok2 := firstNumericStat(heroEnt.Stats)
		if !ok2 {
			return nil, fmt.Errorf("当前没有可精进的能力（需要数值型能力值）")
		}
		changes = append(changes, engine.Change{
			Path: "entities." + s.heroName + ".stats." + attr, Op: "set", Value: curVal + 1,
		})
		msg = fmt.Sprintf("投入时间精进「%s」：%v → %v", attr, curVal, curVal+1)
		changed = "entities." + s.heroName + ".stats." + attr
		newVal = curVal + 1

	case "social":
		target = strings.TrimSpace(target)
		if target == "" || target == s.heroName {
			return nil, fmt.Errorf("请指定要会晤的角色")
		}
		if _, ok := ents[target]; !ok {
			return nil, fmt.Errorf("角色「%s」不存在", target)
		}
		changes = append(changes, engine.Change{
			Path: "entities." + s.heroName + ".relationship." + target, Op: "add", Value: socialRelGain,
		})
		msg = fmt.Sprintf("与「%s」深谈一次，关系 +%.2f", target, socialRelGain)
		changed = "entities." + s.heroName + ".relationship." + target
		newVal = heroEnt.Relationship[target] + socialRelGain

	default:
		return nil, fmt.Errorf("未知行动类型：%s（rest | cultivate | social）", kind)
	}

	// 原子提交（与 LLM 世界推进同路径：硬规则校验 + event_log 记录）
	prop := &engine.Proposal{
		CommandID:    s.nextCmd("player"),
		ActorID:      "player",
		BaseRevision: s.engine.State().Revision,
		Type:         "state_change",
		Changes:      changes,
		Reason:       "玩家行动：" + msg,
	}
	if err := s.engine.Submit(ctx, prop); err != nil {
		return nil, fmt.Errorf("世界提交失败：%v", err)
	}

	// 消耗行动点
	ap.Points--
	s.saveAP(ap)

	return &ActionResult{Kind: kind, Message: msg, APLeft: ap.Points, Changed: changed, NewValue: newVal}, nil
}

// firstNumericStat 找最高 ≤20 的数值型能力（技能/等级通常是数值较高的；负面状态
// 如伤势通常数值低，取最大值避免"精进伤势"的语义错误。零污染：只看数值不看属性名）
func firstNumericStat(stats map[string]any) (string, float64, bool) {
	if stats == nil {
		return "", 0, false
	}
	bestKey := ""
	bestVal := 0.0
	for k, v := range stats {
		if f, ok := toFloat(v); ok && f > 0 && f <= 20 && f > bestVal {
			bestKey, bestVal = k, f
		}
	}
	if bestKey == "" {
		return "", 0, false
	}
	return bestKey, bestVal, true
}