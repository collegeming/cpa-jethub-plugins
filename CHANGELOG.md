# 变更记录

本文件记录每个发布版本的用户可见变更。正文文档只描述当前生效的状态；本文件是它们的去处。

## v0.11.0

### 统一「模型目录」卡片：列出模型、可筛选、显示上游原名

11 个渠道的状态页现在都有一张由共享组件 `plugui.CatalogueCard` 渲染的「模型目录」卡片。

在此之前各插件各行其是：有的列出模型清单，有的只写「来源 / 缓存 / 自动刷新」三个字段——**恰恰没有回答这张卡片存在的意义**（这个渠道提供哪些模型）。codearts 的截图就是后者：卡片上方明确写着「8 个模型」，卡片里一个都没列。

现在每张卡片包含：

- **完整模型清单**，逐条列出；
- **筛选框**（模型超过 12 条时出现）：纯本地 JS，输入片段即可收窄，显示「匹配 N / 总数」，无匹配时给出提示。少于 12 条不渲染——列表本来就一屏放得下时，多一个控件只是碍事；
- **「刷新目录」按钮**（沿用 v0.10.0 的刷新能力）；
- **上游本身的模型名**作为主标签。

### 名称显示口径

| 字段 | 含义 | 显示位置 |
|---|---|---|
| `Native` | 上游厂商自己的模型名 | 主标签 |
| `ID` | 本部署路由用的名字（改名后／带账号前缀） | 仅当与 `Native` 不同时，作为「请求用名」一行 |

改名表（`publicModelID` / `canonicalModelName`）的结果属于 `ID`，不再是主标签。`oauth-excluded-models` 与 `oauth-model-alias` 在宿主层生效，卡片不重复实现，照上游目录如实列出。

### 修复：卡片去重按路由 id，不按显示名

最初按显示名去重，实测在 cline 的线上目录上**静默删除了 12 个真实模型**：上游让两个不同模型共用一个显示名，例如 `qwen/qwen3.8-27b` 与 `qwen/qwen3.8-27b:free` 的显示名都是 `Qwen3.8 27b`，这样的组有 11 组。改为按路由 id 去重后，页面行数与目录条数一致（484 = 484）。

回归测试：`internal/jethub/plugui/catalogue_test.go` 的 `TestCatalogueCardKeepsDistinctModelsWithTheSameLabel`。

### 实测

- **行数一致**：cline 页面 484 行 = 卡片声明 484 条。
- **筛选可用**：输入 `deepseek` → 21 行（「匹配 21 / 484」）；输入 `qwen3.8-27b` → 2 行，两个同名模型都在；输入无关串 → 0 行并显示「没有匹配的模型。」；清空 → 恢复 484 行。
- **脚本确实执行**：先注入探针确认页面标题被改为 `PROBE-JS-OK`，再据此实现筛选。CPAMP 的插件 iframe 同源、无 `sandbox`、无 CSP。无脚本时页面**全部行仍然可见**（筛选只是收窄已渲染的行，不负责渲染）。

### 修复：两个 `-shuffle=on` 下的测试隔离缺陷

两者都是**既存**问题（在本次改动前的 `dfc117b` 上同样复现），与卡片功能无关，但会随机让 CI 变红：

| 测试 | 症状 | 根因 | 修法 |
| --- | --- | --- | --- |
| `plugins/qoder` `TestLoginPagePollWithoutStateIsAnError` | 8 次跑 6 次失败 | 无 `state` 的轮询会回退到包级 `loginSessions` 里最新的 PENDING 会话，另一个测试留下的会话让"缺少 state"永不触发 | 测试先 `shutdownLoginSessions()`，并 `t.Cleanup` |
| `plugins/raccoon`、`plugins/trae` `TestConfigureStartsAndStopsTheScheduler` | 8 次跑 2 次失败 | `runs` 计数属于包级单例，前一个测试的 tick 会被读成"这个循环已经跑过" | 共享包新增 `Scheduler.ResetCounters()`，两个测试在断言前清零 |

修后 `-shuffle=on` 连跑 10 次全绿。

### 文档

`ADDING-A-CHANNEL.md` 新增 §5.1e（卡片必须列出模型、名称口径、按 id 去重、页面不触网）。

## v0.10.0

### 新增：模型目录自动刷新 + 手动刷新按钮

11 个渠道插件（cline、codearts、codebuddy、qoder、trae、raccoon、loomy、lobsterai、minimax、zcode、atomcode）都获得：

- **配置项 `model_refresh_ms`**（默认 `0` = 关闭）：按间隔在后台重新拉取上游目录，并**在目录真的变化时**自动通知宿主重新注册，使 `/v1/models` 无需人工干预即可跟上。目录无变化的周期不写任何凭据文件。
- **状态页「刷新目录」按钮**：立即刷新缓存，并**无条件**发布到宿主，使 `GET /v1/models` 在约 1 秒后反映新目录。页面分别报告"目录是否变化"与"是否已通知宿主"。
- **`GET /v0/management/plugins/<渠道>/catalog?format=json`**：上述按钮的机器可读形式。

支持三种间隔写法：毫秒数、时长字符串（`"30m"`）、`0`（关闭）。

### 修复：`SaveAuth` 在回调载荷不带凭据文件时抹掉宿主字段

`host.auth.save` 替换整个凭据文件，插件未定义的成员靠 `Incoming` 回填。登录轮询、后台刷新等路径的回调载荷**不带**旧凭据文件，`Incoming` 为空，`MergePreserved` 于是原样返回插件的字节——文件里宿主拥有的 `priority`、`weight` 被静默删除，凭据的路由档位重置为默认值。

现在 `SaveAuth` 在 `Incoming` 为空时先通过 `host.auth.list` + `host.auth.get` 回读宿主持有的文件作为基底，再合并。回读失败不阻断写入（首次登录本就没有已存文件）。

回归测试：`internal/abiboot/saveauth_incoming_test.go`。

### 文档

- `DEPLOYMENT.md` 新增 §3.10「刷新模型目录：自动发现 + 手动按钮」——两层缓存模型、自动与手动各自的作用范围、结果页每一项的含义。
- `ADDING-A-CHANNEL.md` 新增 §5.1d——新渠道接入目录刷新的步骤与三条禁令。

### 未在本版改动（已评估）

`plugins/codebuddy/models.go` 的 `reconcileWithFallback` 把上游列表按兜底表做白名单校正，因此厂商新增但**未被任何 agent 声明**的模型不会进入 `/v1/models`。这是有意行为，上游源码给出了原因：服务端按认证上下文返回模型，CLI token 拿到的集合既可能残缺（实测国际版 13 vs IDE 20）又可能混入不可用别名，黑名单只能做减法、补不回漏掉的模型。厂商新增模型的正常通路是把它挂到某个 agent 上（`agentReferenced`），该通路工作正常。改动会回归上游已修复的用户故障，故保持现状。

### 实测记录

两层机制在运行实例上验证：

- **自动路径（稳定期零写入）**：`model_refresh_ms: 30000` 下连续 4 个周期，日志各记一行 `cline 目录刷新：目录无变化，共 484 个模型；目录未变化，无需通知宿主`，凭据文件 mtime 全程未变。
- **自动路径（启动即最新）**：宿主启动时调用 11 次 `model.for_auth` 完成注册，插件缓存随之预热；因此重启后的首个周期判定"无变化"、不写文件，`GET /v1/models` 仍返回 71 条且已下线模型不再出现。
- **手动路径**：写入发布成员后，宿主日志出现 `auth file changed (WRITE)`，1 秒内重新调用插件的 `model.for_auth`（以静态目录告警为探针，计数 +1）。
- **对照**：同样的探针下 25 秒不写入，重新注册次数为 0，排除巧合。

## v0.9.0

- 静态回退表条目只有在仍被某个在线来源背书时才发布，修掉上游结束免费推广后仍被发布的幽灵模型（`cline-free/gemini-3.8-flash`）。
- 新增 `staticAuthority`、`staleStaticIDs` 与 `mergeCatalogue` 的静态条目过滤。
- 首次产出多平台发布物（15 插件 × 5 平台 + 校验和），修复 CI 从未构建非 Linux 平台的问题：macOS `declare -A`、退役的 `macos-13`/`macos-14` runner、Windows 导入库被带点版本号解析成浮点数。
- `DEPLOYMENT.md` 新增 §3.9；`ADDING-A-CHANNEL.md` 新增 §5.1c 与 §5.3（日志字段白名单）。
