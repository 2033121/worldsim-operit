// Package logx 提供带分级、文件持久化、按天轮转的结构化日志。
// 同时采集运行时健康指标（LLM 成功率、事件生成、dry-run 计数等），
// 供 /api/health 与自动修复使用。
package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Level 日志级别
type Level int

const (
	DEBUG Level = iota
	INFO
	WARN
	ERROR
)

var levelNames = map[Level]string{
	DEBUG: "DEBUG",
	INFO:  "INFO",
	WARN:  "WARN",
	ERROR: "ERROR",
}

// Logger 日志器（全局单例）
type Logger struct {
	mu       sync.Mutex
	file     *os.File
	dir      string // 日志目录（wsdata/）
	minLevel Level
	day      string // 当前日志文件名对应的日期（轮转用）
}

var (
	instance *Logger
	once     sync.Once
)

// Get 返回全局日志器（懒初始化；dir 为空则只输出控制台）
func Get(dir string) *Logger {
	once.Do(func() {
		instance = &Logger{dir: dir, minLevel: INFO}
		instance.openFile()
	})
	return instance
}

// SetMinLevel 设置最小输出级别
func (l *Logger) SetMinLevel(lv Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.minLevel = lv
}

func (l *Logger) logFile() string {
	if l.day == "" {
		l.day = time.Now().Format("2006-01-02")
	}
	return filepath.Join(l.dir, fmt.Sprintf("run-%s.log", l.day))
}

// openFile 打开当日日志文件（不存在则创建；跨天自动切换）
func (l *Logger) openFile() {
	if l.dir == "" {
		return
	}
	os.MkdirAll(l.dir, 0755)
	f, err := os.OpenFile(l.logFile(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("[logx] 无法打开日志文件 %s: %v\n", l.logFile(), err)
		return
	}
	l.file = f
}

// rotate 检查是否需要按天轮转
func (l *Logger) rotate() {
	today := time.Now().Format("2006-01-02")
	if l.day != today {
		if l.file != nil {
			l.file.Close()
		}
		l.day = today
		l.openFile()
	}
}

// write 核心写日志：控制台 + 文件（文件含时间戳，控制台保持简洁）
func (l *Logger) write(lv Level, tag, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lv < l.minLevel {
		return
	}
	l.rotate()
	msg := fmt.Sprintf(format, args...)
	ts := time.Now().Format("2006-01-02 15:04:05")
	line := fmt.Sprintf("[%s] [%s] [%s] %s\n", ts, levelNames[lv], tag, msg)
	// 控制台：带级别的完整行
	fmt.Printf("[%s] [%s] %s\n", levelNames[lv], tag, msg)
	// 文件：完整结构化行
	if l.file != nil {
		l.file.WriteString(line)
	}
}

func (l *Logger) Debug(tag, format string, args ...any) { l.write(DEBUG, tag, format, args...) }
func (l *Logger) Info(tag, format string, args ...any)  { l.write(INFO, tag, format, args...) }
func (l *Logger) Warn(tag, format string, args ...any)  { l.write(WARN, tag, format, args...) }
func (l *Logger) Error(tag, format string, args ...any) { l.write(ERROR, tag, format, args...) }

// ---------------------------------------------------------------------------
// 运行健康指标（供 /api/health 与自动修复读取）
// ---------------------------------------------------------------------------

// Metrics 运行时健康指标（线程安全计数器）
type Metrics struct {
	mu sync.Mutex

	// LLM 调用
	LLMCalls      int64   // 总调用次数
	LLMFailures   int64   // 失败次数
	LLMDryRuns    int64   // dry-run 兜底次数（事件生成失败走模板）
	LLMTotalMS    int64   // 累计耗时(ms)
	LLMLastErr    string  // 最近一次错误
	LLMLastErrDay int     // 最近一次错误发生日
	LLMConsecFail int     // 连续失败次数（自动修复用）

	// 世界推进
	LastSimDay   int     // 最近成功推进到的 day
	LastSimOK    bool    // 最近一次模拟是否成功（非 dry-run）
	EventGenOK   int64   // 事件生成成功次数
	EventGenFail int64   // 事件生成失败次数
	StartTime    int64   // 进程启动时间戳
}

var metrics = &Metrics{StartTime: time.Now().Unix()}

// Metrics 返回全局指标（并发安全）
func M() *Metrics { return metrics }

// RecordLLM 记录一次 LLM 调用结果
func (m *Metrics) RecordLLM(ok bool, ms int64, day int, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LLMCalls++
	m.LLMTotalMS += ms
	if ok {
		m.LLMConsecFail = 0
		m.LastSimOK = true
	} else {
		m.LLMFailures++
		m.LLMConsecFail++
		m.LLMLastErr = errMsg
		m.LLMLastErrDay = day
		m.LastSimOK = false
	}
}

// RecordDryRun 记录一次 dry-run 兜底（事件生成失败但用模板续命）
func (m *Metrics) RecordDryRun(day int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LLMDryRuns++
	m.EventGenFail++
	m.LastSimDay = day
}

// RecordEvent 记录事件生成结果
func (m *Metrics) RecordEvent(ok bool, day int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ok {
		m.EventGenOK++
		m.LastSimDay = day
	} else {
		m.EventGenFail++
	}
}

// RecordSimDay 记录世界成功推进到的 day
func (m *Metrics) RecordSimDay(day int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LastSimDay = day
}

// Snapshot 返回指标快照（JSON 友好）
func (m *Metrics) Snapshot() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	rate := 0.0
	if m.LLMCalls > 0 {
		rate = float64(m.LLMCalls-m.LLMFailures) / float64(m.LLMCalls) * 100
	}
	return map[string]any{
		"llm_calls":         m.LLMCalls,
		"llm_failures":      m.LLMFailures,
		"llm_success_rate":  round1(rate),
		"llm_dry_runs":      m.LLMDryRuns,
		"llm_avg_ms":        avg(m.LLMTotalMS, m.LLMCalls),
		"llm_last_err":      m.LLMLastErr,
		"llm_consec_fail":   m.LLMConsecFail,
		"event_gen_ok":      m.EventGenOK,
		"event_gen_fail":    m.EventGenFail,
		"last_sim_day":      m.LastSimDay,
		"last_sim_ok":       m.LastSimOK,
		"uptime_seconds":    time.Now().Unix() - m.StartTime,
	}
}

// Healthy 判断整体健康度：连续 LLM 失败 > 5 或最近 3 次模拟全 dry-run 视为不健康
func (m *Metrics) Healthy() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.LLMConsecFail >= 5 {
		return false, fmt.Sprintf("LLM 连续失败 %d 次（最近：%s）", m.LLMConsecFail, m.LLMLastErr)
	}
	if m.LLMCalls > 0 && m.LastSimDay > 0 && !m.LastSimOK {
		return false, "最近一次世界推进走 dry-run 兜底"
	}
	return true, "正常"
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
func avg(total, n int64) int64 {
	if n <= 0 {
		return 0
	}
	return total / n
}

// Trunc 截断错误信息（防日志爆炸）
func Trunc(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
