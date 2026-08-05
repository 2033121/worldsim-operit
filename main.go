// WorldSim — 多Agent世界模拟器 + 小说化流水线
// 魔改自 Nigh/show-me-the-story（Go 单二进制 + WebUI，零外部依赖）
//
// 双端口架构：
//   :48090 小说创作服务（复用 show-me-the-story 的小说化流水线）
//   :48091 世界模拟服务（WorldSim State Engine + 调度器，新增）
package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"worldsim/internal/config"
	"worldsim/internal/engine"
	"worldsim/internal/health"
	"worldsim/internal/httpapi"
	"worldsim/internal/llm"
	"worldsim/internal/logx"
	"worldsim/internal/novel"
	"worldsim/internal/sim"
	"worldsim/internal/sse"
	"worldsim/internal/worldbook"
)

//go:embed static
var staticFiles embed.FS

//go:embed wsweb
var wsWeb embed.FS

var version = "dev"

const (
	storyPort  = ":48090" // 小说化服务（原项目功能）
	worldPort  = ":48091" // 世界模拟服务（WorldSim 新增）
)

func main() {
	progDir := resolveProgDir()
	storysDir := filepath.Join(progDir, "storys")
	os.MkdirAll(storysDir, 0755)

	// ---------- 结构化日志（分级 + 文件持久化 + 按天轮转） ----------
	lx := logx.Get(progDir)
	lx.Info("系统", "WorldSim 启动，版本=%s 程序目录=%s", version, progDir)

	// ---------- 世界模拟目录 ----------
	worldDir := filepath.Join(progDir, "worlds")
	os.MkdirAll(worldDir, 0755)

	// ---------- API 配置（小说化服务共用） ----------
	apiCfgPath := filepath.Join(progDir, "api.json")
	apiCfg, err := config.LoadAPIConfig(apiCfgPath)
	if err != nil {
		lx.Error("系统", "加载API配置失败: %v", err)
		os.Exit(1)
	}
	llm.EnsureContextBudget(apiCfg)
	if apiCfg.BaseURL == "" || apiCfg.Model == "" {
		lx.Warn("系统", "检测到空白API配置，已自动生成 api.json（请在 Web UI 配置）")
	}

	// ---------- 启动小说化服务（48090） ----------
	logger := sse.NewLogBroadcaster()
	defer logger.Close()

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("嵌入静态文件失败: %v", err)
	}
	go httpapi.StartWebServer(apiCfg, apiCfgPath, logger, storyPort, progDir, version, staticFS)
	lx.Info("系统", "小说创作服务已启动: http://localhost%s", storyPort)

	// ---------- 启动世界模拟服务（48091） ----------
	go startWorldServer(worldDir, apiCfg)
	lx.Info("系统", "世界模拟服务已启动: http://localhost%s", worldPort)

	// ---------- 运行检测 + 自动修复 ----------
	hc := health.New(progDir)
	// 注入自动修复处理器：LLM 连续失败时输出修复建议（保持简单，后续可扩展为模型降级切换）
	health.SetAutoHealHandler(func(reason string) string {
		lx.Warn("自愈", "执行自动修复：%s", reason)
		return "已记录修复动作；建议检查中转站限流或切换模型（api.json）"
	})
	hc.Start()
	lx.Info("系统", "运行检测已启动: /api/health、/api/logs、heartbeat.json")

	lx.Info("系统", "程序目录: %s", progDir)
	lx.Info("系统", "小说项目目录: %s", storysDir)
	lx.Info("系统", "世界模拟目录: %s", worldDir)

	select {} // 阻塞主协程
}

func resolveProgDir() string {
	if len(os.Args) > 1 {
		absDir, err := filepath.Abs(os.Args[1])
		if err == nil {
			if info, err := os.Stat(absDir); err == nil && info.IsDir() {
				return absDir
			}
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Printf(" [错误] 无法获取当前目录: %v\n", err)
		os.Exit(1)
	}
	return cwd
}

// ---------- 世界模拟 HTTP 服务（多世界支持） ----------

// worldInstance 单个世界实例（独立数据目录：worlds/{名字}/）
type worldInstance struct {
	name    string
	dir     string
	engine  *engine.StateEngine
	sim     *sim.Simulator
	llm     *sim.LLMClient
	wb      *worldbook.Worldbook
	novelW  *novel.Writer
	apiCfg  *config.APIConfig
	heroName string // 主角名（小说写手必须用模拟主角名）
	created bool // 是否已初始化世界状态（主角等）
	lastDay *sim.DayResult // 最近一次模拟结果（手动跑天/后台循环都会更新，供"今日对话/事件"面板）
}

func (w *worldInstance) ready() bool { return w != nil && w.engine != nil }

func startWorldServer(worldDir string, apiCfg *config.APIConfig) {
	ws := &worldServer{baseDir: worldDir, apiCfg: apiCfg, worlds: map[string]*worldInstance{}}
	ws.scanWorlds()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/worlds", ws.handleWorldsList)
	mux.HandleFunc("POST /api/worlds/select", ws.handleWorldSelect)
	mux.HandleFunc("POST /api/worlds/create", ws.handleWorldCreate)
	mux.HandleFunc("GET /api/world/state", ws.handleGetState)
	mux.HandleFunc("POST /api/world/proposal", ws.handleProposal)
	mux.HandleFunc("POST /api/world/init", ws.handleInit)
	mux.HandleFunc("GET /api/world/log", ws.handleLog)
	mux.HandleFunc("GET /api/world/replay", ws.handleReplay)
	mux.HandleFunc("POST /api/world/sim/day", ws.handleSimDay)
	mux.HandleFunc("GET /api/world/chronicle", ws.handleChronicle)
	mux.HandleFunc("POST /api/world/sim/llm", ws.handleSetLLM)
	mux.HandleFunc("GET /api/world/sim/thinking", ws.handleThinking)
	mux.HandleFunc("GET /api/world/memories", ws.handleMemories)
	mux.HandleFunc("GET /api/world/decisions", ws.handleDecisions)
	mux.HandleFunc("POST /api/world/decisions/{id}", ws.handleDecisionResolve)
	mux.HandleFunc("GET /api/world/readiness", ws.handleReadiness)
	mux.HandleFunc("GET /api/world/token_stats", ws.handleTokenStats)
	mux.HandleFunc("POST /api/world/novel/generate", ws.handleNovelGenerate)
	mux.HandleFunc("GET /api/world/novel", ws.handleNovelList)
	mux.HandleFunc("GET /api/world/novel/chapter/{num}", ws.handleNovelChapter)

	// 控制台：主题包列表 / 世界书 / 伏笔 / 后台循环
	mux.HandleFunc("GET /api/worldbooks/themes", ws.handleThemesList)
	mux.HandleFunc("GET /api/world/worldbook", ws.handleGetWorldbook)
	mux.HandleFunc("GET /api/world/foreshadows", ws.handleForeshadows)
	mux.HandleFunc("GET /api/world/today", ws.handleToday)
	mux.HandleFunc("POST /api/world/loop", ws.handleLoopSet)
	mux.HandleFunc("GET /api/world/loop", ws.handleLoopStatus)

	// 时间回退：快照列表 / 手动存档 / 回退
	mux.HandleFunc("GET /api/world/snapshots", ws.handleSnapshots)
	mux.HandleFunc("POST /api/world/snapshot", ws.handleSnapshot)
	mux.HandleFunc("POST /api/world/rewind", ws.handleRewind)

	// 运行检测：健康检查 / 日志查看（后期检查与改进用）
	hc := health.New(filepath.Dir(ws.baseDir)) // ws.baseDir=worlds目录，日志写 wsdata（上级）
	mux.HandleFunc("GET /api/health", hc.HandleHealth)
	mux.HandleFunc("GET /api/logs", hc.HandleLogs)

	// WebUI 世界模拟面板（单文件前端）
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := wsWeb.ReadFile("wsweb/index.html")
		if err != nil {
			http.Error(w, "前端加载失败", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	if err := http.ListenAndServe(worldPort, mux); err != nil {
		log.Fatalf(" [世界模拟] 服务启动失败: %v", err)
	}
}

// 多世界：worldServer 持有世界实例池
type worldServer struct {
	baseDir string // worlds/
	worlds  map[string]*worldInstance
	current string // 当前世界名
	apiCfg  *config.APIConfig
	novelMu sync.Mutex // 小说生成防重入锁（并发请求会写重复章号）

	loopMu      sync.Mutex    // 后台持续运行控制
	loopRunning bool          // 循环是否在跑
	loopCancel  context.CancelFunc
	loopTarget  int           // 目标 day（世界时间）
	loopWorld   string        // 循环绑定的世界名
}

// handleThemesList GET /api/worldbooks/themes — 主题包列表（建世界下拉用）
func (ws *worldServer) handleThemesList(w http.ResponseWriter, r *http.Request) {
	themesDir := filepath.Join(ws.baseDir, "..", "worldbooks", "themes")
	entries, err := os.ReadDir(themesDir)
	if err != nil {
		ws.writeJSON(w, 200, map[string]any{"themes": []string{}})
		return
	}
	themes := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			themes = append(themes, strings.TrimSuffix(e.Name(), ".md"))
		}
	}
	ws.writeJSON(w, 200, map[string]any{"themes": themes})
}

// handleGetWorldbook GET /api/world/worldbook — 当前世界书全文（查看/校对用）
func (ws *worldServer) handleGetWorldbook(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可用世界"})
		return
	}
	wbPath := filepath.Join(ws.baseDir, "..", "worldbooks", inst.name+".md")
	data, err := os.ReadFile(wbPath)
	if err != nil {
		ws.writeJSON(w, 200, map[string]any{"name": inst.name, "content": "（世界书不存在）"})
		return
	}
	ws.writeJSON(w, 200, map[string]any{"name": inst.name, "content": string(data)})
}

// handleForeshadows GET /api/world/foreshadows — 未回收伏笔清单（防忘坑）
func (ws *worldServer) handleForeshadows(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil || inst.sim == nil {
		ws.writeJSON(w, 200, map[string]any{"foreshadows": ""})
		return
	}
	ws.writeJSON(w, 200, map[string]any{"foreshadows": inst.sim.OpenForeshadows()})
}

// handleLoopSet POST /api/world/loop — 后台持续运行（不再依赖外部脚本）
// body: {"action":"start","days":500,"mode":"auto"} 或 {"action":"stop"}
func (ws *worldServer) handleLoopSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
		Days   int    `json:"days"`
		Mode   string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "参数错误"})
		return
	}
	ws.loopMu.Lock()
	defer ws.loopMu.Unlock()
	if req.Action == "stop" {
		if ws.loopCancel != nil {
			ws.loopCancel()
		}
		ws.loopRunning = false
		ws.writeJSON(w, 200, map[string]any{"ok": true, "running": false})
		return
	}
	if ws.loopRunning {
		ws.writeJSON(w, 200, map[string]any{"ok": false, "error": "循环已在运行（先 stop）", "running": true})
		return
	}
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可用世界"})
		return
	}
	if inst.sim == nil {
		inst.sim = sim.NewSimulator(inst.engine, inst.dir)
		if inst.heroName != "" {
			inst.sim.SetHeroName(inst.heroName)
		}
		inst.applyLLM()
	}
	if req.Days <= 0 {
		req.Days = 1000
	}
	if req.Days > 36500 {
		req.Days = 36500
	}
	if req.Mode != "" {
		inst.sim.SetMode(req.Mode)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ws.loopCancel = cancel
	ws.loopRunning = true
	ws.loopTarget = inst.engine.State().Day + req.Days
	ws.loopWorld = inst.name
	go func() {
		defer func() {
			ws.loopMu.Lock()
			ws.loopRunning = false
			ws.loopCancel = nil
			ws.loopMu.Unlock()
		}()
		// 防空转：连续 dry-run（LLM 连不上导致事件生成失败走模板）计数
		// 超过阈值自动回退到最近健康快照，避免"空转污染"世界（干跑模板事件破坏剧情）
		consecDryRun := 0
		const maxDryRunBeforeRewind = 5
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if res, err := inst.sim.RunDay(ctx); err != nil {
				// 单日失败不中断循环（中转站抖动/超时），但停一会儿再试
				time.Sleep(1 * time.Second)
				continue
			} else {
				inst.lastDay = res // 供前端"今日对话/事件"面板
			}
			inst.autoSnapshot() // 每7天自动存档（时间回退锚点）

			// --- 防空转检测：事件生成是否走 dry-run ---
			snap := logx.M().Snapshot()
			// 检查本次 RunDay 是否 dry-run：用全局指标 last_sim_ok（最近一次 LLM 是否成功）
			lastOK, _ := snap["last_sim_ok"].(bool)
			llmCalls, _ := snap["llm_calls"].(int64)
			if !lastOK && llmCalls > 0 {
				consecDryRun++
				if consecDryRun == 1 {
					// 第一次失败：先存一个"风险前健康快照"，万一后面连不上，至少有干净锚点
					inst.snapshotBeforeRisk("LLM抖动·风险前存档")
				}
				if consecDryRun >= maxDryRunBeforeRewind {
					fmt.Printf(" [防空转] LLM 连续失败 %d 次，自动回退到最近健康快照（防止空转污染）\n", consecDryRun)
					ws.autoRewindSafe(inst, "LLM连续失败自动回档")
					consecDryRun = 0
				}
			} else {
				consecDryRun = 0 // LLM 正常，重置计数
			}

			// 就绪度驱动：素材够了自动停，等用户看小说
			if rdy, ok := inst.sim.Readiness()["ready"].(bool); ok && rdy {
				return
			}
			if inst.engine.State().Day >= ws.loopTarget {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	ws.writeJSON(w, 200, map[string]any{"ok": true, "running": true, "target_day": ws.loopTarget, "world": ws.loopWorld})
}

// handleLoopStatus GET /api/world/loop — 循环状态（前端开关/进度条用）
func (ws *worldServer) handleLoopStatus(w http.ResponseWriter, r *http.Request) {
	ws.loopMu.Lock()
	defer ws.loopMu.Unlock()
	day := 0
	ready := false
	if inst := ws.worlds[ws.loopWorld]; inst != nil && inst.sim != nil {
		day = inst.engine.State().Day
		if rdy, ok := inst.sim.Readiness()["ready"].(bool); ok {
			ready = rdy
		}
	}
	ws.writeJSON(w, 200, map[string]any{
		"running":    ws.loopRunning,
		"world":      ws.loopWorld,
		"day":        day,
		"target_day": ws.loopTarget,
		"ready":      ready,
	})
}

// autoSnapshot 自动快照：每 7 天存一次（时间回退锚点）+ 手动存档
// 频率提升：30 天太长，LLM 连不上空转几天后想回档会没有干净锚点
func (w *worldInstance) autoSnapshot() {
	if w.sim == nil {
		return
	}
	day := w.engine.State().Day
	if day%7 == 0 {
		if _, err := w.sim.SaveSnapshot(fmt.Sprintf("自动·每7天·Day%d", day)); err == nil {
			fmt.Printf(" [快照] Day%d 自动存档（时间回退锚点）\n", day)
		}
	}
}

// snapshotBeforeRisk 在"可能出问题"前存一个健康快照：
// LLM 连续失败（中转站抖动）时，把当前状态固化为可回退锚点——防止空转污染后无处可回
func (w *worldInstance) snapshotBeforeRisk(reason string) {
	if w.sim == nil {
		return
	}
	if _, err := w.sim.SaveSnapshot(reason); err == nil {
		fmt.Printf(" [快照] 风险前自动存档：%s（Day%d）\n", reason, w.engine.State().Day)
	}
}

// handleSnapshots GET /api/world/snapshots — 时间回退锚点列表（最新在前）
func (ws *worldServer) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil || inst.sim == nil {
		ws.writeJSON(w, 200, map[string]any{"snapshots": []sim.SnapshotMeta{}})
		return
	}
	ws.writeJSON(w, 200, map[string]any{"snapshots": inst.sim.Snapshots()})
}

// handleSnapshot POST /api/world/snapshot — 手动存档（body: {"reason":"..."}）
func (ws *worldServer) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil || inst.sim == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "模拟器未初始化"})
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	meta, err := inst.sim.SaveSnapshot(req.Reason)
	if err != nil {
		ws.writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	ws.writeJSON(w, 200, map[string]any{"ok": true, "snapshot": meta})
}

// autoRewindSafe 防空转自动回档：回退到最近健康快照（LLM 连不上时空转后恢复干净状态）
// 回退后重建 Simulator + 刷新内存状态；若没有快照则只记录（无法回退）
func (ws *worldServer) autoRewindSafe(inst *worldInstance, reason string) {
	if inst == nil || inst.sim == nil {
		return
	}
	// 回退到"当前 day 之前最近"的健康快照（不要回退到当天，当天可能已污染）
	target := inst.engine.State().Day - 1
	if target <= 0 {
		target = 1
	}
	meta, err := inst.sim.RewindTo(target)
	if err != nil {
		fmt.Printf(" [防空转] 无可用健康快照可回退（%v），继续当前状态\n", err)
		return
	}
	// 重建 Simulator + 刷新内存
	if err := inst.engine.Load(filepath.Join(inst.dir, "world_state.json")); err != nil {
		fmt.Printf(" [防空转] 回退后状态加载失败: %v\n", err)
		return
	}
	inst.sim = sim.NewSimulator(inst.engine, inst.dir)
	inst.heroName = inst.sim.HeroName()
	inst.applyLLM()
	fmt.Printf(" [防空转] 已自动回档到 Day%d（原因：%s）\n", meta.Day, reason)
}

// handleRewind POST /api/world/rewind — 时间回退到 ≤day 的最近快照（body: {"day":N}）
// 回退 = 恢复该日完整状态（世界/编年史/记忆/决策/伏笔/段落），之后从该点重新演化（新分支）
func (ws *worldServer) handleRewind(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil || inst.sim == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "模拟器未初始化"})
		return
	}
	var req struct {
		Day int `json:"day"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Day <= 0 {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "需要 day 字段（>0）"})
		return
	}
	// 先停循环，避免回退过程中写入
	ws.loopMu.Lock()
	if ws.loopCancel != nil {
		ws.loopCancel()
	}
	ws.loopRunning = false
	ws.loopCancel = nil
	ws.loopMu.Unlock()

	meta, err := inst.sim.RewindTo(req.Day)
	if err != nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 刷新 engine 内存状态 + 重建 Simulator（重新加载被回退的状态文件）
	if err := inst.engine.Load(filepath.Join(inst.dir, "world_state.json")); err != nil {
		ws.writeJSON(w, 500, map[string]any{"ok": false, "error": "状态刷新失败: " + err.Error()})
		return
	}
	inst.sim = sim.NewSimulator(inst.engine, inst.dir)
	inst.heroName = inst.sim.HeroName()
	inst.applyLLM()
	fmt.Printf(" [回退] 「%s」已回退到 Day%d（rev%d，快照：%s）\n", inst.name, meta.Day, meta.Revision, meta.Reason)
	ws.writeJSON(w, 200, map[string]any{
		"ok": true, "rewound_to": meta.Day, "revision": meta.Revision,
		"reason": meta.Reason, "snapshot": meta,
		"hint": "已回退到该时间点，可重新启动循环继续演化（会走出新分支）",
	})
}

// inst 返回当前世界实例（无则报错）
func (ws *worldServer) inst() *worldInstance {
	if ws.current == "" {
		return nil
	}
	return ws.worlds[ws.current]
}

// scanWorlds 扫描 worlds/ 下已有世界目录并加载
func (ws *worldServer) scanWorlds() {
	entries, err := os.ReadDir(ws.baseDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			w := ws.newInstance(e.Name())
			if w.ready() {
				ws.worlds[e.Name()] = w
				if ws.current == "" {
					ws.current = e.Name()
				}
			}
		}
	}
	fmt.Printf(" [世界模拟] 发现 %d 个世界：%v\n", len(ws.worlds), ws.current)
}

// newInstance 创建世界实例（加载 engine/sim/记忆/小说写手）
func (ws *worldServer) newInstance(name string) *worldInstance {
	dir := filepath.Join(ws.baseDir, name)
	w := &worldInstance{name: name, dir: dir, apiCfg: ws.apiCfg}

	// 加载世界书（worlds/../worldbooks/ 池）
	wbPath := filepath.Join(ws.baseDir, "..", "worldbooks", name+".md")
	if _, err := os.Stat(wbPath); err != nil {
		// 兜底：默认世界书
		wbPath = filepath.Join(ws.baseDir, "..", "worldbooks", "临江市·都市怪谈.md")
	}
	if wb, err := worldbook.Load(wbPath); err == nil {
		w.wb = wb
	} else {
		// 尝试世界目录内
		if wb2, err2 := worldbook.Load(filepath.Join(dir, "worldbook.md")); err2 == nil {
			w.wb = wb2
		}
	}

	// State Engine
	rules, err := engine.LoadRules(filepath.Join(dir, "rules.json"))
	if err != nil {
		rules = engine.DefaultRules()
	}
	se := engine.NewStateEngine(rules, filepath.Join(dir, "event_log.jsonl"))
	se.SetSoftValidator(func(ctx context.Context, p *engine.Proposal, s *engine.WorldState) error {
		return nil // GM 软规则在配置 LLM 后替换
	})
	if err := se.Load(filepath.Join(dir, "world_state.json")); err == nil {
		fmt.Printf(" [世界模拟] 「%s」已加载状态（revision=%d, day=%d）\n", name, se.State().Revision, se.State().Day)
	}
	w.engine = se

	// 断点续传：已有状态则预创建 Simulator
	if se.State().Revision > 0 {
		w.sim = sim.NewSimulator(se, dir)
		w.heroName = w.sim.HeroName() // 恢复主角名（role标记识别，防视角漂移）
	}

	// 小说化写手（放世界自己的目录：worlds/{世界名}/novel/——世界的一切产物内聚，最好找）
	bookDir := filepath.Join(w.dir, "novel")
	w.novelW = novel.NewWriter(ws.apiCfg, name+"·小说", bookDir, w.wb, w.heroName)
	// 自动启用 LLM（api.json 配置有效时）——重启/新建世界不用再手动配置
	ws.autoEnableLLM(w)
	return w
}

// applyLLM 把 inst.llm 应用到 Simulator：世界书 + EnableLLM + GM 软规则
func (w *worldInstance) applyLLM() {
	if w.llm == nil || w.sim == nil {
		return
	}
	if w.wb != nil {
		w.sim.SetWorldbook(w.wb)
	}
	w.sim.EnableLLM(w.llm)
	// 软规则：GM 用 LLM 裁决（§1.1），世界书 A2 作为裁决依据；无 LLM 时硬约束仍生效
	client := w.llm
	worldRule := "现实都市规则；存在超自然现象但普通人看不见；身体有极限；法律与现实社会规则有效。"
	if w.wb != nil {
		worldRule = w.wb.WorldRule()
	}
	w.engine.SetSoftValidator(func(ctx context.Context, p *engine.Proposal, st *engine.WorldState) error {
		if p.ActorID != "protagonist" {
			return nil // 性能优化：GM 只裁决主角行动提案，内部提案走硬约束
		}
		return sim.GMJudgeLLM(ctx, client, st, p, worldRule)
	})
}

// autoEnableLLM 从全局 apiCfg 自动启用 LLM（api.json 有效时）
func (ws *worldServer) autoEnableLLM(w *worldInstance) {
	if ws.apiCfg == nil || ws.apiCfg.BaseURL == "" || ws.apiCfg.Model == "" {
		return
	}
	w.llm = &sim.LLMClient{Cfg: &config.APIConfig{
		BaseURL:    ws.apiCfg.BaseURL,
		Model:      ws.apiCfg.Model,
		APIKey:     ws.apiCfg.APIKey,
		ModelTiers: ws.apiCfg.ModelTiers,
	}}
	w.applyLLM()
}

func (ws *worldServer) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// GET /api/world/state — 查看世界状态（旁观权限，§16.1）
func (ws *worldServer) handleGetState(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可用世界，请先创建"})
		return
	}

	ws.writeJSON(w, 200, inst.engine.State())
}

// POST /api/world/proposal — 提交状态变更提案（唯一写路径，§1.1）
func (ws *worldServer) handleProposal(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可用世界，请先创建"})
		return
	}

	var p engine.Proposal
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		ws.writeJSON(w, 400, map[string]string{"error": "提案JSON解析失败: " + err.Error()})
		return
	}
	if p.CommandID == "" || p.ActorID == "" {
		ws.writeJSON(w, 400, map[string]string{"error": "command_id 与 actor_id 必填"})
		return
	}
	if err := inst.engine.Submit(r.Context(), &p); err != nil {
		ws.writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	if err := inst.engine.Save(filepath.Join(inst.dir, "world_state.json")); err != nil {
		log.Printf(" [世界模拟] 保存状态失败: %v", err)
	}
	ws.writeJSON(w, 200, map[string]any{
		"ok":       true,
		"revision": inst.engine.State().Revision,
	})
}

// POST /api/world/init — 世界初始化（§14 Step2：建世界 + 主角 + 事件种子）
func (ws *worldServer) handleInit(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可用世界，请先创建"})
		return
	}

	var req struct {
		WorldName   string `json:"world_name"`
		City        string `json:"city"`
		Weather     string `json:"weather"`
		Protagonist string `json:"protagonist"` // 主角名
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ws.writeJSON(w, 400, map[string]string{"error": "初始化请求解析失败: " + err.Error()})
		return
	}
	if req.Weather == "" {
		req.Weather = "晴"
	}
	base := inst.engine.State().Revision
	// 世界初始化 Agent（按世界书生成主角/NPC/地点——一切由世界书驱动）
	// 独立 context：客户端断连不影响初始化完成
	initCtx, initCancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer initCancel()
	plan := sim.WorldInitPlanLLM(initCtx, inst.llm, inst.wb)
	hero := req.Protagonist
	if plan != nil {
		hero = plan.Protagonist.Name
		if req.Protagonist != "" {
			hero = req.Protagonist // 调用方显式指定主角名时优先
		}
		inst.heroName = hero
		if inst.sim != nil {
			inst.sim.SetHeroName(hero)
		}
		if inst.novelW != nil {
			inst.novelW.SetHeroName(hero)
		}
		fmt.Printf(" [世界模拟] 初始化方案（按世界书生成）：%s\n", plan.String())
	}
	proposals := []engine.Proposal{}
	if plan != nil {
		proposals = append(proposals, engine.Proposal{
			CommandID:    "init-1",
			ActorID:      "world_agent",
			BaseRevision: base,
			Type:         "state_change",
			Changes:      plan.Changes(hero),
			Reason:       "世界初始化（按世界书生成）",
		})
		// 清理 fallback 残留：之前若因 LLM 失败走过占位模板（主角名="主角"），
		// 现在 LLM 成功生成了真实主角，删掉占位实体，避免"双主角"污染
		if hero != "主角" {
			if _, exists := inst.engine.State().Entities["主角"]; exists {
				proposals = append(proposals, engine.Proposal{
					CommandID:    "init-clean",
					ActorID:      "world_agent",
					BaseRevision: base + 1,
					Type:         "state_change",
					Changes:      []engine.Change{{Path: "entities.主角.", Op: "del"}},
					Reason:       "清理 LLM 失败时生成的占位主角",
				})
			}
		}
	} else {
		// fallback：LLM 不可用/失败 → 最小化通用模板（只建主角骨架，内容由世界书驱动）
		// 注意：不做任何世界特定预设——主角名/职业/地点用占位，NPC 留待后续事件自然引入
		if hero == "" {
			hero = "主角" // 通用占位名；LLM 可用时会被世界书生成的真实主角名替换
		}
		inst.heroName = hero
		if inst.sim != nil {
			inst.sim.SetHeroName(hero)
		}
		if inst.novelW != nil {
			inst.novelW.SetHeroName(hero)
		}
		fmt.Printf(" [世界模拟] 初始化方案：LLM 不可用，用最小通用模板（主角=%s）\n", hero)
		proposals = append(proposals, engine.Proposal{
			CommandID: "init-1", ActorID: "world_agent", BaseRevision: base, Type: "state_change",
			Changes: []engine.Change{
				{Path: "world_level.global_events", Op: "add", Value: "世界开始运转：" + req.City},
				{Path: "world_level.tension", Op: "set", Value: 0.2},
			}, Reason: "世界初始化"},
		)
		// 主角实体（仅基础字段；身份/地点/属性由世界书驱动，LLM 可用时自动补齐）
		proposals = append(proposals, engine.Proposal{
			CommandID:    "init-2",
			ActorID:      "world_agent",
			BaseRevision: base + 1,
			Type:         "state_change",
			Changes: []engine.Change{
				{Path: "entities." + hero + ".location", Op: "set", Value: "出生地"},
				{Path: "entities." + hero + ".money", Op: "set", Value: 0},
				{Path: "entities." + hero + ".health", Op: "set", Value: 90},
				{Path: "entities." + hero + ".job", Op: "set", Value: "普通人"},
				{Path: "entities." + hero + ".alive", Op: "set", Value: true},
				{Path: "entities." + hero + ".status", Op: "set", Value: "active"},
				{Path: "entities." + hero + ".extra.role", Op: "set", Value: "protagonist"},
			},
			Reason: "主角诞生",
		})
		// 不预设常驻 NPC：配角由世界书（A3/A4）与事件自然引入，避免套用别的世界的模板
	}

	var lastErr error
	// 独立 context：客户端断开（宿主 http_request 超时）不影响初始化提交
	submitCtx, submitCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer submitCancel()
	// CommandID 加唯一后缀（幂等去重：init-1 被 fallback 用过会静默跳过，导致 LLM 方案不落库）
	uniq := time.Now().UnixNano() % 100000
	for i := range proposals {
		proposals[i].CommandID = fmt.Sprintf("%s-%d", proposals[i].CommandID, uniq)
	}
	for _, p := range proposals {
		if err := inst.engine.Submit(submitCtx, &p); err != nil {
			lastErr = err
			break
		}
	}
	if lastErr != nil {
		ws.writeJSON(w, 409, map[string]string{"error": lastErr.Error()})
		return
	}
	inst.engine.State().Day = 1
	if err := inst.engine.Save(filepath.Join(inst.dir, "world_state.json")); err != nil {
		log.Printf(" [世界模拟] 保存状态失败: %v", err)
	}
	// 初始化后立即可用：创建 Simulator（加载空编年史）
	if inst.sim == nil {
		inst.sim = sim.NewSimulator(inst.engine, inst.dir)
		inst.sim.SetHeroName(hero)
	}
	inst.applyLLM() // 自动启用 LLM（api.json 配置有效时）——init 后模拟立即走真实 Agent
	inst.created = true
	ws.writeJSON(w, 200, map[string]any{
		"ok":          true,
		"world":       req.WorldName,
		"revision":    inst.engine.State().Revision,
		"day":         inst.engine.State().Day,
		"protagonist": req.Protagonist,
	})
}

// GET /api/world/log — 查看事件日志（全部提交记录）
func (ws *worldServer) handleLog(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可用世界，请先创建"})
		return
	}

	b, err := os.ReadFile(filepath.Join(inst.dir, "event_log.jsonl"))
	if err != nil {
		ws.writeJSON(w, 404, map[string]string{"error": "无事件日志"})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(b)
}

// GET /api/world/replay?revision=N — 事件溯源重放（§17.1）
func (ws *worldServer) handleReplay(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可用世界，请先创建"})
		return
	}

	target := 0
	fmt.Sscanf(r.URL.Query().Get("revision"), "%d", &target)
	state, err := engine.Replay(filepath.Join(inst.dir, "event_log.jsonl"), target, nil)
	if err != nil {
		ws.writeJSON(w, 500, map[string]string{"error": "重放失败: " + err.Error()})
		return
	}
	ws.writeJSON(w, 200, map[string]any{
		"replayed_to": target,
		"state":       state,
	})
}

// POST /api/world/sim/day — 跑一天模拟（§5：mode 支持 auto/scene/summary/skip）
func (ws *worldServer) handleSimDay(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可用世界，请先创建"})
		return
	}

	var req struct {
		Days int    `json:"days"` // 默认1，可连跑多天
		Mode string `json:"mode"` // 张力引擎：auto(自适应) | scene | summary | skip
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Days <= 0 {
		req.Days = 1
	}
	if req.Days > 30 {
		req.Days = 30 // 单次上限，防误操作
	}
	if inst.sim == nil {
		inst.sim = sim.NewSimulator(inst.engine, inst.dir)
		if inst.heroName != "" {
			inst.sim.SetHeroName(inst.heroName)
		}
	}
	if req.Mode != "" {
		inst.sim.SetMode(req.Mode)
	}
	results := []*sim.DayResult{}
	// 独立 context：客户端断开（宿主 http_request 超时）不影响模拟继续完成
	runCtx, runCancel := context.WithTimeout(context.Background(), time.Duration(req.Days)*170*time.Second)
	defer runCancel()
	for i := 0; i < req.Days; i++ {
		res, err := inst.sim.RunDay(runCtx)
		if err != nil {
			ws.writeJSON(w, 500, map[string]string{"error": "模拟失败: " + err.Error()})
			return
		}
		inst.autoSnapshot() // 每7天自动存档（时间回退锚点）+ 风险前存档
		results = append(results, res)
		inst.lastDay = res // 供前端"今日对话/事件"面板（页面刷新/定时刷新直接拉取）
		if res.Paused {
			break // 遇到抉择点暂停（§10）
		}
	}
	ws.writeJSON(w, 200, map[string]any{
		"ok":        true,
		"results":   results,
		"paused":    results[len(results)-1].Paused,
		"revision":  inst.engine.State().Revision,
		"day":       inst.engine.State().Day,
		"cache":     llm.CacheStats(),
	})
}

// GET /api/world/today — 最近一天的模拟结果（今日对话/今日事件），供前端面板
// 手动跑天和后台循环都会更新 inst.lastDay；无数据时返回空（前端显示"无"）
func (ws *worldServer) handleToday(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可用世界，请先创建"})
		return
	}
	if inst.lastDay == nil {
		ws.writeJSON(w, 200, map[string]any{"ok": true, "day": 0, "events": []any{}, "dialogue": []any{}})
		return
	}
	ws.writeJSON(w, 200, map[string]any{
		"ok":       true,
		"day":      inst.lastDay.Day,
		"events":   inst.lastDay.Events,
		"dialogue": inst.lastDay.Dialogue,
	})
}

// GET /api/world/chronicle — 查看编年史（§9.10）
func (ws *worldServer) handleChronicle(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可用世界，请先创建"})
		return
	}

	if inst.sim == nil {
		ws.writeJSON(w, 404, map[string]string{"error": "尚未运行模拟"})
		return
	}
	ws.writeJSON(w, 200, map[string]any{
		"chronicle": inst.sim.Chronicle(),
	})
}

// POST /api/world/sim/llm — 配置 LLM Agent（mock=本地模拟测试 / real=真实API）
// body: {"mode":"mock"} 或 {"mode":"real","base_url":"https://api.deepseek.com","model":"deepseek-chat","api_key":"sk-..."}
func (ws *worldServer) handleSetLLM(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可用世界，请先创建"})
		return
	}

	var req struct {
		Mode        string            `json:"mode"` // mock | real | off
		BaseURL     string            `json:"base_url"`
		Model       string            `json:"model"`
		APIKey      string            `json:"api_key"`
		ModelTiers  map[string]string `json:"model_tiers"` // 模型分层：fast/normal/premium
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ws.writeJSON(w, 400, map[string]string{"error": "请求解析失败: " + err.Error()})
		return
	}
	switch req.Mode {
	case "mock":
		inst.llm = sim.NewMockLLM()
	case "real":
		if req.BaseURL == "" || req.Model == "" {
			ws.writeJSON(w, 400, map[string]string{"error": "real 模式需要 base_url 与 model"})
			return
		}
		// 模型分层：优先用请求里的 tiers；没有则保留旧的；再没有则用默认分层
		tiers := req.ModelTiers
		if len(tiers) == 0 && inst.llm != nil && inst.llm.Cfg != nil {
			tiers = inst.llm.Cfg.ModelTiers
		}
		if len(tiers) == 0 {
			tiers = map[string]string{
				"fast":    "deepseek-v4-flash-0731",
				"normal":  "deepseek-v4-pro",
				"premium": "deepseek-v4-pro",
			}
		}
		inst.llm = &sim.LLMClient{Cfg: &config.APIConfig{
			BaseURL:    req.BaseURL,
			Model:      req.Model,
			APIKey:     req.APIKey,
			ModelTiers: tiers,
		}}
		// 持久化到 api.json（重启后小说/模拟分层不丢）
		if b, err := json.MarshalIndent(inst.llm.Cfg, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(ws.baseDir, "..", "api.json"), b, 0644)
		}
	case "off":
		inst.llm = nil
	default:
		ws.writeJSON(w, 400, map[string]string{"error": "mode 需为 mock|real|off"})
		return
	}
	// 创建/复用 Simulator 并启用 LLM + GM 软规则
	if inst.sim == nil {
		inst.sim = sim.NewSimulator(inst.engine, inst.dir)
	}
	inst.applyLLM()
	ws.writeJSON(w, 200, map[string]any{"ok": true, "mode": req.Mode, "llm_ready": inst.llm != nil})
}

// GET /api/world/sim/thinking — 查看主角最近一次三问推理（§8.4）
func (ws *worldServer) handleThinking(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可用世界，请先创建"})
		return
	}

	if inst.sim == nil {
		ws.writeJSON(w, 404, map[string]string{"error": "尚未运行模拟"})
		return
	}
	ws.writeJSON(w, 200, map[string]any{"thinking": inst.sim.LastThinking()})
}

// ---------- 多世界管理 ----------

// GET /api/worlds — 列出所有世界 + 当前世界
func (ws *worldServer) handleWorldsList(w http.ResponseWriter, r *http.Request) {
	type worldInfo struct {
		Name    string `json:"name"`
		Day     int    `json:"day"`
		Active  bool   `json:"active"`
		HasData bool   `json:"has_data"`
	}
	var list []worldInfo
	names := make([]string, 0, len(ws.worlds))
	for n := range ws.worlds {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w := ws.worlds[n]
		day := 0
		hasData := false
		if w.engine != nil {
			day = w.engine.State().Day
			hasData = w.engine.State().Revision > 0
		}
		list = append(list, worldInfo{Name: n, Day: day, Active: n == ws.current, HasData: hasData})
	}
	ws.writeJSON(w, 200, map[string]any{
		"worlds":  list,
		"current": ws.current,
	})
}

// POST /api/worlds/select — 切换当前世界
func (ws *worldServer) handleWorldSelect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		World string `json:"world"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.World == "" {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "需要 world 字段"})
		return
	}
	if _, ok := ws.worlds[req.World]; !ok {
		ws.writeJSON(w, 404, map[string]any{"ok": false, "error": "世界不存在: " + req.World})
		return
	}
	ws.current = req.World
	ws.writeJSON(w, 200, map[string]any{"ok": true, "current": ws.current})
}

// POST /api/worlds/create — 创建新世界（目录 + 实例）
// body: {"name":"...", "worldbook":"世界书名（可选）", "theme":"主题包名（可选：经典修仙/都市异能/克苏鲁异界…）", "desc":"一句话设定（theme 模式下可选，不传则 LLM 按主题包自拟）"}
func (ws *worldServer) handleWorldCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		Worldbook string `json:"worldbook"` // 世界书名（worldbooks/ 池），可选
		Theme     string `json:"theme"`     // 主题包名（worldbooks/themes/ 池），可选
		Desc      string `json:"desc"`      // 一句话设定（theme 模式用）
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "需要 name 字段"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if _, ok := ws.worlds[name]; ok {
		ws.writeJSON(w, 409, map[string]any{"ok": false, "error": "世界已存在: " + name})
		return
	}
	dir := filepath.Join(ws.baseDir, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		ws.writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 写入默认 rules.json
	if _, err := os.Stat(filepath.Join(dir, "rules.json")); err != nil {
		rb, _ := json.MarshalIndent(engine.DefaultRules(), "", "  ")
		_ = os.WriteFile(filepath.Join(dir, "rules.json"), rb, 0644)
	}
	wbName := req.Worldbook
	// 主题包模式：LLM 按主题包 + 用户设定自动生成世界书（一劳永逸：选主题 → 世界书 → 世界）
	if wbName == "" && req.Theme != "" {
		themePath := filepath.Join(ws.baseDir, "..", "worldbooks", "themes", req.Theme+".md")
		if themeContent, err := os.ReadFile(themePath); err == nil {
			desc := req.Desc
			if desc == "" {
				desc = "按主题包的典型设定生成一个有生活质感的世界（主角从最底层开始，逐步成长）"
			}
			// 独立 context（不随 HTTP 请求取消）+ 重试3次（中转站长请求偶发 connection reset）
			var wbText string
			for attempt := 0; attempt < 3; attempt++ {
				genCtx, genCancel := context.WithTimeout(context.Background(), 240*time.Second)
				var gerr error
				wbText, gerr = worldbook.GenWorldbookLLM(genCtx, ws.apiCfg, string(themeContent), desc)
				genCancel()
				if gerr == nil && strings.TrimSpace(wbText) != "" {
					break
				}
				fmt.Printf(" [世界模拟] 世界书生成第%d次失败(%v)，重试…\n", attempt+1, gerr)
				time.Sleep(3 * time.Second)
			}
			if strings.TrimSpace(wbText) != "" {
				wbText = worldbook.TrimWorldbook(wbText)
				_ = os.WriteFile(filepath.Join(ws.baseDir, "..", "worldbooks", name+".md"), []byte(wbText), 0644)
				wbName = name
				fmt.Printf(" [世界模拟] 世界书已生成（主题包：%s）：%s\n", req.Theme, name)
			} else {
				fmt.Printf(" [世界模拟] 世界书生成失败（3次重试后），回退模板\n")
			}
		}
	}
	// 指定世界书则复制到 worldbooks/（供 newInstance 按名字加载）
	if wbName != "" {
		src := filepath.Join(ws.baseDir, "..", "worldbooks", wbName+".md")
		if _, err := os.Stat(src); err == nil {
			_ = os.WriteFile(filepath.Join(ws.baseDir, "..", "worldbooks", name+".md"), mustRead(src), 0644)
		}
	}
	wi := ws.newInstance(name)
	ws.worlds[name] = wi
	ws.current = name // 创建后自动切换
	fmt.Printf(" [世界模拟] 新世界已创建：%s\n", name)
	ws.writeJSON(w, 200, map[string]any{"ok": true, "world": name, "current": ws.current})
}

// GET /api/world/memories — 查看当前世界角色记忆（§4.6）
func (ws *worldServer) handleMemories(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可用世界"})
		return
	}
	if inst.sim == nil || inst.sim.MemoryStore() == nil {
		ws.writeJSON(w, 200, map[string]any{"memories": map[string]any{}})
		return
	}
	ms := inst.sim.MemoryStore()
	type actorMem struct {
		Actor   string           `json:"actor"`
		Memories []sim.MemoryEntry `json:"memories"`
	}
	var out []actorMem
	actors := ms.Actors()
	for _, a := range actors {
		entries := ms.Recent(a, 20)
		if len(entries) > 0 {
			out = append(out, actorMem{Actor: a, Memories: entries})
		}
	}
	ws.writeJSON(w, 200, map[string]any{"memories": out})
}

// GET /api/world/readiness — 模拟就绪度（素材够不够写小说：按剧情弧线完成度判断，不按天数）
func (ws *worldServer) handleReadiness(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil || inst.sim == nil {
		ws.writeJSON(w, 200, map[string]any{"ready": false, "reason": "模拟器未初始化"})
		return
	}
	ws.writeJSON(w, 200, inst.sim.Readiness())
}

// GET /api/world/decisions — 岔口决策队列（AI 代决 + 用户可干预；最新在前）
func (ws *worldServer) handleDecisions(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil || inst.sim == nil {
		ws.writeJSON(w, 200, map[string]any{"decisions": []sim.DecisionEntry{}})
		return
	}
	ws.writeJSON(w, 200, map[string]any{"decisions": inst.sim.AllDecisions()})
}

// POST /api/world/decisions/{id} — 用户改选（body: {"choice":"B"}，覆盖 AI 代决，写手按用户方向写）
func (ws *worldServer) handleDecisionResolve(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil || inst.sim == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "模拟器未初始化"})
		return
	}
	id := r.PathValue("id")
	var req struct {
		Choice string `json:"choice"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Choice == "" {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "需要 choice 字段（如 A/B/C 或一段文字）"})
		return
	}
	if !inst.sim.ResolveDecision(id, req.Choice) {
		ws.writeJSON(w, 404, map[string]any{"ok": false, "error": "岔口不存在：" + id})
		return
	}
	ws.writeJSON(w, 200, map[string]any{"ok": true, "id": id, "choice": req.Choice})
}

// GET /api/world/token_stats — 各环节 Token 用量总览（仿 langfuse：按环节聚合，观察"钱花在哪"）
func (ws *worldServer) handleTokenStats(w http.ResponseWriter, r *http.Request) {
	ws.writeJSON(w, 200, llm.SpanSummary())
}

func mustRead(p string) []byte {
	b, _ := os.ReadFile(p)
	return b
}

// ---------- 小说化联通（§8：编年史 → 小说章节） ----------

// POST /api/world/novel/generate — 把已模拟天数写成小说章节（逐章 LLM 生成）
// body: {"days_per_chapter":3,"chapter_len":"normal","all":false}
func (ws *worldServer) handleNovelGenerate(w http.ResponseWriter, r *http.Request) {
	ws.novelMu.Lock() // 防并发重复生成（同一时间只允许一个生成任务）
	defer ws.novelMu.Unlock()
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可用世界，请先创建"})
		return
	}

	if inst.sim == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "模拟器未初始化"})
		return
	}
	// 跨章记忆注入：未回收伏笔清单（模拟层账本→写手）+ 加载已有章节摘要（断点续写不丢记忆）
	inst.novelW.Foreshadows = inst.sim.OpenForeshadows()
	inst.novelW.LoadSummaries()
	var req struct {
		DaysPerChapter int    `json:"days_per_chapter"`
		ChapterLen     string `json:"chapter_len"`
		All            bool   `json:"all"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.DaysPerChapter > 0 {
		inst.novelW.DaysPerCh = req.DaysPerChapter
	}
	if req.ChapterLen != "" {
		inst.novelW.ChapterLen = req.ChapterLen
	}

	chronicle := inst.sim.Chronicle()
	thinkings := inst.sim.Thinkings()
	if len(chronicle) == 0 {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "编年史为空，先跑几天模拟（POST /api/world/sim/day）"})
		return
	}

	plans := inst.novelW.PlanChapters(chronicle, thinkings)
	if len(plans) == 0 {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可写的章节"})
		return
	}
	// 生成前清空旧章节（防上一轮残留/重复章号）
	chDir := filepath.Join(inst.novelW.BookDir, "chapters")
	os.RemoveAll(chDir)
	os.MkdirAll(chDir, 0755)

	// 统计已生成章节（按文件存在性）
	written := map[int]bool{}
	chaptersDir := filepath.Join(inst.novelW.BookDir, "chapters")
	if entries, err := os.ReadDir(chaptersDir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if len(name) >= 3 && name[0] >= '0' && name[0] <= '9' {
				var n int
				if _, err := fmt.Sscanf(name[:3], "%d", &n); err == nil {
					written[n] = true
				}
			}
		}
	}

	entities := inst.engine.State().Entities
	var done []int
	var skipped []int
	ctx := r.Context()
	for i := range plans {
		p := &plans[i]
		if written[p.Num] && !req.All {
			p.Status = "done"
			skipped = append(skipped, p.Num)
			continue
		}
		// 岔口决策注入：本章天数范围内的已定方向（用户改选优先，否则 AI 代决）——写手按方向写
		if inst.sim != nil {
			inst.novelW.Decisions = sim.FormatDirections(inst.sim.DecisionsFor(p.DayStart, p.DayEnd))
		}
		if _, err := inst.novelW.WriteChapter(ctx, *p, chronicle, thinkings, entities); err != nil {
			ws.writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error(), "chapter": p.Num})
			return
		}
		p.Status = "done"
		done = append(done, p.Num)
	}

	// 导出全书
	exports, _ := inst.novelW.ExportBook()
	webPath, _ := inst.novelW.ExportWebNovel()
	if webPath != "" {
		exports = append(exports, webPath)
	}
	// 同步到手机可见目录（/sdcard/Download/WorldSim/小说/{世界名}/）——用户能在文件管理器里直接找到
	syncNovelToDownload(inst.name, inst.novelW.BookDir)
	ws.writeJSON(w, 200, map[string]any{
		"ok":      true,
		"plans":   plans,
		"written": done,
		"skipped": skipped,
		"exports": exports,
	})
}

// syncNovelToDownload 把小说文件夹复制到手机 Download（每个世界固定目录，方便用户直接取文件）
func syncNovelToDownload(worldName, bookDir string) {
	dst := filepath.Join("/sdcard/Download", "WorldSim", "小说", worldName)
	if err := os.RemoveAll(dst); err != nil {
		return
	}
	if err := os.MkdirAll(dst, 0755); err != nil {
		return
	}
	// 复制目录内容（chapters/、全书、notes、summary）
	entries, err := os.ReadDir(bookDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		src := filepath.Join(bookDir, e.Name())
		if e.IsDir() {
			copyDir(src, filepath.Join(dst, e.Name()))
		} else {
			data, err := os.ReadFile(src)
			if err == nil {
				_ = os.WriteFile(filepath.Join(dst, e.Name()), data, 0644)
			}
		}
	}
}

// copyDir 递归复制目录
func copyDir(src, dst string) {
	_ = os.MkdirAll(dst, 0755)
	entries, err := os.ReadDir(src)
	if err != nil {
		return
	}
	for _, e := range entries {
		srcP := filepath.Join(src, e.Name())
		dstP := filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyDir(srcP, dstP)
		} else {
			data, err := os.ReadFile(srcP)
			if err == nil {
				_ = os.WriteFile(dstP, data, 0644)
			}
		}
	}
}

// GET /api/world/novel — 章节计划列表 + 导出文件
func (ws *worldServer) handleNovelList(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可用世界，请先创建"})
		return
	}

	if inst.sim == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "模拟器未初始化"})
		return
	}
	chronicle := inst.sim.Chronicle()
	plans := inst.novelW.PlanChapters(chronicle, inst.sim.Thinkings())
	if plans == nil {
		plans = []novel.ChapterPlan{}
	}
	chaptersDir := filepath.Join(inst.novelW.BookDir, "chapters")
	written := map[int]bool{}
	if entries, err := os.ReadDir(chaptersDir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if len(name) >= 3 && name[0] >= '0' && name[0] <= '9' {
				var n int
				if _, err := fmt.Sscanf(name[:3], "%d", &n); err == nil {
					written[n] = true
				}
			}
		}
	}
	for i := range plans {
		if written[plans[i].Num] {
			plans[i].Status = "done"
		}
	}
	exports, _ := inst.novelW.ExportBook()
	if exports == nil {
		exports = []string{}
	}
	ws.writeJSON(w, 200, map[string]any{"book": "临江异闻录", "plans": plans, "exports": exports})
}

// GET /api/world/novel/chapter/{num} — 读取章节正文
func (ws *worldServer) handleNovelChapter(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可用世界，请先创建"})
		return
	}

	num := r.PathValue("num")
	var n int
	if _, err := fmt.Sscanf(num, "%d", &n); err != nil {
		ws.writeJSON(w, 400, map[string]any{"ok": false, "error": "章节号无效"})
		return
	}
	chaptersDir := filepath.Join(inst.novelW.BookDir, "chapters")
	entries, err := os.ReadDir(chaptersDir)
	if err != nil {
		ws.writeJSON(w, 404, map[string]any{"ok": false, "error": "章节不存在"})
		return
	}
	for _, e := range entries {
		if len(e.Name()) >= 3 && e.Name()[0:3] == fmt.Sprintf("%03d", n) {
			data, err := os.ReadFile(filepath.Join(chaptersDir, e.Name()))
			if err != nil {
				ws.writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			ws.writeJSON(w, 200, map[string]any{"ok": true, "num": n, "title": strings.TrimSuffix(e.Name()[4:], ".md"), "content": string(data)})
			return
		}
	}
	ws.writeJSON(w, 404, map[string]any{"ok": false, "error": "章节不存在"})
}