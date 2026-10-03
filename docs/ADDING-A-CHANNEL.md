# 新增一个渠道：完整开发与接线清单

本文是「把一个新渠道接入本仓库」的**唯一清单**。它存在的理由很直接：这个仓库里，
一个渠道能不能用，取决于**十来个互不相邻的地方**同时正确——插件写好了但没接进 hub、
模型改名漏了、签到响应少一个字段，都会表现为「页面上一句看不懂的提示」，而不是编译错误。

按顺序做，最后跑 §7 的验收清单。

---

## 0. 先做上游侦察，再写一行代码

**先直接打上游**，不要用 CPA 的 `/v1/models` 判断上游有什么。那个列表已经被宿主的
`oauth-excluded-models` 和 `oauth-model-alias` 过滤/改名过了，用它反推上游结论必然出错。

需要确认的五件事：

| 问题 | 怎么确认 |
|---|---|
| 鉴权方式 | OAuth / 设备码 / 浏览器回调？令牌会轮换吗？到期怎么算（JWT `exp`？`created_at+expires_in`？服务端下发？） |
| 聊天端点与协议 | 是 OpenAI Chat Completions（可 `chat-completions` 直通）还是私有协议（要 `translate.go`）？ |
| 模型目录 | 哪个接口下发？是否分档位？`plan_available` 之类的权益字段怎么算？ |
| 可领取的东西 | 有没有签到/领取端点？幂等吗？**要不要验证码？** |
| 身份指纹 | 请求头里哪些是强制的？服务端是否按 UA/指纹区别对待？ |

把每一条的**原始请求 + 原始响应**记进 `docs/PORTING.md` 或插件注释。没有原始证据的结论
不要写进代码注释——这个仓库的注释会被后来人当文档读。

---

## 1. 插件骨架

```
plugins/<id>/
  main.go         cgo ABI（从任一现有插件复制，只改包注释）
  plugin.go       注册、方法表、管理路由表
  config.go       ProviderKey、端点、超时、ConfigFields()
  credential.go   凭据形状、Encode/Parse、到期判定
  auth.go         auth.identifier/parse/login.start/login.poll/refresh
  models.go       模型目录（实时 + 兜底）
  executor.go     executor.* 路由
  upstream.go     出站请求构造、响应/流失处理、错误映射
  errors.go       上游错误分类
  management.go   management.handle 的 JSON 分支
  pluginui.go     HTML 页面
  freshness.go    用前续期（internal/jethub/authrefresh）
  *_test.go       见 §6
```

必需能力（`plugin.go` 的 `abiboot.Capabilities`）：

```go
ModelRegistrar: true, ModelProvider: true, AuthProvider: true,
Executor: true, ExecutorModelScope: pluginapi.ExecutorModelScopeOAuth,
ExecutorInputFormats:  []string{"chat-completions"},   // 按上游协议声明
ExecutorOutputFormats: []string{"chat-completions"},
QuotaProvider: true, ManagementAPI: true,
```

**上游就是 OpenAI Chat Completions 时不要写 `translate.go`。** 直通比翻译少一整类 bug：
请求体除模型名外原样转发，消息、工具、`reasoning_effort`、图片分片都不会在翻译里丢失。

---

## 2. 登录入口三件套（最常漏）

每个渠道的状态页必须同时提供三个入口，缺一个都会让用户走进死路：

| 入口 | 作用 | 位置 |
|---|---|---|
| **新建账号** | 加第二个账号，链接带 `plugui.AddAccountQuery`（`add=1`） | 状态页「全部账号」卡片 |
| **重新登录** | 替换已有凭据，链接带 `auth_index` | 状态页账号卡片 **和** 账号列表每一行 |
| **去登录** | 一个账号都没有时的首次登录 | 状态页空账号卡片 |

「凭据无法读取」这类错误卡片上也要给「重新登录」。

测试要断言**数量**而不是存在性：`strings.Count(html, "login?auth_index=idx-1") >= 2`。
只断言「包含」的话，删掉其中一处不会变红——这个坑已经踩过一次。

---

## 3. 模型命名：对外只暴露统一名称

用户按统一词汇选模型（`DeepSeek-V4.1-Flash`、`GLM-5.3-Flash`…）。上游 id 与它不一致时，
**在插件里改名，不要写 `oauth-model-alias`**：

```go
// plugins/atomcode/models.go
var canonicalModelNames = map[string]string{
    "glm5.3-flash":   "GLM-5.3-Flash",
    "qwen3.8-27b":    "Qwen3.8-27B",
    "deepseek-flash": "DeepSeek-V4.1-Flash",
}
```

理由（实测，见 `plugins/atomcode/models.go` 注释）：`oauth-model-alias` **无法可靠地把一个
已被占用的名字交给第二个 provider**。同一个目标名被多个 provider 映射时，胜出方在重载之间
会变；曾出现 `atomcode` 的 `qwen3.8-27b` 与 `cline` 的 `Qwen3.8-27B` 同时存在——同一个模型
两个条目。而**插件自己发布的名字**会像 `plugins/zcode/models.go` 记录的那样，与其它渠道的
同名模型合并（CPA 把同名模型合成一个入口，多凭据分担流量），正是我们要的效果。

`oauth-model-alias` 仍然适合**引入一个全新名字**的场合。

两条硬性要求：

1. **双向映射**。对外发布 `Qwen3.8-27B`，发给上游的必须还是 `qwen3.8-27b`——
   忘了反向映射，上游会用「参数错误」之类的方式静默拒绝，而不是报错。
2. **改了名要验证推理**，不是只看 `/v1/models`。三个模型名各发一次请求。

> 同名合并是**有意**的：`GLM-5.3` / `GLM-5.3-Flash` 由多个渠道共同提供，合并后增加容量而不是
> 制造新 id。渠道隔离靠下游用不同的 API Key（key-provider-access），不是靠模型名后缀。

---

## 4. 接进 hub 的渠道总览

`plugins/hub/targets.go` 的 `targetCatalogue()` 是**用户看得见的渠道列表**，新渠道不登记就
不会出现在 Jet Hub 里。

```go
{
    ID: "atomcode", Label: "AtomCode（AtomGit）", Icon: brandicons.AtomCode,
    Support:     supportJSON,                       // supportJSON / supportStatusHTML / supportNone
    CheckinPath: "/checkin",
    CheckinQuery: url.Values{"action": {"claim"}},  // ⚠ 必填项，见下
    Note: "...",
},
```

- `CheckinQuery` **必须与插件自己的路由约定一致**：插件的 `/checkin` 只在
  `action=claim` 时真的写数据，这里就必须带 `action=claim`。带错的话一键动作会「看起来成功、
  什么都没领」。
- 该渠道没有签到端点时用 `supportNone` 并写清原因，不要为了好看硬接。
- 加完渠道后，`plugins/hub/orchestrator_test.go` 的 `checkinRoutes()` / `providerStatusRoutes()`
  / `want` 表 / summary 计数，和 `overview_test.go` 的 `channelOrder` + `wantIcons` 都要同步，
  否则测试会红——这是**故意的**，它保证新渠道不会漏接。

---

## 5. 页面 JSON：宿主只认固定字段

hub 的行是从各插件自己的 `status?format=json` 渲染的，读的是**固定字段路径**
（`plugins/hub/overview.go` 的 fact builders）。一个都不提供时，渠道行会显示
「provider 只返回了本页不展示的配置字段」——装上了，但看起来是死的。

| 字段 | 渲染成 | 说明 |
|---|---|---|
| `model_count` | 模型 N | 不给就没有模型信息 |
| `expires_at` / `expires_at_ms` / `expired` | 有效期至 … | 同时给人和机器两种写法 |
| `accounts[]` + `account_count` | 每账号一行 | 每行自带 `expires_at_ms` / `expired` |
| `accounts[].remaining` + `.total` | 剩余 R / T | 额度；注意单位要在文案里说清 |
| `daily_checkin.*` / `checkin.*` | 签到状态 | 有签到语义时给 |
| `message`/`server_message`（签到响应顶层） | 结果说明 | **见下，最容易漏** |

### 5.1 签到响应必须有顶层 `message`

hub 的 `interpretCheckinJSON` 只读**顶层** `message`（以及 `claim.message`、`server_message`）。
把说明塞在 `outcomes[].message` 里等于没写——用户看到的只有一个 `failed`。
失败时 `message` 必须是**可执行的那句话**（原因 + 补救方式），不是状态词。

### 5.1b 不能成功的动作，不要提供入口

这条比「写闸门」更靠前：如果一个动作**注定失败**，正确的做法是**不提供它**，并在总览里如实标注「不支持」——
而不是每天让用户点一次、看一次同样的报错。

判断标准很简单：**这个失败能不能被用户或插件消除？**

| 情况 | 做法 |
|---|---|
| 上游根本没有这个接口（cline） | `supportNone` + Note 说明 |
| 上游有接口，但需要插件拿不到的东西（ZCode 的阿里云验证码） | `supportNone` + Note 说明，并把相关路由／页面／代码一起删除 |

第二种情况还有个额外要求：**把不能用的代码删掉，而不是留在仓库里**。留着会让人以为它只是没接好，
下次有人再花一轮去调它。ZCode 的验证码通道就是这么处理的：试过、失败在场景未授权来源，
于是路由、页面、领取代码与验证码组件全部移除，只把结论写进部署文档。

HUB 的 `failed` 计数是给**真的意外**用的。一个每天必然失败的渠道会长期占用它，把真正的故障淹掉。

**第三种情况：能力协商的缺口——降级，而不是拒绝。**

上面两种情况是"这个动作不该有入口"。还有一种更隐蔽的：动作本身合理，只是**当前这条上游做不了**，而同一个模型名在别的上游做得了。

判据是：**这个失败能不能靠池化自动绕开？** 能绕开时，正确做法是在**发往该上游之前**降级（剥掉它吃不下的一部分），而不是把错误抛上去——因为抛上去会让整个名字被一条请求打挂，而用户本可以从别的渠道拿到完整答案。

典型是文生图之下的模态：`DeepSeek-V4.1-Flash` 由四个上游共用，codearts 收到图片会整单拒绝（`InferHub.001001020.406: The request model is not multimodal`），其余上游能正常读图。CPA **不按模态路由**，所以请求命中谁全看轮询。

```go
// 上游吃不下 → 本地降级，并记 warn（不要静默）。
// imagesStripped 是 translator 的第 4 个返回值——共享的 openai.Request
// 刻意不做扩展，所以剥图结果走返回值而不是挂在请求结构上。
imagesStripped := stripImagesForTextModel(request)
```

| 做法 | 结果 |
|---|---|
| 原样转发 | ❌ 整单 406，用户看不到任何东西 |
| 返回 400 拒绝 | ❌ 语义更糟：CPA 判为请求方错误会**中断跨渠道故障转移**，用户彻底拿不到答案 |
| 本地剥图 + 记 warn | ✅ 该上游答得诚实（明确说看不到图），其它上游照常给出完整答案 |

⚠️ **降级必须留痕**：warn 日志要写清"哪条上游剥了什么"。否则下次现象就是"有时能看图、有时看不到"，排查者会以为是随机故障——本仓库的 codearts 剥图日志（`剥离了请求中的图片`）就是为此而加。

**注意反向的坑**：不要为了"更好看"把该上游从名字里摘掉。摘掉会失去它的额度与并发，降级能同时保住两者。

### 5.1c 静态回退表只是元数据，永远不是目录

新渠道通常要内嵌一份**静态模型表**，补上游不给的字段（上下文窗口、输出上限、是否支持图片）。这类表是**某一刻的快照**，有一个容易致命的用法：把它当成"可调用模型"的来源。

**只有在线目录能决定一个 id 是否可调用。** 上游结束一次免费推广、或下架一个模型时，本地不会有任何提示；若静态表仍把该 id 并入公开列表，就会出现"列表里有、上游没有"的幽灵模型——每一次调用都必然失败。

实测（cline，2026-10-02）：`cline-free/gemini-3.8-flash` 的推广被上游结束后，`/v1/models` 464 条里零命中、直连 404 `model not found`，而 CPA 仍在发布它，客户端侧表现为每次 `HTTP 502`。

因此静态表条目必须**被某个在线来源背书**才发布：

| 情况 | 做法 |
|---|---|
| 该 id 出现在在线目录里 | 发布，静态表只贡献元数据（名字/上限/模态） |
| 该 id 只存在于静态表 | **不发布**，并记一条 warn 说明它已下线 |
| 在线目录整体不可用（请求失败/空响应） | 整表照旧发布——"列表为空"比"列表略旧"更糟 |

⚠️ 两种目录的**权威范围不同**：cline 的 `/api/v1/models`（464 条）里**一条 `cline-free/*` 都没有**，免费家族只能由 `recommended-models` 的 `free` 数组背书。判定时要按名字族区分来源，不能用一个来源去否定另一个的结论。

⚠️ **空响应不等于权威**：端点返回 200 但内容为空/不可解析时，与请求失败同等对待，都不得触发删除。

### 5.1d 目录刷新接进 `internal/jethub/catalog`

新渠道接完目录发现之后，要一并接上「自动刷新 + 手动刷新按钮」。共享包已经把两层机制封好了，插件只需提供自己的发现函数。

**先分清刷的是哪一层**：

| 层 | 内容 | 谁能改 | 触发方式 |
|---|---|---|---|
| ① 插件缓存 | 上游目录端点返回的清单 | 插件 | 清缓存重拉，零副作用 |
| ② 宿主注册表 | `GET /v1/models` 返回的内容 | CPA | 宿主重新注册该 provider |

插件**没有**"让宿主重新注册我的模型"这种调用能力——`sdk/pluginabi` 里没有对应方法。宿主只在进程启动、`config.yaml` 变更、**凭据文件语义变化**这三种时机重新注册，第三种是插件唯一能自己触发的。

接法（照 `plugins/cline/` 抄）：

1. **刷新函数**：先 `reset()` 自己的缓存，再走自己正常的发现路径（`discoverModels` / `catalogueForAuth`），返回 `catalog.Outcome{Models, Changed}`。`Changed` 由刷新前后的模型 ID 集合对比得出。
2. **自动刷新**：`catalog.NewScheduler` + `Scheduler.Start(Request{...})`，Request 里设 **`PublishOnChange: true`** 且 **`AuthName` 留空** —— 目录变化时才发布到 ②。在 `Configure` 里按 `model_refresh_ms` 启动，`Quiesce`/`Shutdown` 里 `Stop()`。
3. **手动按钮**：管理页加 `plugui.Action{Label: "刷新目录", Query: "action=refresh-catalog", Kind: "primary"}`，处理函数里用 `catalog.Run` 并传 `AuthName`（该账号的凭据文件名）——手动路径**无条件**写 ②，因为用户按下按钮就是期望重新注册。

⚠️ **自动刷新只在目录变化时发布**：`Request.PublishOnChange = true` 让宿主只在目录真的变了时才重新注册。宿主自己续期令牌时也在写同一个凭据文件，一个每周期都无条件写的循环会和它反复互相覆盖；等到真变化才写，这个冲突就从"持续"变成"偶发"。目录稳定时自动路径是零写入的。

⚠️ **不要用固定的 `AuthName` 做自动发布**：`Configure` 发生在登录之前，那一刻还没有账号，名字取到空串后自动发布就永远不会触发。留空 `AuthName` 并用 `PublishOnChange`，发布目标由共享包在每个周期从 `host.auth.list` 现取。

⚠️ **回退到静态表不算刷新成功**：若上游全挂时插件会回退到静态表，该次刷新必须返回 error。把回退报成成功，等于告诉运维"上游确认了这个目录"，而它根本没应答。

⚠️ **`h.SaveAuth` 的 `Incoming` 可能为空**：登录轮询等回调载荷里不带旧凭据文件，此时 `SaveAuth` 会自己回读宿主持有的文件来保住 `priority`/`weight` 等宿主字段（见 `internal/abiboot/host.go`）。自定义写凭据路径时不要绕开它。

### 5.2 只有显式 `action=claim` 才允许写

页面加载、监控轮询、hub 的一次状态读取都会 GET 这个路由。没有这个闸，任何一次轮询都在
替你领取。测试要断言「无 action 时**一次写请求都没发**」。

一个反例值得记住：把**一次性**奖励放进每天都会按的按钮，必须双重设闸——先读状态（已领就
一次写都不发），再依赖服务端幂等标志兜底。

### 5.3 日志字段有白名单，自定义 key 会被静默丢弃

`h.Log(level, message, fields)` 里的 `fields` **不是自由格式**。CPA 的控制台格式化器只打印白名单里的字段名：

```go
// internal/logging/global_logger.go:55-59（CPA v8.0.4）
var logFieldOrder = []string{
	"provider", "model",
	"plugin_id", "plugin_name", "source_id",
	"version", "active_version", "retired_version", "overwritten",
	"mode", "budget", "level", "original_mode", "original_value", "min", "max", "clamped_to", "error",
	"credential", "connection", "proxy_scheme", "remote_transport",
	"media_session_id", "call_id", "peer", "state", "reason",
}
```

白名单之外的 key **不报错、不警告，直接从日志行里消失**。实测：写 `{"models": "cline-free/gemini-3.8-flash"}` 的告警，落到日志里只剩 `provider=cline`——最关键的模型名没了。

**规则**：任何不在上表的诊断信息，一律拼进 `message`；只有上表里的名字才放进 `fields`。

```go
// ❌ 模型名会消失（models 不在白名单）
h.Log("warn", "静态目录表条目已下线", map[string]any{"provider": ProviderKey, "models": id})
// ✅ 诊断信息在 message 里，字段只用白名单内的
h.Log("warn", "静态目录表条目已下线，不再发布："+id, map[string]any{"provider": ProviderKey})
```

⚠️ 这条**无法用单测发现**：插件侧日志是 fire-and-forget 的 RPC（`abiboot.Host.Log` 丢弃返回值），单测里既不经过 CPA 的格式化器，也看不到被丢弃的结果。唯一可靠的验收方式是在真实容器日志里 `grep` 一次、确认内容完整。同理适用于 `error` 之外所有自定义字段——`provider` / `model` / `error` 是最常用的三个安全 key。

---

## 6. 测试与反向验证

每个插件至少覆盖：凭据到期/轮换、登录流程两步、模型目录与命名、执行器请求构造与响应处理、
管理页三个入口、签到闸门。

**每条新断言都要做反向验证**：故意破坏它保护的行为，确认测试变红，再恢复。
本次会话里，未经反向验证的断言中有两条其实是假绿的（`strings.Contains` 而不是计数；
测试只调了辅助函数、没走真实接线）。做法：

```bash
cp plugins/<id>/x.go /tmp/x.bak
# 破坏行为
go test ./plugins/<id>/ -run TestThatThing   # 必须 FAIL
cp /tmp/x.bak plugins/<id>/x.go
```

---

## 7. 收尾：配置、文档、发布、验收

**配置**（`config.yaml`）

- `plugins.configs.<id>`：`enabled`、`model_prefix: false`（本部署统一关闭前缀）、渠道特有项。
- `oauth-excluded-models`：想收敛模型列表就按 provider 列 id。⚠ 排除匹配的是**插件上报的
  原始 id**（先排除、再别名）。
- 改完配置**热重载即可**；涉及 `.so` 变更必须重启容器。

**文档**

- `README.md`：插件状态表加一行；渠道数、执行器格式表同步；有取舍写进「需要知情的实现取舍」。
- `docs/DEPLOYMENT.md`：`plugins.configs` 配置块、登录方式表、渠道特有说明。
- `plugins/<id>/README.md`（可选，渠道有明显陷阱时建议写）。

**发布**

- `internal/jethub/brandicons/brandicons.go` 加厂商图标（用厂商自己的素材，嵌 data URL）。
- `registry.json` 加条目。
- `scripts/build.sh` 与 `scripts/release.sh` 的 `PLUGINS` 加 id（**变体**还要加
  `variant_source_dir`）。
- `.gitignore` 加 `/<id>`（防止 `go build` 掉在仓库根的裸二进制）。
- `VERSION=x.y.z bash scripts/release.sh` → tag → `gh release create`。

**验收清单**

- [ ] `/v1/models` 里能看到该渠道，且**只有统一名称**（没有小写/带斜杠的原始 id 残留）
- [ ] 每个模型名都能真的推理成功（**改名后尤其要测**）
- [ ] Jet Hub 渠道总览里有这一行，状态、模型数、有效期都正常
- [ ] 一键签到结果里该行是正确类别，且有可读的 `message`
- [ ] 无 `action` 的裸请求**不产生任何写操作**
- [ ] 状态页三个登录入口齐全，且计数断言通过
- [ ] `go build ./...`、`go vet ./...`、`gofmt -l`、`go test ./...` 全绿
- [ ] 部署后重启容器再验一遍（热重载不覆盖 `.so`）

---

## 8. 「模型没出现」排查顺序

按这个顺序查，每一步都能排除一类原因：

1. **宿主排除**：`oauth-excluded-models` 里有没有这个 provider 的条目？
   （实测过一次教训：模型不是上游撤了，是被本地排除清单过滤了。）
2. **宿主别名**：`oauth-model-alias` 是否把它改名成了别的名字？看 `/v1/models` 里是否有
   目标名，且 `owned_by` 是别的渠道。
3. **插件目录**：`status?format=json` 的 `model_count` 是多少？插件自己上报了几个？
   （插件数 ≠ 宿主数时，差额通常来自第 1、2 步。）
4. **上游目录**：直接打上游的目录接口，看服务端到底下发什么。**不要**用 `/v1/models` 反推。
5. **上游受理**：目录里没有但网关仍可能受理（实测 `deepseek-flash` 从目录消失后仍返回 200）。
   这类模型用 `extra_models` 之类的显式配置补，不要硬编码进默认目录——「能调用」不等于「可用」。

---

## 9. 一句话版本

> 插件写完只是**一半**。剩下的一半是：登录入口、统一命名、hub 登记、宿主能读的 JSON 字段、
> 签到的 `message` 与写闸门、配置排除/别名、文档与发布脚本——以及**每条断言的反向验证**。
