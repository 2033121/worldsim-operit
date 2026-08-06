package sim

// ---------- Phase 3：战斗/冲突回合制（玩家可玩） ----------
//
// 纯规则模拟，零 LLM 零成本：双方用世界数据（能力/健康/名望）做检定，骰子驱动回合。
// 设计要点：
//   - 预览模式：结果只展示给玩家（胜负概率/回合日志），不写入世界状态——
//     真正剧情仍由事件 Agent 决定，避免规则结果与剧情矛盾
//   - 零污染：不引用任何具体世界体系（不写"法术/斗气/灵根"），能力值
//     一律用"世界数据里的数值型能力"抽象驱动
//   - 确定性：同种子可复现（测试用），默认随机

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
)

// CombatRound 一回合记录
type CombatRound struct {
	Round    int     `json:"round"`
	Attacker string  `json:"attacker"`
	Action   string  `json:"action"` // 攻击 | 躲避
	Hit      bool    `json:"hit"`
	Damage   float64 `json:"damage"`
	HPLeft   float64 `json:"hp_left"` // 防守方剩余健康
}

// CombatResult 一次战斗模拟结果
type CombatResult struct {
	Hero       string        `json:"hero"`
	Enemy      string        `json:"enemy"`
	HeroHP     float64       `json:"hero_hp"`
	EnemyHP    float64       `json:"enemy_hp"`
	Winner     string        `json:"winner"` // hero | enemy | draw
	Rounds     []CombatRound `json:"rounds"`
	WinRate    float64       `json:"win_rate"`     // 重复模拟的胜率预估（0~100）
	RoundsUsed int           `json:"rounds_used"`  // 胜率模拟用到的回合数
	PowerGap   string        `json:"power_gap"`    // 实力差距描述（抽象档位）
}

// SimulateCombat 模拟一场战斗（预览，不写世界）。enemyName 为对手实体名。
func (s *Simulator) SimulateCombat(enemyName string) *CombatResult {
	ents := s.engine.State().Entities
	heroEnt, okH := ents[s.heroName]
	enemyEnt, okE := ents[enemyName]
	res := &CombatResult{Hero: s.heroName, Enemy: enemyName, Winner: "draw"}
	if !okH || !okE || s.heroName == "" || enemyName == "" {
		res.Winner = "invalid"
		return res
	}

	heroAtk, heroDef := combatStats(heroEnt.Stats, heroEnt.Health)
	enemyAtk, enemyDef := combatStats(enemyEnt.Stats, enemyEnt.Health)

	// 实力差距（抽象档位：由攻防比决定，不绑定具体体系）
	gap := enemyAtk + enemyDef - heroAtk - heroDef
	switch {
	case gap >= 2.5:
		res.PowerGap = "对手明显强于你"
	case gap <= -2.5:
		res.PowerGap = "你明显强于对手"
	case gap >= 1:
		res.PowerGap = "对手略占上风"
	case gap <= -1:
		res.PowerGap = "你略占上风"
	default:
		res.PowerGap = "势均力敌"
	}
// 单局模拟（随机骰子）
	hpH, hpE := heroEnt.Health, enemyEnt.Health
	if hpH <= 0 {
		hpH = 50
	}
	if hpE <= 0 {
		hpE = 50
	}
	initH, initE := hpH, hpE
	res.Rounds = simulateOneFight(heroAtk, heroDef, enemyAtk, enemyDef, &hpH, &hpE, s.heroName, enemyName)
	res.HeroHP = hpH
	res.EnemyHP = hpE
	res.Winner = fightWinner(hpH, hpE, initH, initE)

	// 胜率预估：重复 40 局统计（与单局同判定）
	wins := 0
	for i := 0; i < 40; i++ {
		ah, ae := heroEnt.Health, enemyEnt.Health
		if ah <= 0 {
			ah = 50
		}
		if ae <= 0 {
			ae = 50
		}
		ih, ie := ah, ae
		simulateOneFight(heroAtk, heroDef, enemyAtk, enemyDef, &ah, &ae, s.heroName, enemyName)
		if fightWinner(ah, ae, ih, ie) == "hero" {
			wins++
		}
	}
	res.WinRate = float64(wins) / 40 * 100
	return res
}

// fightWinner 统一胜负判定：一方归零→败；限时结束双方存活→比剩余健康比例（公平，
// 避免血厚者因绝对血量天然占优）；比例持平=平局
func fightWinner(hpH, hpE, initH, initE float64) string {
	switch {
	case hpH <= 0 && hpE <= 0:
		return "draw"
	case hpH <= 0:
		return "enemy"
	case hpE <= 0:
		return "hero"
	}
	// 双方都存活：比剩余比例（剩余/初始）
	ratioH, ratioE := 1.0, 1.0
	if initH > 0 {
		ratioH = hpH / initH
	}
	if initE > 0 {
		ratioE = hpE / initE
	}
	switch {
	case ratioH > ratioE:
		return "hero"
	case ratioE > ratioH:
		return "enemy"
	default:
		return "draw"
	}
}

// combatStats 从世界数据提取攻/防（抽象：数值型能力 + 健康，不绑定体系名）
// 只计 ≤20 的能力型数值（个人修为/技能等）；大数值（财富/资源/势力等）不参与战力，
// 避免"家财万贯的商人"在单挑中被算作碾压战力。零污染：不看属性名，只看数值范围。
func combatStats(stats map[string]any, health float64) (atk, def float64) {
	atk = 1.0 // 基础值（任何存在都有基本行动力）
	def = 0.5
	if stats != nil {
		var nums []float64
		for _, v := range stats {
			if f, ok := toFloat(v); ok && f > 0 && f <= 20 {
				nums = append(nums, f)
			}
		}
		if len(nums) > 0 {
			sort.Float64s(nums)
			atk += nums[len(nums)-1] / 5 // 最高能力影响攻击
			for _, n := range nums {
				def += n / 25 // 综合能力提升防御
			}
		}
	}
	def += health / 50
	return atk, def
}

// simulateOneFight 打一场：最多 6 回合，先手随机，命中率 75%+攻防差修正
func simulateOneFight(heroAtk, heroDef, enemyAtk, enemyDef float64, hpH, hpE *float64, heroName, enemyName string) []CombatRound {
	var rounds []CombatRound
	first := rand.Intn(2) == 0 // 0=hero先手 1=enemy先手
	for r := 1; r <= 6; r++ {
		if *hpH <= 0 || *hpE <= 0 {
			break
		}
		if (r%2 == 1) == (first == false) {
			// 主角回合
			hit := rand.Float64() < 0.75+heroAtk*0.02-enemyDef*0.01
			dmg := 0.0
			if hit {
				dmg = maxF(1, heroAtk*2-enemyDef)
			}
			*hpE -= dmg
			rounds = append(rounds, CombatRound{Round: r, Attacker: heroName, Action: actionWord(hit), Hit: hit, Damage: dmg, HPLeft: *hpE})
		} else {
			hit := rand.Float64() < 0.75+enemyAtk*0.02-heroDef*0.01
			dmg := 0.0
			if hit {
				dmg = maxF(1, enemyAtk*2-heroDef)
			}
			*hpH -= dmg
			rounds = append(rounds, CombatRound{Round: r, Attacker: enemyName, Action: actionWord(hit), Hit: hit, Damage: dmg, HPLeft: *hpH})
		}
	}
	if len(rounds) == 0 {
		rounds = []CombatRound{{Round: 1, Attacker: heroName, Action: "对峙", Hit: false, Damage: 0, HPLeft: *hpE}}
	}
	return rounds
}

// actionWord 回合动作文案（抽象：命中/未中，不写具体招式）
func actionWord(hit bool) string {
	if hit {
		return "出手命中"
	}
	return "出手落空"
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		var f float64
		if _, err := fmt.Sscanf(t, "%f", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

// combatTargets 可选战斗对象：与主角有交集（关系≠0）且存活的实体
func (s *Simulator) combatTargets() []string {
	ents := s.engine.State().Entities
	heroEnt, ok := ents[s.heroName]
	if !ok {
		return nil
	}
	var out []string
	for name, e := range ents {
		if name == s.heroName || !e.Alive || e.Status == "departed" {
			continue
		}
		if v, ok := heroEnt.Relationship[name]; ok && v < 0.3 {
			out = append(out, name) // 关系疏远/敌对者才值得动手
		}
	}
	if len(out) == 0 { // 兜底：任意存活实体
		for name, e := range ents {
			if name != s.heroName && e.Alive && e.Status != "departed" {
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// FormatCombatSummary 战斗结果摘要（前端回执用）
func FormatCombatSummary(r *CombatResult) string {
	if r == nil {
		return ""
	}
	w := map[string]string{"hero": "你胜", "enemy": "对方胜", "draw": "两败俱伤", "invalid": "无法战斗"}[r.Winner]
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s vs %s：%s（胜率预估 %.0f%%）", r.Hero, r.Enemy, w, r.WinRate))
	if len(r.Rounds) > 0 {
		last := r.Rounds[len(r.Rounds)-1]
		sb.WriteString(fmt.Sprintf("，%d 回合后 %s 剩余健康 %.0f", len(r.Rounds), last.Attacker, last.HPLeft))
	}
	return sb.String()
}