// Package health 提供运行检测与自动修复：
//
//	① /api/health —— 服务存活 + LLM 连通 + 世界推进健康度
//	② 自动修复 —— LLM 连续失败时切换降级策略、检测世界停滞、输出自愈报告
//	③ 心跳 —— 进程崩溃可被外部检测（重启看门狗）
package health

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"worldsim/internal/logx"
)

// Checker 健康检查器
type Checker struct {
	mu          sync.Mutex
	BaseDir     string // wsdata 目录
	WorldsDir   string // worlds 目录
	HeartbeatMS int64  // 心跳间隔毫秒
	lastBeat    int64  // 上次心跳时间戳
	startTime   int64

	// 世界推进停滞检测
	lastDayChecked int
	stuckThreshold int // 连续检查 N 次无推进视为停滞
	stuckCount     int

	// 自动修复冷却（避免问题持续时每轮刷屏）
	lastHealAt time.Time

	// 修复动作计数（报告用）
	AutoHeals int64
	LastHeal  string
}

// healCooldown 自动修复冷却期：触发一次后该时长内不再重复触发
const healCooldown = 2 * time.Minute

// New 创建健康检查器
func New(baseDir string) *Checker {
	return &Checker{
		BaseDir:        baseDir,
		WorldsDir:      filepath.Join(baseDir, "worlds"),
		HeartbeatMS:    5000,
		startTime:      time.Now().Unix(),
		lastBeat:       time.Now().UnixMilli(),
		stuckThreshold: 3,
	}
}

// Start 启动后台守护：心跳写盘 + 定期自检
func (c *Checker) Start() {
	go func() {
		ticker := time.NewTicker(time.Duration(c.HeartbeatMS) * time.Millisecond)
		for range ticker.C {
			c.Beat()
			c.AutoHeal()
		}
	}()
}

// Beat 写心跳文件（外部看门狗检测进程存活用）
func (c *Checker) Beat() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastBeat = time.Now().UnixMilli()
	if c.BaseDir == "" {
		return
	}
	payload := map[string]any{
		"alive":   true,
		"ts_ms":   c.lastBeat,
		"pid":     os.Getpid(),
		"uptime":  time.Now().Unix() - c.startTime,
		"version": "1.0",
	}
	b, _ := json.Marshal(payload)
	os.WriteFile(filepath.Join(c.BaseDir, "heartbeat.json"), b, 0644)
}

// 全局修复回调（由 main 注入：如"切换 LLM 降级模型"）
var (
	healMu      sync.Mutex
	healHandler func(reason string) string // 返回修复结果描述
)

// SetAutoHealHandler 注入自动修复处理器（main 里设置）
func SetAutoHealHandler(f func(reason string) string) {
	healMu.Lock()
	defer healMu.Unlock()
	healHandler = f
}

// AutoHeal 自动修复：读健康指标，发现问题触发修复
// 冷却：触发一次后 healCooldown 内不再重复触发（问题持续时不再每轮刷屏）
func (c *Checker) AutoHeal() {
	m := logx.M()
	ok, reason := m.Healthy()
	if ok {
		// 恢复健康时清空停滞计数
		c.mu.Lock()
		c.stuckCount = 0
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	c.stuckCount++
	// 冷却期内的重复不健康不再触发（抖动保护：连续2次 + 冷却保护：2分钟内不重复）
	if c.stuckCount < 2 || time.Since(c.lastHealAt) < healCooldown {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	// 触发修复
	healMu.Lock()
	fn := healHandler
	healMu.Unlock()
	result := "无修复处理器"
	if fn != nil {
		result = fn(reason)
	}
	c.mu.Lock()
	c.AutoHeals++
	c.LastHeal = fmt.Sprintf("%s → %s", time.Now().Format("15:04:05"), result)
	c.lastHealAt = time.Now() // 记录冷却起点
	c.stuckCount = 0          // 修复后重置，观察是否恢复
	c.mu.Unlock()
	logx.Get(c.BaseDir).Warn("自愈", "触发自动修复：%s → %s", reason, result)
}

// ---------- HTTP Handler ----------

// HandleHealth /api/health 健康检查端点
func (c *Checker) HandleHealth(w http.ResponseWriter, r *http.Request) {
	m := logx.M()
	healthy, reason := m.Healthy()
	metrics := m.Snapshot()
	resp := map[string]any{
		"ok":         healthy,
		"reason":     reason,
		"time":       time.Now().Format("2006-01-02 15:04:05"),
		"pid":        os.Getpid(),
		"uptime_sec": time.Now().Unix() - c.startTime,
		"heartbeat":  c.lastBeat,
		"metrics":    metrics,
		"auto_heals": c.AutoHeals,
		"last_heal":  c.LastHeal,
		"worlds_dir": c.WorldsDir,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if !healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(resp)
}

// HandleLogs /api/logs 读取今日日志尾部（诊断用）
func (c *Checker) HandleLogs(w http.ResponseWriter, r *http.Request) {
	lines := 100
	if v := r.URL.Query().Get("lines"); v != "" {
		fmt.Sscanf(v, "%d", &lines)
		if lines > 500 {
			lines = 500
		}
	}
	path := filepath.Join(c.BaseDir, fmt.Sprintf("run-%s.log", time.Now().Format("2006-01-02")))
	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "日志不存在: "+err.Error(), 404)
		return
	}
	// 取尾部 lines 行
	all := string(data)
	idx := 0
	count := 0
	for i := len(all) - 1; i >= 0; i-- {
		if all[i] == '\n' {
			count++
			if count > lines {
				idx = i + 1
				break
			}
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(all[idx:]))
}
