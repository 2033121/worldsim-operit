# WorldSim · Operit 移动端专版

> 多Agent世界模拟器 → 网文生产引擎（Operit 专属分支）
> 让 AI 模拟"一个世界真实地运转"，再把世界编年史自动改写成人味十足的小说。
> 基于 [Nigh/show-me-the-story](https://github.com/Nigh/show-me-the-story) 深度改造（Go 单二进制 + WebUI，零外部依赖）。

本仓库是 **Operit（手机端 AI 工作台）专用分支**，与上游 `worldsim` 仓库分开维护。所有改动只推送到本仓库，服务部署于 Android 上的 Operit 沙盒环境。

---

## ✨ 特性

- **多Agent世界模拟**：总导演(GM)/事件Agent/主角三问决策/感知分发/NPC互动/小说写手，各司其职
- **任意题材通用**：15个主题包（修仙/末世/西幻/克苏鲁/都市/星际/历史…）+ 通用世界书骨架 → 一句话创建新世界
- **提示词零污染**：所有 LLM Prompt 使用抽象描述，由世界书驱动，绝不硬编码任何世界名/角色名/示例
- **动态实体属性（Stats）**：角色属性不再固定"健康/金钱"，由世界书力量体系/资源体系推导（西幻→序列/污染值/钟感…），引擎零硬编码
- **配角分层系统**：核心配角（core）/普通配角（support）/龙套（walkon，不建档不占记忆）/背景提及（mentioned，淡出态）
  - 随剧情注册新配角机制保留，背景人物可升级正式登场
  - 配角 30 天未出场自动淡出，仅在编年史被提起；事件指定时恢复活跃
- **时间尺度自适应**：修仙跳年、末世跳日、星际按标准时——LLM 从世界书自行判断，不硬编码
- **就绪度驱动**：模拟不按天数结束，按"素材够不够写小说"（段落/戏剧素材/伏笔回收/张力）自动判定
- **岔口决策队列**：剧情多方向岔口 AI 自动代决（零阻塞），用户可随时翻案，写手按用户方向写
- **运行检测 + 自动修复**：健康检查 `/api/health`、日志查看 `/api/logs`、心跳文件、连续失败自动修复
- **防空转快照回档**：自动快照（7天/风险前/手动），LLM 连续失败空转时自动回退到最近健康快照
- **去AI味**：886+ 条真实网文示范素材库 + 六层写作方法论注入
- **单二进制**：Go embed WebUI，零外部依赖，ARM64/Android 直接跑

---

## 🏗️ 架构

```
WorldSim（端口 48091）
├── State Engine（事件溯源：event_log.jsonl + world_state.json + Replay）
├── Simulator（多Agent调度：事件→感知→主角决策→GM裁决→NPC→记录）
│   ├── GM Agent        世界书裁决/段落规划（导演）
│   ├── Event Agent     事件生成（B5事件谱：冲突/奇遇/生活切片…）
│   ├── Protagonist     三问决策（价值/能力/世界线）→ 记忆沉淀
│   ├── NPC 互动        Init→Act→React 对话链
│   └── 伏笔账本        埋设/成熟/回收全周期
├── 世界书体系          _template.md 通用骨架 + themes/ 15主题包 + B5事件谱
├── 就绪度             arcs/drama/foreshadows/tension 四指标
├── 运行检测            internal/health（/api/health + /api/logs + heartbeat）
├── 日志系统            internal/logx（分级日志 + 按天轮转 + 健康指标采集）
├── 时间回退            snapshots/ 快照目录（自动 + 风险前 + 防空转回档）
└── WebUI              单文件控制台（决策翻案/循环开关/回退/小说阅读）
```

## 🛠️ Operit 移动端专版增强（相对上游）

| 模块 | 说明 |
|---|---|
| `internal/logx/` | 分级日志（DEBUG/INFO/WARN/ERROR）+ 按天轮转文件 + LLM 成功率/耗时/dry-run/连续失败指标采集 |
| `internal/health/` | `GET /api/health`（存活+LLM连通+世界健康度）、`GET /api/logs`、heartbeat.json 心跳、AutoHeal 自动修复 |
| 防空转快照 | 自动快照 30天→7天；LLM 首次失败自动存健康快照；连续 5 次 dry-run 自动回退最近健康快照重建模拟器 |
| 世界书解析器 | 重写 Parse：支持 A1~A12/B1~B5/C/C1/D/E1~E9 全字段，修复 section 错位 bug |
| 配角分层 | `extra.tier`：core/support/walkon/mentioned 四档；初始化 core3-4+support2-3+提及型 |
| 动态属性 Stats | `Entity.Stats map[string]any`，世界书驱动，引擎零硬编码，JSON 数值容错解析 |
| 提示词净化 | 全量删除 LLM Prompt 中的硬编码世界观/角色名/固定示例，改为抽象描述 |

## 🚀 快速开始

### 1. 构建（Go 1.22+）

```bash
go build -o worldsim .
# 产物：单个 ~10MB 二进制，零依赖
```

### 2. 配置 LLM（api.json，放在程序目录）

```json
{
  "base_url": "https://your-api-endpoint/v1",
  "model": "your-model",
  "api_key": "YOUR_API_KEY",
  "http_timeout_seconds": 300,
  "model_tiers": {
    "fast": "your-fast-model",
    "normal": "your-normal-model",
    "premium": "your-premium-model"
  }
}
```

### 3. 启动

```bash
./worldsim /path/to/data-dir
# 世界模拟服务: http://localhost:48091
# 小说创作服务:  http://localhost:48090
# 健康检查:      http://localhost:48091/api/health
```

浏览器打开 `http://localhost:48091` 即控制台：建世界（选主题包）→ 初始化 → 开循环 → 等就绪 → 生成小说。

### 4. 用 API 驱动（一行跑通）

```bash
# 创建世界（主题包+一句话设定 → LLM 自动生成世界书）
curl -X POST localhost:48091/api/worlds/create \
  -H 'Content-Type: application/json' \
  -d '{"name":"青岚界","theme":"经典修仙","desc":"山村少年捡到残破剑胚"}'

# 初始化（按世界书生成主角/NPC/地点，含分层与 Stats）
curl -X POST localhost:48091/api/world/init

# 后台持续运行（到就绪自动停）
curl -X POST localhost:48091/api/world/loop \
  -H 'Content-Type: application/json' -d '{"action":"start","days":1000}'

# 查就绪 → 生成小说 → 读章节
curl localhost:48091/api/world/readiness
curl -X POST localhost:48091/api/world/novel/generate
curl localhost:48091/api/world/novel/chapter/1
```

## 📚 API 一览

| 分组 | 接口 |
|---|---|
| 世界管理 | `GET /api/worlds` `POST /api/worlds/create` `POST /api/worlds/select` `POST /api/world/init` |
| 状态 | `GET /api/world/state` `GET /api/world/chronicle` `GET /api/world/memories` `GET /api/world/foreshadows` |
| 模拟 | `POST /api/world/sim/day` `POST /api/world/loop`(start/stop/status) `GET /api/world/readiness` |
| 决策 | `GET /api/world/decisions` `POST /api/world/decisions/{id}` |
| 时间回退 | `GET /api/world/snapshots` `POST /api/world/snapshot` `POST /api/world/rewind` |
| 运行检测 | `GET /api/health` `GET /api/logs` |
| 小说 | `POST /api/world/novel/generate` `GET /api/world/novel` `GET /api/world/novel/chapter/{num}` |
| 主题包 | `GET /api/worldbooks/themes` |
| 统计 | `GET /api/world/token_stats` `GET /api/world/sim/thinking` |

## 🔌 Operit 部署说明

- 源码位置：`/sdcard/Download/Operit/WorldSim_dev/worldsim/`
- 部署二进制：`/sdcard/Download/Operit/plugins/worldsim/worldsim`（同时更新 `/tmp/worldsim_run/worldsim`）
- 编译环境：sdcard 上 go build 报 RLock 错误，需在 `/tmp/wsbuild` 编译后拷贝
- 进程启动：`setsid ... > /dev/null 2>&1 < /dev/null & disown` 防终端会话杀进程
- 重启服务：`pkill -9 -f '/tmp/worldsim_run/worldsim'`（⚠️ 不要用 `pkill -f 'worldsim'` 宽匹配——会误杀长跑脚本/看门狗的子进程）
- 一键推送：`bash push_operit.sh "提交说明"`（自动同步源码 → 增量提交 → 推送本仓库）

## 📂 目录说明

```
worldsim/
├── main.go            服务入口（双端口：48090小说 / 48091世界模拟）
├── wsweb/             WebUI 单文件控制台（embed 进二进制）
├── internal/
│   ├── engine/        State Engine（事件溯源/提案/重放/软规则）
│   ├── sim/           多Agent模拟器（事件/决策/NPC/伏笔/记忆/快照/就绪度）
│   ├── worldbook/     世界书解析 + 主题包 + LLM世界书生成
│   ├── llm/           分层模型调用 + token 追踪 + 前缀缓存统计
│   ├── logx/          分级日志 + 健康指标采集（移动端专版新增）
│   ├── health/        运行检测 + 自动修复（移动端专版新增）
│   ├── novel/         小说写手（素材投喂/章节规划/去AI味铁律）
│   └── config/        配置加载
├── worldbooks/        世界书池（模板+主题包+实例）
└── docs/              设计文档
```

## ⚠️ 安全与版权

- **密钥**：`api.json` 已被 `.gitignore` 排除，**绝不提交**。示例请用占位符。
- **数据**：`worlds/` `storys/` 为个人世界数据，不入库。
- **素材库**：`material/`（886+ 条真实网文风格示范）有版权考量，本地持有，不入库。

## 📜 License

[MIT](LICENSE)