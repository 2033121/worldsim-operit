// LLM output helpers: locate the first complete JSON object in free-form
// model output (string-aware brace matching).
package llm

import "strings"

// ponytail: byte walk outside JSON strings only; truncating mid-\\ or \\u may miscount.
// Used by ExtractJSON for string-aware object boundary detection.
func WalkJSONStructure(s string, onStruct func(i int, c byte)) {
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inString {
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		onStruct(i, c)
	}
}

func ExtractJSON(content string) string {
	start := strings.Index(content, "{")
	if start == -1 {
		return ""
	}
	sub := content[start:]
	depth := 0
	end := -1
	WalkJSONStructure(sub, func(i int, c byte) {
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				end = i + 1
			}
		}
	})
	if end == -1 {
		return ""
	}
	return content[start : start+end]
}

// ExtractJSONObjects 提取内容中所有独立的 JSON 对象（string-aware 深度匹配）。
// 用于 LLM 输出格式不完美时的容错：数组里夹字符串/坏元素、对象间缺逗号、夹杂散文等情况，
// 逐个提取 {...} 后由调用方拼数组解析。找不到任何对象返回空切片。
func ExtractJSONObjects(content string) []string {
	var out []string
	start := -1
	depth := 0
	WalkJSONStructure(content, func(i int, c byte) {
		if c == '{' {
			if depth == 0 {
				start = i
			}
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 && start >= 0 {
				out = append(out, content[start:i+1])
				start = -1
			}
		}
	})
	return out
}
