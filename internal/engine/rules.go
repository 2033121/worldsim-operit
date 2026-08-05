package engine

import (
	"encoding/json"
	"fmt"
	"os"
)

// Rules 是硬约束规则（设计文档 §1.1 rules.yaml 的 Go 实现）。
// MVP 用 JSON 存储（标准库零依赖）；结构对齐设计文档的四类规则：
//   ① 数值范围  ② 前置条件  ③ 权限规则  ④ 枚举合法性
// 注：设计文档写的是 rules.yaml——若引入 YAML 解析库（gopkg.in/yaml.v3）
// 可无缝切换，Rules 结构不变。

type Rules struct {
	NumericRules  []NumericRule   `json:"numeric_rules"`
	Preconditions []Precondition  `json:"preconditions"`
	Permissions   []Permission    `json:"permissions"`
	Enums         map[string][]string `json:"enums"`
}

type NumericRule struct {
	Path string   `json:"path"` // 支持 * 通配，如 "entities.*.health"
	Min  *float64 `json:"min,omitempty"`
	Max  *float64 `json:"max,omitempty"`
}

type Precondition struct {
	Action   string        `json:"action"`
	Requires []Requirement `json:"requires"`
}

type Requirement struct {
	Path   string `json:"path"`
	Equals *string `json:"equals,omitempty"`
	Gte    *string `json:"gte,omitempty"` // 引用 changes[i].value 或常量
}

type Permission struct {
	Actor     string   `json:"actor"` // 支持 * 通配，如 "npc_*"
	DenyPaths []string `json:"deny_paths"`
}

// DefaultRules 返回一个开箱即用的默认规则集（示例世界用）
func DefaultRules() Rules {
	zero := 0.0
	hundred := 100.0
	return Rules{
		NumericRules: []NumericRule{
			{Path: "entities.*.money", Min: &zero},
			{Path: "entities.*.health", Min: &zero, Max: &hundred},
		},
		Permissions: []Permission{
			{Actor: "npc_*", DenyPaths: []string{"entities.*.money", "entities.*.health"}},
			{Actor: "protagonist", DenyPaths: []string{"world_level.factions"}},
		},
		Enums: map[string][]string{
			"entities.*.status": {"active", "departed", "dead", "mentioned"},
		},
	}
}

// LoadRules 从 JSON 文件加载规则集；文件不存在则返回默认规则
func LoadRules(path string) (Rules, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultRules(), nil
		}
		return Rules{}, err
	}
	var r Rules
	if err := json.Unmarshal(b, &r); err != nil {
		return Rules{}, fmt.Errorf("解析规则文件 %s 失败: %w", path, err)
	}
	return r, nil
}

// SaveRules 把规则集写入 JSON 文件（供用户编辑）
func SaveRules(r Rules, path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}