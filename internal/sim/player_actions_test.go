package sim

import (
	"context"
	"strings"
	"testing"

	"worldsim/internal/engine"
)

// testAPSimulator 构造带引擎的 Simulator（Phase4 行动测试）
func testAPSimulator(t *testing.T) *Simulator {
	t.Helper()
	se := engine.NewStateEngine(engine.Rules{}, t.TempDir()+"/ev.log")
	se.State().Entities = map[string]engine.Entity{
		"颜平": {Location: "青叶林", Job: "散修", Health: 60, Money: 20, Status: "active",
			Stats:       map[string]any{"境界": "炼气三层", "灵植术": 5},
			Relationship: map[string]float64{"周伯": 0.5},
		},
		"周伯": {Location: "青叶林", Job: "老仆", Health: 90, Status: "active"},
	}
	s := NewSimulator(se, t.TempDir())
	s.SetHeroName("颜平")
	return s
}

// TestPlayerActionRest 休整：健康+8、扣行动点、写入世界状态
func TestPlayerActionRest(t *testing.T) {
	s := testAPSimulator(t)
	ctx := context.Background()

	res, err := s.PlayerAction(ctx, "rest", "")
	if err != nil {
		t.Fatalf("休整失败: %v", err)
	}
	if res.APLeft != maxPlayerAP-1 {
		t.Fatalf("行动点应扣1: %+v", res)
	}
	// 世界状态真实变化
	if got := s.engine.State().Entities["颜平"].Health; got != 68 {
		t.Fatalf("健康应+8→68, 实际 %v", got)
	}
	if s.PlayerAPBalance() != maxPlayerAP-1 {
		t.Fatalf("余额应%d, 实际 %d", maxPlayerAP-1, s.PlayerAPBalance())
	}
	// 满血不能刷
	s.engine.State().Entities["颜平"] = engine.Entity{Health: 98, Stats: map[string]any{"灵植术": 5}, Status: "active"}
	if _, err := s.PlayerAction(ctx, "rest", ""); err == nil {
		t.Fatal("满血休整应报错")
	}
}

// TestPlayerActionCultivate 精进：能力+1
func TestPlayerActionCultivate(t *testing.T) {
	s := testAPSimulator(t)
	// 清空行动点→验证不足报错
	if _, err := s.PlayerAction(context.Background(), "rest", ""); err != nil {
		t.Fatal(err)
	}
	// 精进灵植术 5→6（灵植术是首个≤20数值属性？"境界"是字符串，灵植术5是数值）
	res, err := s.PlayerAction(context.Background(), "cultivate", "")
	if err != nil {
		t.Fatalf("精进失败: %v", err)
	}
	if got := s.engine.State().Entities["颜平"].Stats["灵植术"]; got != float64(6) {
		t.Fatalf("灵植术应6, 实际 %v", got)
	}
	if !strings.Contains(res.Message, "灵植术") {
		t.Fatalf("反馈应含属性名: %q", res.Message)
	}
}

// TestPlayerActionSocial 会晤：关系+0.05
func TestPlayerActionSocial(t *testing.T) {
	s := testAPSimulator(t)
	res, err := s.PlayerAction(context.Background(), "social", "周伯")
	if err != nil {
		t.Fatalf("会晤失败: %v", err)
	}
	if got := s.engine.State().Entities["颜平"].Relationship["周伯"]; got != 0.55 {
		t.Fatalf("关系应0.55, 实际 %v", got)
	}
	if res.NewValue != 0.55 {
		t.Fatalf("NewValue 应0.55: %+v", res)
	}
	// 无效目标
	if _, err := s.PlayerAction(context.Background(), "social", "不存在"); err == nil {
		t.Fatal("无效目标应报错")
	}
}

// TestPlayerAPRecover 行动点每日恢复
func TestPlayerAPRecover(t *testing.T) {
	s := testAPSimulator(t)
	// 用掉3点
	for i := 0; i < 3; i++ {
		if _, err := s.PlayerAction(context.Background(), "rest", ""); err != nil {
			// rest满血会报错，用social代替（关系会变但无妨）
			s.PlayerAction(context.Background(), "social", "周伯")
		}
	}
	left := s.PlayerAPBalance()
	if left >= maxPlayerAP {
		t.Fatalf("应已消耗行动点: %d", left)
	}
	// 模拟跑2天
	s.recoverAP(s.day + 1)
	s.recoverAP(s.day + 2)
	if got := s.PlayerAPBalance(); got != left+2 {
		t.Fatalf("应恢复2点: %d → %d", left, got)
	}
	// 上限
	for i := 0; i < 10; i++ {
		s.recoverAP(s.day + 10 + i)
	}
	if got := s.PlayerAPBalance(); got > maxPlayerAP {
		t.Fatalf("行动点超上限: %d", got)
	}
}

// TestPlayerActionZeroPollution 行动系统零污染
func TestPlayerActionZeroPollution(t *testing.T) {
	s := testAPSimulator(t)
	res, _ := s.PlayerAction(context.Background(), "rest", "")
	for _, bad := range []string{"修仙", "境界", "灵根", "炼气", "宗门", "法术", "斗气", "魔法"} {
		if strings.Contains(res.Message, bad) {
			t.Fatalf("行动反馈污染: 含 %q", bad)
		}
	}
}