# WorldSim · Token 省钱优化报告

> 调研日期：2026-08-04 ｜ 方式：GitHub API 搜索 + 实测中转站缓存支持
> 目标：**保证质量的前提下，大幅降低 30 天/数年长程模拟的 LLM token 花费**

---

## 一、核心发现（实测验证）

### 🏆 金矿：DeepSeek 前缀缓存（中转站已支持）

实测 `tokenrhythm.studio` 中转站两次相同前缀调用：

| 调用 | prompt_tokens | cached_tokens | 命中率 |
|------|--------------|---------------|--------|
| 第一次 | 237 | 0 | 0% |
| 第二次 | 237 | **128** | **54%** |

**结论**：相同前缀（system prompt 字节级一致）自动命中缓存，命中部分按约 1/10 价格计费。
**WorldSim 天然适配**：我们的 LLM 调用结构 = 静态 system（世界书+规则+人设卡）+ 动态 user（当天状态），
只要保证 system 前缀稳定，**每次调用的世界书/规则部分几乎全部命中缓存**！

参考项目：**waveloom**（★115，纯 Go，DeepSeek 原生，前缀缓存架构 + pro/flash 分级，输入成本降至 1/50）——
我们把它"前缀缓存经济学"的核心思路直接落地到 WorldSim。

---

## 二、已落地优化（代码已改，下轮模拟生效）

### 1. 前缀稳定化（缓存命中率最大化）
- **NPC 对话**：动态"记忆"从 system 挪到 user —— system 只剩人设卡+规则（静态），跨天命中缓存
- **主角回应**：动态"老陈说的话"从 system 挪到 user
- 原则：**system = 纯静态**（世界书/规则/人设卡），**user = 动态**（当天状态/记忆/场景）

### 2. 前缀缓存统计（验证效果）
- `internal/llm/api.go`：解析响应 `usage.prompt_tokens_details.cached_tokens`，全局 atomic 统计
- 状态接口返回 `cache` 字段：`前缀缓存：N次调用 | 命中 X token (Y%) | 未命中 Z token`
- 跑完 30 天即可看到真实命中率

### 3. Prompt 瘦身（减少未命中部分）
- 新增 `internal/sim/slim.go`：
  - `slimEntities()`：实体只保留 location/job/money/health/status/关系值，**砍掉 Extra 里的 persona_sheet 人设卡、记忆等大字段**（这些按需单独注入）
  - `compactState()`：world_level（势力/地点/近5条事件/张力）+ 精简实体
- 应用点（4 处，每天省大量 token）：
  - 事件生成 `EventGenLLM`：全量实体 → slim
  - 世界影响 `WorldImpactLLM`：全量状态 marshal → compactState
  - Skip 快进 `skipSummaryLLM`：全量状态 → compactState
  - 主角决策 `ProtagonistDecideLLM`：单实体含 Extra → slim

### 4. 已有省钱机制（回顾确认）
- **模型分层**：fast=flash 干 90% 活，premium=pro 只写小说（已做）
- **张力引擎**：Scene/Summary/Skip 三级，低张力日 Skip 快进（1 次调用管多天）（已做）
- **记忆治理**：月度睡眠巩固 + CapMemory 200 条上限 + top-k 检索（已做）
- **GM 裁决**：软规则只对主角提案跑（已做）

---

## 三、可继续深挖的方向（备选）

| 方向 | 思路 | 预估收益 | 代价 |
|------|------|---------|------|
| **本地响应缓存 LRU** | 相同 system+user 直接复用结果（hash 命中） | 中（重复查询/裁决） | 低 |
| **世界书按需裁剪** | ForEventAgent 已只给 A1+A2+B2；ForWorldAgent 可去掉 B3/B4（导演意图不每天给） | 中 | 低 |
| **多天合并调用** | 平淡期 3-5 天合并成 1 次"周报"式推进 | 高（天数越多越省） | 中（改变剧情粒度） |
| **LLMLingua 式 prompt 压缩**（microsoft/LLMLingua ★6521） | 用小模型压缩长 prompt，保留关键信息 | 高 | 高（需引入压缩模型/服务，与 Go 单二进制冲突） |
| **更便宜的 fast 模型** | 事件/日常用更小更快模型 | 中 | 低（需中转站有更低档模型） |
| **代理缓存服务** | 本地 HTTP 缓存层，命中直接返回（如 GPTCache 思路） | 中 | 中 |

---

## 四、预期效果

以 30 天模拟估算（每天约 5-7 次调用）：
- **前缀缓存**：世界书/规则/人设卡约占每次 prompt 的 60-80%，若命中率 80%+ → **输入成本降到约 1/3**
- **Prompt 瘦身**：实体/状态裁剪再省 20-40% 输入 token
- 叠加后：**总输入 token 预计节省 50-70%，质量不变**（裁剪的只是冗余上下文，核心设定/记忆/状态仍在）
