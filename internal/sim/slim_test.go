package sim

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"worldsim/internal/engine"
)

// ---------- slimHeroJSON：稳定序分级实体 ----------

func TestSlimHeroJSONStableOrder(t *testing.T) {
	ents := map[string]engine.Entity{
		"赵兴":   {Location: "青叶镇", Job: "商人", Status: "active", Money: 10, Health: 1.0, Relationship: map[string]float64{"颜平": 0.5}},
		"颜平":   {Location: "青叶林", Job: "散修", Status: "active", Money: 5, Health: 0.9, Stats: map[string]any{"境界": "练气"}},
		"路人甲": {Location: "街上", Job: "卖菜", Status: "active"},
		"已逝者": {Location: "坟地", Job: "无", Status: "departed"},
	}
	out := slimHeroJSON("颜平", ents)

	// departed 过滤
	if strings.Contains(out, "已逝者") {
		t.Error("departed 实体不应出现在输出中")
	}
	// 顺序：龙套(路人甲) → 重要配角(赵兴) → 主角(颜平)
	idxLuguo := strings.Index(out, "路人甲")
	idxZhao := strings.Index(out, "赵兴")
	idxHero := strings.Index(out, "颜平")
	if !(idxLuguo >= 0 && idxLuguo < idxZhao && idxZhao < idxHero) {
		t.Errorf("排序错误：龙套(%d) < 配角(%d) < 主角(%d)", idxLuguo, idxZhao, idxHero)
	}
	// 合法 JSON
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Errorf("输出不是合法 JSON: %v", err)
	}
}

func TestSlimHeroJSONDeterministic(t *testing.T) {
	ents := map[string]engine.Entity{
		"赵兴":   {Location: "青叶镇", Job: "商人", Status: "active"},
		"颜平":   {Location: "青叶林", Job: "散修", Status: "active", Stats: map[string]any{"境界": "练气"}},
		"路人甲": {Location: "街上", Job: "卖菜", Status: "active"},
	}
	// 同一输入两次输出必须完全一致（缓存前缀稳定性的根基）
	a := slimHeroJSON("颜平", ents)
	b := slimHeroJSON("颜平", ents)
	if a != b {
		t.Errorf("相同输入两次输出不一致（缓存前缀会被打碎）\na=%s\nb=%s", a, b)
	}
}

func TestSlimHeroJSONFieldTier(t *testing.T) {
	ents := map[string]engine.Entity{
		"颜平":   {Location: "青叶林", Job: "散修", Status: "active", Money: 5, Health: 0.9, Stats: map[string]any{"境界": "练气"}},
		"路人甲": {Location: "街上", Job: "卖菜", Status: "active", Money: 2, Health: 1.0},
	}
	out := slimHeroJSON("颜平", ents)
	// 主角全量（含 stats/money/health），龙套只有基础三字段
	heroSec := out[strings.Index(out, "颜平"):]
	if !strings.Contains(heroSec, `"stats"`) {
		t.Error("主角应含 stats")
	}
	luguoSec := out[:strings.Index(out, "颜平")]
	if strings.Contains(luguoSec, `"stats"`) || strings.Contains(luguoSec, `"money"`) {
		t.Error("龙套不应含 stats/money（分级摘要）")
	}
}

// ---------- compactArcJSON / rawJSON：段落全量保留 ----------

func TestCompactArcJSONFullPreserve(t *testing.T) {
	raw := `{"arc_name":"验种前夜","goal":"拿到印牙","villain":"青巾客搅局","foreshadow_focus":"赵桐锦盒","milestones":["夜半合契","独眼认印"],"payoff":"印牙到手","time_hint":"3天","cycle":"目标","payoff_type":"打脸","golden_finger_stage":"用法","energy_phase":"储备"}`
	out := compactArcJSON(raw)
	// 全字段保留不截断
	for _, field := range []string{"arc_name", "goal", "villain", "foreshadow_focus", "milestones", "payoff", "time_hint", "cycle", "payoff_type", "golden_finger_stage", "energy_phase"} {
		if !strings.Contains(out, field) {
			t.Errorf("字段被丢: %s\nout=%s", field, out)
		}
	}
	// 值完整（milestones 数组元素都在）
	if !strings.Contains(out, "夜半合契") || !strings.Contains(out, "独眼认印") {
		t.Error("milestones 内容被截断")
	}
	// 合法 JSON
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Errorf("输出不是合法 JSON: %v", err)
	}
}

func TestCompactArcJSONBadInputFallback(t *testing.T) {
	// 非法输入：原样返回（兜底不 panic）
	out := compactArcJSON("不是JSON")
	if out != "不是JSON" {
		t.Errorf("非法输入应原样返回，got: %s", out)
	}
}

func TestArcRawJSONRoundTrip(t *testing.T) {
	e := ArcEntry{
		ArcName: "验种前夜", Goal: "拿到印牙", Villain: "青巾客搅局",
		ForeshadowFocus: "赵桐锦盒", Milestones: []string{"夜半合契", "独眼认印"},
		Payoff: "印牙到手", TimeHint: "3天", Cycle: "目标",
		PayoffType: "打脸", GoldenFinger: "用法", EnergyPhase: "储备",
	}
	out := e.rawJSON()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Errorf("rawJSON 不是合法 JSON: %v", err)
	}
	if parsed["arc_name"] != "验种前夜" || parsed["cycle"] != "目标" {
		t.Errorf("rawJSON 字段丢失: %v", parsed)
	}
	// compactArcJSON(rawJSON(e)) 应等值（双路径一致性：运行时与重启加载行为一致）
	if compactArcJSON(out) != out {
		t.Error("compactArcJSON(rawJSON(e)) != rawJSON(e)：两条路径不一致！")
	}
}

// ---------- lastN ----------

func TestLastN(t *testing.T) {
	s := []string{"a", "b", "c"}
	if got := lastN(s, 2); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Errorf("lastN(3,2) = %v", got)
	}
	if got := lastN(s, 10); !reflect.DeepEqual(got, s) {
		t.Errorf("lastN(3,10) 应原样返回, got %v", got)
	}
}
