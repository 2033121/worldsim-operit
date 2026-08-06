package sim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestQueueAndConsume 玩家指令：入队→pending→事件日取走→结算回执
func TestQueueAndConsume(t *testing.T) {
	s := testSimulator(t)

	id, err := s.QueuePlayerIntent("让主角去调查那件怪事", "主角")
	if err != nil || id == "" {
		t.Fatalf("入队失败: err=%v id=%q", err, id)
	}
	if _, err := s.QueuePlayerIntent("   ", ""); err == nil {
		t.Fatal("空指令应报错")
	}

	pend := s.pendingPlayerIntents()
	if len(pend) != 1 || pend[0].Status != "pending" {
		t.Fatalf("pending 应为1条, 实际 %d (%+v)", len(pend), pend)
	}
	if pend[0].Intent != "让主角去调查那件怪事" || pend[0].Target != "主角" {
		t.Fatalf("字段不符: %+v", pend[0])
	}

	// 模拟事件日结算
	evs := []EventCard{
		{Day: 5, Title: "夜探枯井", Location: "村外枯井"},
		{Day: 5, Title: "井底线索", Location: "枯井"},
	}
	s.settlePlayerIntents(playerIntentIDs(pend), 5, playerEventSummary(evs))

	all := s.PlayerIntents()
	if all[0].Status != "consumed" || all[0].ConsumedDay != 5 {
		t.Fatalf("结算后应为 consumed/day5: %+v", all[0])
	}
	if !strings.Contains(all[0].Response, "夜探枯井") {
		t.Fatalf("回执应含事件标题: %q", all[0].Response)
	}
	// 消费后 pending 应为空
	if len(s.pendingPlayerIntents()) != 0 {
		t.Fatal("消费后 pending 应为空")
	}
}

// TestPlayerIntentPersistence 指令持久化：保存→重载（模拟重启）不丢
func TestPlayerIntentPersistence(t *testing.T) {
	dir := t.TempDir()
	s := &Simulator{worldDir: dir}
	s.QueuePlayerIntent("让主角闭关突破", "")

	// 模拟重启：新建实例加载
	s2 := &Simulator{worldDir: dir}
	s2.loadPlayerIntents()
	all := s2.PlayerIntents()
	if len(all) != 1 || all[0].Intent != "让主角闭关突破" || all[0].Status != "pending" {
		t.Fatalf("重启后指令丢失: %+v", all)
	}
	// 文件权限应收紧
	info, err := os.Stat(filepath.Join(dir, "player_intents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("文件权限应为 0600, 实际 %v", info.Mode().Perm())
	}
}

// TestPlayerIntentPrompt 提示词零污染 + 指令注入完整
func TestPlayerIntentPrompt(t *testing.T) {
	ints := []PlayerIntent{
		{Intent: "让主角去调查", Target: "主角"},
		{Intent: "想办法加入那个大势力"},
	}
	p := playerIntentPrompt(ints)
	if !strings.Contains(p, "让主角去调查") || !strings.Contains(p, "想办法加入那个大势力") {
		t.Fatalf("prompt 缺指令内容: %s", p)
	}
	if !strings.Contains(p, "涉及目标：主角") {
		t.Fatalf("prompt 缺 target: %s", p)
	}
	// 零污染：不得出现具体世界/角色/示例词汇
	for _, bad := range []string{"修仙", "都市", "灵根", "宗门", "颜平", "赵兴", "青叶林"} {
		if strings.Contains(p, bad) {
			t.Fatalf("提示词污染: 含 %q", bad)
		}
	}
	if playerIntentPrompt(nil) != "" {
		t.Fatal("空指令应返回空串")
	}
}

// testSimulator 构造最小 Simulator（不含 LLM/engine，只测玩家介入逻辑）
func testSimulator(t *testing.T) *Simulator {
	t.Helper()
	return &Simulator{worldDir: t.TempDir()}
}