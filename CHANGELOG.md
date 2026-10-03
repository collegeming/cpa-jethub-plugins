# 变更记录

本文件记录每个发布版本的用户可见变更。正文文档只描述当前生效的状态；本文件是它们的去处。

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
