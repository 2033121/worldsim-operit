package novel

import (
	"fmt"
	"sort"
	"strings"

	"worldsim/internal/sim"
)

// PickChapterChronicle 本章编年史精选（确定性规则，零 LLM 零成本，同数据同结果）。
//
// 规则（用户确认版）：
//  1. 类型过滤：保留 SAID（对话原话，最高价值）+ FACT（真实事件），排除 STATE（属性设置流水账，
//     实测占编年史 81%，全是 "entities.X.字段 set → 值" 的噪声，无叙事价值）。
//  2. 权重：SAID 无条件保留（原话是灵魂）；FACT 需 Weight ≥ 0.5（FACT 实测均值 0.511，正好滤掉一半平庸条目）。
//  3. 同天去重：同一天同一内容只保留一次（防重复注入）。
//  4. 排序取 top10：权重降序，同权重对话优先，再按天升序（保时间线）。
//  5. 对话原文保真：Content 原样输出，引号内不许改词。
//
// 输出形如：
//
//	【本章编年史精选（模拟世界里真实发生过的对话与事件，可作细节原料；引号内为原话禁止改词）】
//	· day3 [对话] 周伯："吐血了还蹲着忙活，命不要啦？手巾接着，擦擦"
//	· day5 [事件] 赵福查田册：灵田亩产报得高，要另核田契
func PickChapterChronicle(chronicle []sim.ChronicleEntry, dayStart, dayEnd int) string {
	type picked struct {
		day     int
		content string
		weight  float64
		kind    string
	}
	var picks []picked
	seen := map[string]bool{}

	for _, e := range chronicle {
		if e.Day < dayStart || e.Day > dayEnd {
			continue
		}
		// ① 类型过滤：STATE 排除（属性流水账无叙事价值）
		if e.Kind == "STATE" {
			continue
		}
		// 零污染：过滤历史 fmt 错误残留（%!(EXTRA ...) 是 Go 格式化错误字符串，禁止进 prompt）
		if strings.Contains(e.Content, "%!(") {
			continue
		}
		// ② 权重过滤：FACT 需 ≥0.5；SAID 无条件保留
		if e.Kind == "FACT" && e.Weight < 0.5 {
			continue
		}
		// ③ 同天去重
		key := fmt.Sprintf("%d|%s", e.Day, e.Content)
		if seen[key] {
			continue
		}
		seen[key] = true
		picks = append(picks, picked{e.Day, e.Content, e.Weight, e.Kind})
	}
	if len(picks) == 0 {
		return ""
	}
	// ④ 排序：权重降序 → 对话优先 → 天升序，取 top10
	sort.SliceStable(picks, func(i, j int) bool {
		if picks[i].weight != picks[j].weight {
			return picks[i].weight > picks[j].weight
		}
		if picks[i].kind != picks[j].kind {
			return picks[i].kind == "SAID"
		}
		return picks[i].day < picks[j].day
	})
	if len(picks) > 10 {
		picks = picks[:10]
	}

	var sb strings.Builder
	sb.WriteString("【本章编年史精选（模拟世界里真实发生过的对话与事件，可作细节原料增强贴合；引号内为人物原话，禁止改词；未使用不强制）】\n")
	for _, p := range picks {
		label := "事件"
		if p.kind == "SAID" {
			label = "对话"
		}
		sb.WriteString(fmt.Sprintf("· day%d [%s] %s\n", p.day, label, p.content))
	}
	return strings.TrimSpace(sb.String())
}
