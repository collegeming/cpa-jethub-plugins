# atomcode — AtomCode（AtomGit）

把 AtomGit 的 AtomCode 免费套餐接进 CLIProxyAPI。

- 登录：AtomGit 账号的浏览器 OAuth，由 `acs.atomgit.com` 代理（`/auth/login` → 轮询 `/auth/check` → `/auth/token`）。插件**不监听本地回调端口**，回调落在代理自己的域名上。
- 推理：上游本身就是 OpenAI Chat Completions，所以执行器声明 `chat-completions` 入出并**原样转发**——消息、工具、`reasoning_effort`、图片分片都不经过任何转换。
- 模型目录：`GET /coding-plan/models-v2?plan_type=<档位>`，只注册 `plan_available=true` 的条目；档位从 `status-v2` 的套餐名推断。
- 领取：`POST /coding-plan/claim-v2`，按 Max → Pro → Lite 依次尝试，命中即停。
- 用量：`GET /coding-plan/usage`（近 60 天）。

## 两个必须知道的约束

### 1. 不要给网关发 `atomcode/*` 的 User-Agent

官方客户端会给聊天请求加一套闭源签名（`atomcode-codingplan-crypto`，开源源码树里只有 `unreachable!()` 占位）。网关按 **User-Agent** 决定是否要求这套签名。实测（同一 token、同一请求体，只改 UA）：

| `User-Agent` | 结果 |
| --- | --- |
| `atomcode/5.2.0`（官方客户端） | `403 {"detail":{"code":"ATOMCODE_SIG_MISSING"}}` |
| 任意其它值（含本插件的 `cpa-jethub-atomcode/0.1.0`） | `200`，正常推理 |

因此：

- `AdapterUserAgent` 是**功能性**常量，不是外观问题，`upstream_test.go` 专门守这条；
- 网关固定为 `https://api-ai.gitcode.com/v1`（官方默认的 `llm-api.atomgit.com/v1` 要求签名，用不了）；
- `models-v2` 每个条目自带的 `base_url` 指向需要签名的官方网关，插件只展示不采用。

顺带一个会导致误判的现象：带官方 UA 时，不在 `models-v2` 名单里的模型会返回 `403 model is not enabled for codingplan '<档位>'`。**这不是真的权限判定**——同样的模型换个 UA 就能正常返回 200。

### 2. 续期会轮换凭据

`POST /oauth/refresh` 会同时下发新的 `access_token` 和新的 `refresh_token`，并且**旧 access_token 立即失效**（实测旧令牌随后返回 `401 {"message":"401 Unauthorized"}`）。所以：

- 续期必须把新的一对写回，丢掉新的 refresh token 就等于让账号退化成需要重新登录；
- 续期做了单飞（`internal/jethub/authrefresh`）：两个并发续期会互相作废，第二个写入还会覆盖掉第一个的 refresh token；
- 到期时间只能由 `created_at + expires_in` 推出——AtomGit 的访问令牌不是 JWT，没有 `exp` 可读。

## 设置

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `enabled` | `true` | 启用插件 |
| `priority` | `0` | 凭据调度优先级 |
| `gateway_base` | `https://api-ai.gitcode.com/v1` | 聊天网关；**不要**改成官方默认网关 |
| `broker_base` | `https://acs.atomgit.com` | 登录代理 |
| `codingplan_api_base` | `https://api.gitcode.com/api/v5` | 模型目录／套餐／领取／用量 |
| `discover_models` | `true` | 关闭后只用内置快照 |
| `model_prefix` | `true` | 是否把账号 ID 作为模型前缀 |
| `model_cache_ttl_ms` | `7200000` | 模型目录缓存时长 |
| `plan_type` | `auto` | 查询档位：`auto` 时按 `status-v2` 的套餐名推断 |
| `login_timeout_ms` | `300000` | 单次登录会话存活时长 |
| `refresh_window_seconds` | `3600` | 到期前多久续期 |

## 页面

| 路径 | 用途 |
| --- | --- |
| `/status` | 账号列表、凭据有效期、套餐额度、可用模型 |
| `/login` | 两步式浏览器登录（`action=start` → `action=poll`） |
| `/checkin` | 套餐状态 + 领取 + 近 60 天用量；**只有带 `action=claim` 才真的领取** |

三个入口（新建账号 / 重新登录 / 去登录）都挂在状态页上，与其它渠道一致。

## 已知边界

- `claim-v2` 没有幂等标记：重复领取会再次返回 `success:true`，因此一键领取会如实报「已领取」而不是假装刚发放。
- 免费套餐的额度是**调用次数 / 滚动窗口**（实测 200 次 / 5 小时），不是 token 额度；接口不返回 `x-ratelimit-*` 头，限额只能通过 `status-v2` 观察。
- 网关在参数不被接受时会返回 **HTTP 200 + 正文 `参数错误`**。插件在非流式与流式两条路径上都把它识别成真实错误，不会把这句话当成模型的回答透传出去。
