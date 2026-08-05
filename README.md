# WorldSim · 多Agent世界模拟器 → 网文生产引擎

> 让 AI 模拟"一个世界真实地运转"，再把世界编年史自动改写成人味十足的小说。
> 基于 [Nigh/show-me-the-story](https://github.com/Nigh/show-me-the-story) 深度改造（Go 单二进制 + WebUI，零外部依赖）。

## ✨ 特性

- **多Agent世界模拟**：总导演(GM)/事件Agent/主角三问决策/感知分发/NPC互动/小说写手，各司其职
- **任意题材通用**：15个主题包（修仙/末世/西幻/克苏鲁/都市/星际/历史…）+ 通用世界书骨架 → 一句话创建新世界
- **时间尺度自适应**：修仙跳年、末世跳日、星际按标准时——LLM 从世界书自行判断，不硬编码
- **就绪度驱动**：模拟不按天数结束，按"素材够不够写小说"（段落/戏剧素材/伏笔回收/张力）自动判定
- **岔口决策队列**：剧情多方向岔口 AI 自动代决（零阻塞），用户可随时翻案，写手按用户方向写
- **时间回退**：快照制存档，剧情跑偏/卡死随时回退到任意锚点重新演化
- **去AI味**：886+ 条真实网文示范素材库 + 六层写作方法论注入（记忆钉/冲突钩子/伏笔/感官五维/视角三不/高频词禁用）
- **双控制入口**：WebUI 可视化控制台（浏览器操作）+ 沙盒包 API（AI 对话驱动）
- **单二进制**：Go embed WebUI，零外部依赖，ARM64/Android 直接跑

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
├── 时间回退            snapshots/ 快照目录（文件复制制）
└── WebUI              单文件控制台（决策翻案/循环开关/回退/小说阅读）
```

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
```

浏览器打开 `http://localhost:48091` 即控制台：建世界（选主题包）→ 初始化 → 开循环 → 等就绪 → 生成小说。

### 4. 用 API 驱动（一行跑通）

```bash
# 创建世界（主题包+一句话设定 → LLM 自动生成世界书）
curl -X POST localhost:48091/api/worlds/create \
  -H 'Content-Type: application/json' \
  -d '{"name":"青岚界","theme":"经典修仙","desc":"山村少年捡到残破剑胚"}'

# 初始化（按世界书生成主角/NPC/地点）
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
| 小说 | `POST /api/world/novel/generate` `GET /api/world/novel` `GET /api/world/novel/chapter/{num}` |
| 主题包 | `GET /api/worldbooks/themes` |
| 统计 | `GET /api/world/token_stats` `GET /api/world/sim/thinking` |

## 🔌 Operit 插件包

本仓库是源码；打包好的 **Operit 插件包**（沙盒包 25 工具 + Skill + WebUI + 15 主题包）见 `/sdcard/Download/Operit/plugins/worldsim/`（分发 zip：`worldsim_plugin.zip`），含 README/TESTING 验收清单。

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
