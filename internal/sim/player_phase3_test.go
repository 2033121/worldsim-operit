package sim

import (
	"strings"
	"testing"

	"worldsim/internal/engine"
)

// TestSimulateCombat 战斗模拟：双方数据→攻防→回合→胜负
func TestSimulateCombat(t *testing.T) {
	se := engine.NewStateEngine(engine.Rules{}, t.TempDir()+"/ev.log")
	se.State().Entities = map[string]engine.Entity{
		"颜平": {Location: "青叶林", Job: "散修", Health: 80, Money: 20, Status: "active",
			Stats: map[string]any{"境界": "炼气三层", "灵植术": 5}},
		"赵兴": {Location: "青叶镇", Job: "富商", Health: 60, Money: 500, Status: "active",
			Stats: map[string]any{"身手": 3}},
	}
	s := NewSimulator(se, t.TempDir())
	s.SetHeroName("颜平")

	r := s.SimulateCombat("赵兴")
	if r.Winner == "invalid" {
		t.Fatalf("战斗应有效: %+v", r)
	}
	if len(r.Rounds) == 0 {
		t.Fatal("应有回合记录")
	}
	if r.WinRate < 0 || r.WinRate > 100 {
		t.Fatalf("胜率范围错误: %.1f", r.WinRate)
	}
	if r.PowerGap == "" {
		t.Fatal("实力差距描述缺失")
	}
	// 每回合都有攻方
	for _, rd := range r.Rounds {
		if rd.Attacker != "颜平" && rd.Attacker != "赵兴" {
			t.Fatalf("回合攻方错误: %+v", rd)
		}
	}
	// 无效对手
	inv := s.SimulateCombat("不存在的人")
	if inv.Winner != "invalid" {
		t.Fatalf("无效对手应返回 invalid: %+v", inv)
	}
}

// TestCombatZeroPollution 战斗模块零污染：模板不含具体体系词
func TestCombatZeroPollution(t *testing.T) {
	se := engine.NewStateEngine(engine.Rules{}, t.TempDir()+"/ev.log")
	se.State().Entities = map[string]engine.Entity{
		"颜平": {Health: 80, Stats: map[string]any{"境界": "炼气三层"}},
		"赵兴": {Health: 60, Stats: map[string]any{"身手": 3}},
	}
	s := NewSimulator(se, t.TempDir())
	s.SetHeroName("颜平")
	r := s.SimulateCombat("赵兴")
	// 模板层不应出现任何具体体系词；主角/对手名是运行时数据（不算污染）
	for _, bad := range []string{"修仙", "境界", "灵根", "炼气", "宗门", "法术", "斗气", "魔法", "灵植"} {
		if strings.Contains(FormatCombatSummary(r), bad) {
			t.Fatalf("战斗摘要污染: 含 %q", bad)
		}
	}
	// 回合动作文案抽象
	for _, rd := range r.Rounds {
		if rd.Action != "出手命中" && rd.Action != "出手落空" && rd.Action != "对峙" {
			t.Fatalf("回合动作应抽象: %q", rd.Action)
		}
	}
}

// TestQuests 任务卡：里程碑→任务，arcDone 驱动状态
func TestQuests(t *testing.T) {
	se := engine.NewStateEngine(engine.Rules{}, t.TempDir()+"/ev.log")
	s := NewSimulator(se, t.TempDir())
	s.arcBook = []ArcEntry{{Num: 1, Day: 1, ArcName: "旧契反手", Status: "open",
		Milestones: []string{"夜访暗线", "老槐落叶", "勘界对峙", "旧契反噬"}}}

	// arcDone=1：第1个完成，第2个进行中
	s.arcDone = 1
	qs := s.Quests()
	if len(qs) != 4 {
		t.Fatalf("应有4个任务, 实际 %d", len(qs))
	}
	want := []string{"done", "active", "upcoming", "upcoming"}
	for i, q := range qs {
		if q.Status != want[i] {
			t.Fatalf("任务%d状态应为 %s, 实际 %s (%+v)", i, want[i], q.Status, q)
		}
		if q.Title == "" || q.ArcName != "旧契反手" || q.Total != 4 {
			t.Fatalf("任务字段错误: %+v", q)
		}
	}
	// 段落收尾后无任务
	s.arcBook[0].Status = "done"
	if len(s.Quests()) != 0 {
		t.Fatal("段落结束后应无任务")
	}
	// QuestPrompt 含进行中任务（arcDone=1 → active 是第 2 个"老槐落叶"）
	s.arcBook[0].Status = "open"
	if !strings.Contains(s.QuestPrompt(), "老槐落叶") {
		t.Fatalf("QuestPrompt 应含 active 任务: %q", s.QuestPrompt())
	}
}