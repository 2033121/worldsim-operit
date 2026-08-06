# WorldSim 游戏化分支开发说明（feature/playable）

> 游戏化改造在**独立分支 + 独立源码副本**上开发，**不影响 main 稳定版**（长跑/日常修复继续在 main 走）。

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
| 部署游戏化服务 | 编译产物替换 /tmp/worldsim_run/worldsim（注意与 main 服务冲突，需停旧服务） |

## 游戏化方向（调研启发，参考 /tmp/refs/ 克隆项目）

1. **玩家介入层**（Phase 1）：长跑暂停时用户可对世界发指令，事件 Agent 响应
   - 参考：mnehmos-engine 的 "watch or intervene"
2. **机制层**（Phase 2）：
   - 战斗回合：stats 当检定值，骰子回合制（参考 ai-gamestudio combat 插件）
   - 关系档位：Relationship 值 → 陌生/熟识/友好/亲密 + 变化原因（参考 social 插件）
   - 任务系统：段落 milestones → 可接任务，truth_tag 校验（参考 mnehmos quests）
3. **游戏化分支的原型接口**（规划）：
   - `POST /api/player/act`：玩家指令注入
   - `GET /api/player/state`：可玩状态面板（stats/关系/任务）
   - 回合暂停：事件决策点停下等玩家选择

## 参考项目（已克隆 /tmp/refs/）

- `mnehmos-engine`（TypeScript，2万行）：确定性引擎校验 + 互动区域 + 经济系统 + DM 层
- `ai-gamestudio`（Python+插件）：combat/inventory/social 游戏化机制插件
