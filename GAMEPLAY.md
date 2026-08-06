# WorldSim 游戏化分支开发说明（feature/playable）

> 游戏化改造在**独立分支 + 独立源码副本**上开发，**不影响 main 稳定版**（长跑/日常修复继续在 main 走）。
> 最后更新：Phase 1-4 全部完成，测试 22 个全 PASS。

## 分支架构

```
GitHub: 2033121/worldsim-operit
├── main             稳定版（世界模拟+网文生产引擎，长跑运行中）
└── feature/playable 游戏化分支（"能在小说世界里玩"）

本地源码
├── /sdcard/Download/Operit/WorldSim_dev/worldsim       → main（稳定版，勿动）
└── /sdcard/Download/Operit/WorldSim_dev/worldsim_game  → feature/playable（游戏化工作副本）
```

## 日常操作

| 操作 | 命令 |
|---|---|
| 推送 main 稳定版 | `bash push_operit.sh "说明"` |
| 推送游戏化分支 | `bash push_game.sh "说明"`（同步 worldsim_game → feature/playable） |
| 编译游戏化副本 | 拷贝 worldsim_game → /tmp/wsbuild_game 后 `go build`（sdcard 直接编译会 RLock 报错） |
| 部署游戏化服务 | `WORLDSIM_PORT=48092 WORLDSIM_STORY_PORT=48093` 起独立端口（与 main 48091 不冲突），数据目录用独立副本 |
| 测试游戏化 | `cd /tmp/wsbuild_game && GOTOOLCHAIN=local go test ./internal/sim/` |

## ✅ 已完成进度（Phase 1-4）

### Phase 1：玩家指令介入层
- **新增文件**：`internal/sim/player.go`
- `POST /api/world/player/act`：玩家向世界发指令（`{intent, target}`），排队零阻塞
- 事件日注入：RunDay 事件生成前把 pending 指令拼进 extraCtx（"来自外界的指令"抽象描述，零污染）
- 消费回执：settlePlayerIntents 写入事件摘要（`{status: consumed, response}`）
- 持久化：`player_intents.json` 原子写 + 0600 权限，重启不丢
- 关键设计：不打断长跑、不硬控世界（事件 Agent 可呼应/部分呼应/延后）

### Phase 2：状态面板 + 行动选项 + WebUI
- **新增文件**：`internal/sim/player_state.go`
- `GET /api/world/player/state`：主角状态卡（能力 chips）/当前段落/顶级关系/最近事件/行动选项/指令回执
- **行动选项**（零 LLM 零成本，世界数据驱动）：
  - investigate：当前段落目标 → "围绕当前形势深入调查"
  - social：关系最好角色 → "主动去找X深谈"
  - cultivate：最高能力 → "投入时间提升「attr」"
  - rest/explore：健康<70 休整，否则打听
- **WebUI**：中栏新增"🎮 玩家"Tab（默认激活）：
  - 主角状态卡（头像/能力 chips/健康/资财进度条）
  - 当前目标卡（段落+对手+里程碑）
  - 行动按钮 + 自定义指令输入
  - 指令时间线（🕐等待 → ✅回执）
  - 关系网络（档位徽章+数值条）
- `WORLDSIM_PORT`/`WORLDSIM_STORY_PORT` 环境变量支持独立端口

### Phase 3：任务系统 + 战斗回合模拟
- **新增文件**：`internal/sim/player_quest.go`、`internal/sim/player_combat.go`
- **任务系统**：段落 milestones → 任务卡（done/active/upcoming），状态由 arcDone 驱动，段落收尾自动换新任务
- **战斗模拟**：`POST /api/world/player/combat`（body `{target}`）
  - 纯规则零 LLM：stats 数值（≤20 能力型）+ 健康 → 攻防；骰子回合制（最多 6 回合）
  - 输出：胜率预估（40 局统计）、实力差距档位、逐回合日志
  - **预览模式**：不写世界状态（真实剧情由事件 Agent 决定）
- **数值修正经验**：
  - 财富/资源类大数值（>20）不参与战力（避免"家财万贯的商人"碾压战力）
  - 限时结束比**剩余血量比例**而非绝对血量（避免血厚者天然占优）

### Phase 4：玩家行动真实生效
- **新增文件**：`internal/sim/player_actions.go`
- **行动点系统**：上限 5、每模拟日恢复 1（RunDay 调用 recoverAP），持久化 `player_ap.json`
- `POST /api/world/player/action`（body `{kind, target}`）：
  - rest：健康 +8（满血防刷）
  - cultivate：最高 ≤20 数值能力 +1（不精进负面状态如伤势）
  - social：与 target 关系 +0.05
- **关键设计**：走 `engine.Submit` 原子提交（与 LLM 世界推进同路径，硬规则校验 + event_log），LLM 后续感知玩家变化
- **WebUI**：主角卡 ⚡行动点徽章、数值行动按钮带 ⚡1 标记、行动后 toast + 面板实时刷新

## 📡 API 清单（游戏化）

| 接口 | 说明 |
|---|---|
| `POST /api/world/player/act` | 发指令（异步，事件日消费） |
| `GET /api/world/player/state` | 完整玩家面板（hero/arc/rel/actions/intents/quests/combat_targets/ap_balance） |
| `POST /api/world/player/combat` | 战斗回合模拟（预览，body `{target}`） |
| `POST /api/world/player/action` | 行动真实生效（body `{kind: rest\|cultivate\|social, target}`） |

## ✅ 测试清单（internal/sim/，22 个全 PASS）

- Phase1：TestQueueAndConsume / TestPlayerIntentPersistence / TestPlayerIntentPrompt（零污染）
- Phase2：TestPlayerStateFull / TestPlayerStateNoHero / TestPlayerStateZeroPollution
- Phase3：TestSimulateCombat / TestCombatZeroPollution / TestQuests
- Phase4：TestPlayerActionRest / TestPlayerActionCultivate / TestPlayerActionSocial / TestPlayerAPRecover / TestPlayerActionZeroPollution
- 原有：slim 系列 8 个

## 🎮 下一步候选（未开工）

1. **战斗真实生效**：战斗胜/负写入世界（关系/声望变化）
2. **多世界共享玩家档案**：一个玩家游历多个世界
3. **平衡性调优**：行动点节奏/数值上限按体验微调
4. **关键节点暂停**：高 severity 事件时暂停等玩家选择（Phase1 曾规划，未做）

## 零污染铁律（本项目红线）

- LLM Prompt 只能抽象描述、由世界书驱动，绝不硬编码世界名/角色名/体系词
- 已清理历史污染：character.go stats 生成、kc_skills.go "境界"、narrative/units.go "修仙/末世"、玩家模块 "机缘"→"机遇"
- 战斗/任务/行动系统只看数值范围与通用字段，不看属性名（单测断言）

## 参考项目（已克隆 /tmp/refs/）

- `mnehmos-engine`（TypeScript，2万行）：确定性引擎校验 + 互动区域 + 经济系统 + DM 层
- `ai-gamestudio`（Python+插件）：combat/inventory/social 游戏化机制插件