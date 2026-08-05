package sim

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ---------- 时间回退（快照制） ----------
// 设计：每次落盘后，把"可完整恢复的状态文件"复制成一份快照目录。
// 回退 = 把目标快照目录里的文件复制回 worldDir（覆盖），由调用方重建 Simulator。
// 文件复制制：零序列化风险，与运行时完全一致。
// 自动快照：每 30 天 + 用户手动；滚动保留最近 20 个。

type SnapshotMeta struct {
	Day       int    `json:"day"`
	Revision  int    `json:"revision"`
	CreatedAt string `json:"created_at"`
	Reason    string `json:"reason"`
	Dir       string `json:"dir"`
}

// 快照包含的状态文件（全部是落盘文件，缺啥补啥）
var snapshotFiles = []string{
	"world_state.json",
	"event_log.jsonl",
	"chronicle.jsonl",
	"thinkings.json",
	"plans.json",
	"agents_memory.json",
	"decisions.jsonl",
	"token_stats.json",
}

func (s *Simulator) snapshotsDir() string {
	return filepath.Join(s.worldDir, "snapshots")
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}

// SaveSnapshot 保存当前完整状态快照（自动/手动通用）
func (s *Simulator) SaveSnapshot(reason string) (SnapshotMeta, error) {
	dir := filepath.Join(s.snapshotsDir(),
		fmt.Sprintf("snap_day%d_rev%d", s.day, s.engine.State().Revision))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return SnapshotMeta{}, err
	}
	// 幂等：已存在（同 day+rev）则只更新 meta，避免重复快照
	for _, f := range snapshotFiles {
		src := filepath.Join(s.worldDir, f)
		if _, err := os.Stat(src); err == nil {
			_ = copyFile(src, filepath.Join(dir, f))
		}
	}
	meta := SnapshotMeta{
		Day:       s.day,
		Revision:  s.engine.State().Revision,
		CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
		Reason:    reason,
		Dir:       dir,
	}
	if b, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "meta.json"), b, 0644)
	}
	cleanupSnapshots(s.snapshotsDir(), 20)
	return meta, nil
}

// Snapshots 全部快照（按 day 降序，最新在前）
func (s *Simulator) Snapshots() []SnapshotMeta {
	entries, err := os.ReadDir(s.snapshotsDir())
	if err != nil {
		return nil
	}
	out := []SnapshotMeta{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mb, err := os.ReadFile(filepath.Join(s.snapshotsDir(), e.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var m SnapshotMeta
		if json.Unmarshal(mb, &m) != nil {
			continue
		}
		m.Dir = filepath.Join(s.snapshotsDir(), e.Name())
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day > out[j].Day })
	return out
}

// RewindTo 回退到 ≤day 的最近快照：把快照文件复制回 worldDir（覆盖）
// 返回回退到的快照；调用方随后需重建 Simulator + 刷新 engine 内存状态。
func (s *Simulator) RewindTo(day int) (SnapshotMeta, error) {
	for _, sn := range s.Snapshots() {
		if sn.Day <= day {
			n := 0
			for _, f := range snapshotFiles {
				src := filepath.Join(sn.Dir, f)
				if _, err := os.Stat(src); err == nil {
					if copyFile(src, filepath.Join(s.worldDir, f)) == nil {
						n++
					}
				}
			}
			if n == 0 {
				return SnapshotMeta{}, fmt.Errorf("快照无有效文件: %s", sn.Dir)
			}
			return sn, nil
		}
	}
	return SnapshotMeta{}, fmt.Errorf("没有 ≤Day%d 的快照（先跑几天模拟生成快照，或手动存档）", day)
}

// cleanupSnapshots 滚动清理：最多保留 keep 个（按目录名排序，删最旧）
func cleanupSnapshots(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	if len(entries) <= keep {
		return
	}
	names := []string{}
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for i := 0; i < len(names)-keep; i++ {
		_ = os.RemoveAll(filepath.Join(dir, names[i]))
	}
}
