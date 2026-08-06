package llm

import "strings"

// ExtractJSONArray 从 LLM 输出中提取第一个 JSON 数组（含外层 [ ]）。
// 用于事件生成等输出数组的场景；找不到数组返回 ""。
func ExtractJSONArray(content string) string {
	start := strings.Index(content, "[")
	if start == -1 {
		return ""
	}
	// 从 start 开始找匹配的 ']'（简单处理：取最后一个 ']'，LLM 输出一般结构简单）
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(content); i++ {
		c := content[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return content[start : i+1]
			}
		}
	}
	return ""
}
