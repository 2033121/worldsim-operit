package sim

import (
	"strings"
	"testing"

	"worldsim/internal/engine"
)

// TestPlayerStateFull 玩家状态面板：主角卡/段落/关系/行动选项 全部由世界数据生成
func TestPlayerStateFull(t *testing.T) {
	se := engine.NewStateEngine(engine.Rules{}, t.TempDir()+"/ev.log")
	se.State().Day = 42
	se.State().Entities = map[string]engine.Entity{
		"颜平": {
			Location: "青叶林", Job: "散修", Health: 60, Money: 20, Status: "active",
			Stats: map[string]any{"境界": "炼气三层", "灵根": "木灵根"},
			Relationship: map[string]float64{"周伯": 0.9, "赵兴": 0.1, "路人甲": 0.02},
			Extra:       map[string]any{"rel_status_周伯": "生死之交"},
		},
		"周伯": {Location: "青叶林", Job: "老仆", Status: "active"},
	}
	s := NewSimulator(se, t.TempDir())
	s.SetHeroName("颜平")
	s.arcBook = []ArcEntry{{Num: 1, Day: 40, ArcName: "林间试炼", Goal: "查明雾谷异象", Villain: "雾中黑影", Status: "open"}}
	s.chronicle = []ChronicleEntry{
		{Day: 41, Kind: "EVENT", Content: "「雾谷回响」迷雾吞了半个猎户村"},
		{Day: 42, Kind: "STATE", Content: "主角抵达雾谷外围"},
	}
	// 已有玩家指令（历史）
	s.QueuePlayerIntent("让主角去雾谷深处看看", "主角")

	ps := s.PlayerState()

	// 主角卡
	if ps.Hero.Name != "颜平" || ps.Hero.Health != 60 || ps.Hero.Money != 20 {
		t.Fatalf("主角卡字段错误: %+v", ps.Hero)
	}
	if ps.Hero.Capabilities["境界"] != "炼气三层" {
		t.Fatalf("能力值缺失: %+v", ps.Hero.Capabilities)
	}
	// 当前段落
	if ps.Arc.Name != "林间试炼" || ps.Arc.Villain != "雾中黑影" {
		t.Fatalf("段落错误: %+v", ps.Arc)
	}
	// 关系：最高值在前（周伯0.9 > 赵兴0.1），路人甲(0.02<0.1)应被过滤
	if len(ps.RelTop) != 2 || ps.RelTop[0].Name != "周伯" {
		t.Fatalf("关系排序错误: %+v", ps.RelTop)
	}
	if ps.RelTop[0].Rank != "生死之交" {
		t.Fatalf("关系档位应从世界书读: %+v", ps.RelTop[0])
	}
	// 行动选项：4 个（调查/交际/精进/休整-健康60<70）
	if len(ps.Actions) != 4 {
		t.Fatalf("行动选项应为4个, 实际 %d: %+v", len(ps.Actions), ps.Actions)
	}
	hasKind := map[string]bool{}
	for _, a := range ps.Actions {
		hasKind[a.Kind] = true
		if a.Intent == "" || a.Label == "" {
			t.Fatalf("行动缺字段: %+v", a)
		}
	}
	if !hasKind["investigate"] || !hasKind["social"] || !hasKind["cultivate"] || !hasKind["rest"] {
		t.Fatalf("行动类型缺失: %v", hasKind)
	}
	// 最近事件
	if len(ps.Recent) == 0 || !strings.Contains(ps.Recent[0], "雾谷回响") {
		t.Fatalf("最近事件错误: %+v", ps.Recent)
	}
	// 指令统计
	if ps.Stats["pending"] != 1 {
		t.Fatalf("pending 应为1: %+v", ps.Stats)
	}
}

// TestPlayerStateNoHero 未初始化：玩家面板不崩
func TestPlayerStateNoHero(t *testing.T) {
	se := engine.NewStateEngine(engine.Rules{}, t.TempDir()+"/ev.log")
	s := NewSimulator(se, t.TempDir())
	ps := s.PlayerState()
	if ps.Hero.Name != "" || ps.Day != 0 {
		t.Fatalf("未初始化应返回空主角: %+v", ps.Hero)
	}
	if len(ps.Actions) != 0 {
		t.Fatalf("未初始化不应有行动: %+v", ps.Actions)
	}
}

// TestPlayerStateZeroPollution 行动选项 prompt 零污染
func TestPlayerStateZeroPollution(t *testing.T) {
	se := engine.NewStateEngine(engine.Rules{}, t.TempDir()+"/ev.log")
	se.State().Entities = map[string]engine.Entity{
		"颜平": {Location: "青叶林", Job: "散修", Health: 80, Money: 20, Status: "active",
			Stats:       map[string]any{"境界": "炼气三层"},
			Relationship: map[string]float64{"周伯": 0.9},
		},
	}
	s := NewSimulator(se, t.TempDir())
	s.SetHeroName("颜平")
	s.arcBook = []ArcEntry{{Num: 1, Day: 1, ArcName: "日常", Goal: "安稳度日", Status: "open"}}

	for _, a := range s.buildActions() {
		// 行动文案应来自抽象模板（调查/深谈/提升/休整/打听/探察），由世界数据填充
		if !strings.Contains(a.Intent, "调查") && !strings.Contains(a.Intent, "深谈") &&
			!strings.Contains(a.Intent, "提升") && !strings.Contains(a.Intent, "休整") &&
			!strings.Contains(a.Intent, "打听") && !strings.Contains(a.Intent, "探察") {
			t.Fatalf("行动文案应来自抽象模板: %+v", a.Intent)
		}
	}
}