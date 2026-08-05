package sim

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"worldsim/internal/engine"
)

// ---------- 开放式关系系统（P0-1：无预设单/多女主，感情自由演化） ----------
// 设计原则：
//   · 不预设谁成为女主——新角色由世界自然产生，靠互动积累好感，演化为恋人/朋友/路人/对手
//   · 感情可升温（在一起）也可破裂（分开/淡出），状态机双向可走
//   · 双向视角：主角对TA的好感 + TA对主角的好感各自独立演化

// 关系状态（正向推进 + 破裂路径）
const (
	RelStranger     = "stranger"     // 陌生人
	RelAcquaintance = "acquaintance" // 认识
	RelFriend       = "friend"       // 朋友
	RelCloseFriend  = "close_friend" // 挚友
	RelCrush        = "crush"        // 心动
	RelDating       = "dating"       // 交往
	RelLover        = "lover"        // 恋人
	RelPartner      = "partner"      // 伴侣/道侣
	RelConflict     = "conflict"     // 冲突
	RelEstranged    = "estranged"    // 疏远
	RelEx           = "ex"           // 已分开/决裂
)

// RelStatusLabel 由好感+信任映射关系状态（纯函数，无预设上限）
func RelStatusLabel(affinity, trust float64) string {
	switch {
	case affinity >= 0.92 && trust >= 0.75:
		return RelPartner
	case affinity >= 0.83:
		return RelLover
	case affinity >= 0.72 && trust >= 0.5:
		return RelDating
	case affinity >= 0.62:
		return RelCrush
	case affinity >= 0.48:
		return RelCloseFriend
	case affinity >= 0.32:
		return RelFriend
	case affinity >= 0.12:
		return RelAcquaintance
	default:
		return RelStranger
	}
}

// RelationshipView 某角色视角下的一段关系（用于查询/展示）
type RelationshipView struct {
	Other     string   `json:"other"`
	Affinity  float64  `json:"affinity"`  // 好感 -1~1（负=厌恶）
	Trust     float64  `json:"trust"`     // 信任 0~1
	Status    string   `json:"status"`    // 关系状态
	SinceDay  int      `json:"since_day"` // 初遇日
	Events    []string `json:"events"`    // 关系大事记
	Role      string   `json:"role"`      // 对方在剧情中的角色定位
}

// ---------- 关系读写（走 State Engine 提案，引擎是唯一事实源） ----------

// ReadRelation 读主角对 other 的关系视角（从当前状态）
func (s *Simulator) ReadRelation(hero, other string) RelationshipView {
	st := s.engine.State()
	v := RelationshipView{Other: other, Status: RelStranger, Role: "npc"}
	he := st.Entities[hero]
	oe := st.Entities[other]
	if a, ok := he.Relationship[other]; ok {
		v.Affinity = a
	}
	if t, ok := he.Extra["rel_trust_"+other].(float64); ok {
		v.Trust = t
	} else if t, ok := he.Extra["rel_trust_"+other].(int); ok {
		v.Trust = float64(t)
	}
	if stt, ok := he.Extra["rel_status_"+other].(string); ok {
		v.Status = stt
	} else {
		v.Status = RelStatusLabel(v.Affinity, v.Trust)
	}
	if d, ok := he.Extra["rel_since_"+other].(float64); ok {
		v.SinceDay = int(d)
	}
	if evs, ok := he.Extra["rel_events_"+other].(string); ok && evs != "" {
		v.Events = strings.Split(evs, "；")
	}
	if r, ok := oe.Extra["role"].(string); ok {
		v.Role = r
	}
	return v
}

// UpdateRelation 更新双向关系（好感/信任/状态/大事记），返回变更提案
func (s *Simulator) UpdateRelation(hero, other string, affDelta, trustDelta float64, event string) []engine.Change {
	cur := s.ReadRelation(hero, other)
	aff := clamp(cur.Affinity+affDelta, -1, 1)
	trust := clamp(cur.Trust+trustDelta, 0, 1)
	status := RelStatusLabel(aff, trust)
	// 关系大事记
	events := cur.Events
	if event != "" {
		events = append(events, fmt.Sprintf("Day%d·%s", s.day, event))
		if len(events) > 12 {
			events = events[len(events)-12:]
		}
	}
	since := cur.SinceDay
	if since == 0 {
		since = s.day
	}

	var changes []engine.Change
	add := func(path string, v any) {
		changes = append(changes, engine.Change{Path: path, Op: "set", Value: v})
	}
	add(fmt.Sprintf("entities.%s.relationship.%s", hero, other), round2(aff))
	add(fmt.Sprintf("entities.%s.extra.rel_trust_%s", hero, other), round2(trust))
	add(fmt.Sprintf("entities.%s.extra.rel_status_%s", hero, other), status)
	add(fmt.Sprintf("entities.%s.extra.rel_since_%s", hero, other), since)
	add(fmt.Sprintf("entities.%s.extra.rel_events_%s", hero, other), strings.Join(events, "；"))
	// 对方对主角的好感同步演化（对方感受略滞后、幅度略小）
	oc := s.ReadRelation(other, hero)
	oaff := clamp(oc.Affinity+affDelta*0.7, -1, 1)
	ostatus := RelStatusLabel(oaff, oc.Trust)
	add(fmt.Sprintf("entities.%s.relationship.%s", other, hero), round2(oaff))
	add(fmt.Sprintf("entities.%s.extra.rel_status_%s", other, hero), ostatus)
	add(fmt.Sprintf("entities.%s.extra.rel_trust_%s", other, hero), round2(clamp(oc.Trust+trustDelta*0.6, 0, 1)))
	return changes
}

// BreakRelation 感情破裂（分手/决裂/决裂后疏远）：好感与信任骤降
func (s *Simulator) BreakRelation(hero, other string, reason string) []engine.Change {
	return s.UpdateRelation(hero, other, -0.5, -0.4, "关系破裂："+reason)
}

// DecayRelations 时间衰减：久不联系，好感/信任缓慢下降（"只见过几年"的疏远）
func (s *Simulator) DecayRelations(hero string, everyNDays int) []engine.Change {
	st := s.engine.State()
	he := st.Entities[hero]
	if he.Relationship == nil {
		return nil
	}
	var names []string
	for n := range he.Relationship {
		if n != hero {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil
	}
	if s.day%everyNDays != 0 {
		return nil
	}
	var changes []engine.Change
	for _, n := range names {
		v := s.ReadRelation(hero, n)
		if v.Status == RelPartner || v.Status == RelLover || v.Status == RelDating {
			continue // 亲密关系不衰减（用心维系）
		}
		decay := -0.015
		changes = append(changes, engine.Change{Path: fmt.Sprintf("entities.%s.relationship.%s", hero, n), Op: "add", Value: decay})
		// 长时间低好感 → 状态跌回陌生人
		if v.Affinity+decay < 0.12 && v.Status != RelStranger {
			changes = append(changes, engine.Change{Path: fmt.Sprintf("entities.%s.extra.rel_status_%s", hero, n), Op: "set", Value: RelStranger})
		}
	}
	return changes
}

// ---------- 自然产生角色（女主/配角由世界演化，不预设） ----------

// NewCharacter 事件引入的新角色
type NewCharacter struct {
	Name     string `json:"name"`
	Gender   string `json:"gender"`   // 男/女/未知
	Identity string `json:"identity"` // 职业/身份
	Persona  string `json:"persona"`  // 一句话人设（性格/背景）
	Location string `json:"location"` // 首次出场地点
	RoleHint string `json:"role_hint"` // 剧情定位建议：love_interest(潜在女主)/important_npc/rival/npc
	Tier     string `json:"tier"`     // 配角层级：core=核心(建档+记忆) | support=重要(轻量) | walkon=龙套(不建档不记忆)
}

// RegisterCharacter 注册新角色实体（谁登场由世界决定，是否成为女主由互动决定）
// 分层：core=核心配角（完整人设+记忆）| support=重要配角（轻量）| walkon=龙套（不建档不占记忆）
func (s *Simulator) RegisterCharacter(c NewCharacter) []engine.Change {
	if c.Name == "" {
		return nil
	}
	// 已存在则跳过
	if _, ok := s.engine.State().Entities[c.Name]; ok {
		return nil
	}
	tier := strings.TrimSpace(c.Tier)
	if tier == "" {
		// 未标注：按 role_hint 推断（love_interest/rival=core，其余=support）
		switch c.RoleHint {
		case "love_interest", "rival", "important_npc":
			tier = "core"
		default:
			tier = "support"
		}
	}
	var changes []engine.Change
	// 兜底地点：主角所在地优先（任何世界通用）
	fallbackLoc := ""
	if h, ok := s.engine.State().Entities[s.heroName]; ok {
		fallbackLoc = h.Location
	}
	if fallbackLoc == "" {
		fallbackLoc = "本地"
	}
	changes = append(changes,
		engine.Change{Path: "entities." + c.Name + ".location", Op: "set", Value: firstNonEmpty(c.Location, fallbackLoc)},
		engine.Change{Path: "entities." + c.Name + ".job", Op: "set", Value: firstNonEmpty(c.Identity, "本地人")},
		engine.Change{Path: "entities." + c.Name + ".alive", Op: "set", Value: true},
		engine.Change{Path: "entities." + c.Name + ".status", Op: "set", Value: "active"},
		engine.Change{Path: "entities." + c.Name + ".health", Op: "set", Value: 90},
		engine.Change{Path: "entities." + c.Name + ".extra.persona", Op: "set", Value: firstNonEmpty(c.Persona, "一个普通市民")},
		engine.Change{Path: "entities." + c.Name + ".extra.gender", Op: "set", Value: firstNonEmpty(c.Gender, "未知")},
		engine.Change{Path: "entities." + c.Name + ".extra.role", Op: "set", Value: firstNonEmpty(c.RoleHint, "npc")},
		engine.Change{Path: "entities." + c.Name + ".extra.tier", Op: "set", Value: tier},
		engine.Change{Path: "entities." + c.Name + ".extra.debut_day", Op: "set", Value: s.day},
	)
	// 龙套（walkon）：轻量注册，不建档不占记忆——只留一句话人设，出场即走
	if tier == "walkon" {
		return changes
	}
	// 主角认识TA（核心/重要配角才有关系值）
	changes = append(changes, engine.Change{Path: "entities." + s.heroName + ".relationship." + c.Name, Op: "set", Value: 0.08})
	changes = append(changes, engine.Change{Path: "entities." + s.heroName + ".extra.rel_since_" + c.Name, Op: "set", Value: s.day})
	return changes
}

// ---------- 角色生命周期（"只见过几年"：到点离开/远去/死亡） ----------

// CheckLifecycle 检查角色生命周期：活跃期结束 → 触发离开（由世界Agent写理由）
func (s *Simulator) CheckLifecycle() []engine.Change {
	st := s.engine.State()
	var changes []engine.Change
	for name, ent := range st.Entities {
		if name == s.heroName || ent.Status != "active" {
			continue
		}
		exitDay, _ := ent.Extra["exit_day"].(float64)
		if int(exitDay) <= 0 || int(exitDay) != s.day {
			continue
		}
		// 离开方式：默认远去（50%）或决裂/死亡（随机，世界Agent可改写）
		way := "远去"
		changes = append(changes,
			engine.Change{Path: "entities." + name + ".status", Op: "set", Value: "departed"},
			engine.Change{Path: "entities." + name + ".extra.exit_reason", Op: "set", Value: way + "（Day" + fmt.Sprint(s.day) + "）"},
		)
		// 主角记忆沉淀
		s.mem.AddDay(s.heroName, fmt.Sprintf("%s在Day%d%s，之后再没见过", name, s.day, way), "event", 0.85, s.day)
	}
	return changes
}

// FadeOutCheck 配角淡出机制：小说里配角不是永远在场的——
// 长时间没出场的 support/core 配角自动降级为 "mentioned"（只在编年史/事件里被提起，不再主动登场），
// 之后若再次与主角互动则恢复 active。龙套（walkon）不参与（本来就轻量）。
func (s *Simulator) FadeOutCheck() []engine.Change {
	st := s.engine.State()
	if s.llm == nil {
		return nil // 无 LLM 时不淡出（避免 dry-run 误伤）
	}
	var changes []engine.Change
	for name, ent := range st.Entities {
		if name == s.heroName || ent.Status != "active" {
			continue
		}
		tier, _ := ent.Extra["tier"].(string)
		if tier == "walkon" || tier == "" {
			continue // 龙套/未知层级不参与淡出
		}
		lastSeen, _ := ent.Extra["last_seen_day"].(float64)
		// 出场不足 15 天（还在新鲜期）或 30 天内见过 → 不淡出
		if int(lastSeen) <= 0 || s.day-int(lastSeen) < 30 {
			continue
		}
		changes = append(changes,
			engine.Change{Path: "entities." + name + ".status", Op: "set", Value: "mentioned"},
			engine.Change{Path: "entities." + name + ".extra.faded_day", Op: "set", Value: s.day},
			engine.Change{Path: "entities." + name + ".extra.faded_reason", Op: "set", Value: "淡出视野（Day" + fmt.Sprint(s.day) + "后仅偶尔被提起）"},
		)
		// 主角记忆沉淀："好久没见过TA了"
		s.mem.AddDay(s.heroName, fmt.Sprintf("%s已经很久没出现在生活里了，只在别人嘴里偶尔听到", name), "event", 0.7, s.day)
	}
	return changes
}

// ---------- 工具 ----------

// castNames 返回当前世界角色名单（主角 + 活跃NPC），用于生成/维护人设档案
// 分层：core/support 参与建档；walkon（龙套）不建档不占记忆
func (s *Simulator) castNames() []string {
	var names []string
	seen := map[string]bool{}
	if s.heroName != "" {
		names = append(names, s.heroName)
		seen[s.heroName] = true
	}
	for name, ent := range s.engine.State().Entities {
		if seen[name] {
			continue
		}
		if ent.Status == "active" || ent.Alive {
			if tier, _ := ent.Extra["tier"].(string); tier == "walkon" {
				continue // 龙套不建档
			}
			names = append(names, name)
		}
	}
	return names
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}