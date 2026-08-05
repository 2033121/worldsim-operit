package novel

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ---------- 小说描写素材库（真人大神作品提取：825条/119子类） ----------

type MaterialSample struct {
	Class  string // 大类（一_打斗·战斗）
	Sub    string // 子类（3.1 自然风景）
	Source string // 来源作品
	Text   string // 示范片段
}

// MaterialBank 素材库：按需检索注入小说写手，模仿质感不抄袭
type MaterialBank struct {
	samples []MaterialSample
	byClass map[string][]MaterialSample
	bySub   map[string][]MaterialSample
}

// LoadMaterialBank 加载素材库目录（material/*.md）
func LoadMaterialBank(dir string) *MaterialBank {
	mb := &MaterialBank{byClass: map[string][]MaterialSample{}, bySub: map[string][]MaterialSample{}}
	if dir == "" {
		return mb
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return mb
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		class := strings.TrimSuffix(e.Name(), ".md")
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		mb.parseClass(class, string(data))
	}
	fmt.Printf(" [素材库] 加载 %d 条描写示范（%d 类）\n", len(mb.samples), len(mb.byClass))
	return mb
}

func (mb *MaterialBank) parseClass(class, content string) {
	lines := strings.Split(content, "\n")
	var cur *MaterialSample
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "### ") {
			// 新示范块
			if cur != nil && strings.TrimSpace(cur.Text) != "" {
				mb.add(cur)
			}
			cur = &MaterialSample{Class: class}
			continue
		}
		if cur == nil {
			continue
		}
		if strings.HasPrefix(ln, "**来源**：") {
			cur.Source = strings.TrimPrefix(ln, "**来源**：")
			continue
		}
		if strings.HasPrefix(ln, "## ") {
			cur.Sub = strings.TrimPrefix(ln, "## ")
			continue
		}
		// 正文（去首行缩进）
		body := strings.TrimSpace(ln)
		if body != "" {
			cur.Text += body + "\n"
		}
	}
	if cur != nil && strings.TrimSpace(cur.Text) != "" {
		mb.add(cur)
	}
}

func (mb *MaterialBank) add(s *MaterialSample) {
	s.Text = strings.TrimSpace(s.Text)
	// 只保留示范正文（去掉标题行）
	if s.Text == "" || len([]rune(s.Text)) < 30 {
		return
	}
	mb.samples = append(mb.samples, *s)
	mb.byClass[s.Class] = append(mb.byClass[s.Class], *s)
	if s.Sub != "" {
		mb.bySub[s.Sub] = append(mb.bySub[s.Sub], *s)
	}
}

// PickFor 按本章"戏的类型"精准投喂示范（旧版按类名关键词匹配几乎总落空→实际随机抽，已废弃）：
//   1. 从本章素材识别戏的类型（对话/打斗/心理/场景/人物/情节/氛围，关键词打分）
//   2. 命中类型优先抽；对话类必抽（网文骨架：对话独立成段）；不足随机补足
//   3. 每条标注"用途"（这段示范学什么）——写手照形态学，不照抄
func (mb *MaterialBank) PickFor(chapterMaterial string, maxPer, maxTotal int) string {
	if mb == nil || len(mb.samples) == 0 {
		return ""
	}
	// 类型识别词表（大类 ↔ 本章戏的类型 ↔ 触发词）
	typeProfiles := []struct {
		class string
		label string
		kws   []string
	}{
		{"四_对话·语言描写", "对话交锋", []string{"说", "问", "答", "喊", "吼", "骂", "笑", "开口", "声音", "话", "沉默", "劝", "警告", "试探", "质问"}},
		{"一_打斗·战斗·动作描写", "动作/打斗", []string{"打", "冲", "躲", "拳", "刀", "逃", "追", "扑", "砸", "撞", "踢", "抓", "闪", "反击", "扑倒"}},
		{"二_情感·心理描写", "情感/心理", []string{"想", "怕", "恨", "爱", "心", "怒", "慌", "委屈", "难受", "紧张", "忍", "哭", "后悔", "不甘"}},
		{"三_场景·环境描写", "场景/环境", []string{"街", "灯", "楼", "门", "夜", "雨", "雾", "巷", "店", "窗口", "城市", "工地", "天桥"}},
		{"五_人物刻画·形象描写", "人物刻画", []string{"眼神", "脸", "手", "笑", "身影", "模样", "抬头", "盯着", "站", "走", "坐下", "皱纹", "衣着"}},
		{"六_情节·事件情景描写", "情节/事件", []string{"发生", "发现", "突然", "追查", "线索", "交易", "绑架", "撞见", "意外", "变故", "对峙", "局"}},
		{"七_氛围·意境·感官描写", "氛围/感官", []string{"冷", "暗", "潮", "腥", "闷", "静", "刺", "烫", "凉", "味", "光", "阴", "压抑"}},
		{"八_生活质感·日常切片", "生活/日常", []string{"吃", "饭", "店", "夜班", "回家", "睡", "做饭", "买菜", "上班", "下班", "邻居", "家里", "家常", "铺", "摊", "巷", "早饭", "晚饭", "食堂", "柜", "账", "洗", "晾", "排队"}},
	}
	type scored struct {
		class string
		label string
		score int
	}
	var ranked []scored
	for _, p := range typeProfiles {
		score := 0
		for _, kw := range p.kws {
			if strings.Contains(chapterMaterial, kw) {
				score++
			}
		}
		ranked = append(ranked, scored{p.class, p.label, score})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })

	var picked []MaterialSample
	seen := map[string]bool{}
	addPool := func(cls string, n int) {
		pool := mb.byClass[cls]
		if len(pool) == 0 {
			return
		}
		start := rand.Intn(len(pool))
		for i := 0; i < len(pool) && n > 0; i++ {
			s := pool[(start+i)%len(pool)]
			if seen[s.Text] {
				continue
			}
			seen[s.Text] = true
			picked = append(picked, s)
			n--
		}
	}
	// 1. 识别出的类型（score>0）优先抽，每类 maxPer 条
	for _, r := range ranked {
		if r.score == 0 || len(picked) >= maxTotal {
			break
		}
		addPool(r.class, maxPer)
	}
	// 2. 对话类必抽（网文骨架：对话独立成段、带信息量）
	if len(picked) < maxTotal {
		addPool("四_对话·语言描写", 2)
	}
	// 3. 不足随机补（全库随机，保证每章都有示范）
	if len(picked) < maxTotal {
		classes := make([]string, 0, len(mb.byClass))
		for c := range mb.byClass {
			classes = append(classes, c)
		}
		rand.Shuffle(len(classes), func(i, j int) { classes[i], classes[j] = classes[j], classes[i] })
		for _, c := range classes {
			if len(picked) >= maxTotal {
				break
			}
			addPool(c, 1)
		}
	}
	if len(picked) == 0 {
		return ""
	}
	labelOf := func(class string) string {
		for _, r := range ranked {
			if r.class == class {
				return r.label
			}
		}
		switch class {
		case "四_对话·语言描写":
			return "对话交锋"
		case "一_打斗·战斗·动作描写":
			return "动作/打斗"
		case "二_情感·心理描写":
			return "情感/心理"
		}
		return "形态参考"
	}
	var sb strings.Builder
	sb.WriteString("网文段落形态示范（真实网文原文片段。每条标注用途——参考它的节奏/写法，禁止照抄）：\n")
	for i, s := range picked {
		if i >= maxTotal {
			break
		}
		sb.WriteString(fmt.Sprintf("· 用途：%s（%s｜%s）\n%s\n", labelOf(s.Class), s.Class, s.Sub, truncateRunes(s.Text, 240)))
	}
	return strings.TrimSpace(sb.String())
}

func splitCN(s string) []string {
	runes := []rune(s)
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(runes)-1; i++ {
		w := string(runes[i : i+2])
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}