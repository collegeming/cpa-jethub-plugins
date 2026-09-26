# cpa-jethub-plugins

CLIProxyAPI（CPA）原生 Go 插件集合。每个插件把 Jet-Hub 的一个平台适配器移植为 `-buildmode=c-shared` 动态库，经 CPA 的插件 ABI 以 JSON envelope 提供登录、凭据续期、模型目录、执行器与配额能力。

## 架构

| 层 | 位置 | 职责 |
| --- | --- | --- |
| 共享 ABI 层 | `internal/abiboot` | envelope 编解码、能力注册、方法分发、宿主回调（`host.http.*`、`host.auth.*`、`host.log`） |
| 插件实现 | `plugins/<provider>/` | `main.go` 导出 4 个 C 符号；`plugin.go` 返回 `Registration` 与方法表 |
| 分发清单 | `registry.json` | 插件元数据清单（官方 `schema_version: 1` 格式，产物走本仓库的 GitHub Release） |
| 管理界面 | `internal/jethub/plugui` | 渲染管理路由的 HTML 页面，只消费宿主主题变量，无前端构建 |
| 构建 | `scripts/build.sh` | 逐插件产出 c-shared 动态库，文件名可直接安装 |
| 打包 | `scripts/release.sh` | 产出发行 zip 与 `checksums.txt` |

每个插件目录是**单一 Go 包 `package main`**：`main.go` 承载 cgo 胶水并导出 `cliproxy_plugin_init`、`cliproxyPluginCall`、`cliproxyPluginFree`、`cliproxyPluginShutdown`，其余 `.go` 文件承载业务逻辑。这样 `go build -buildmode=c-shared ./plugins/<provider>` 可以从该目录直接产出动态库。

依赖约束：标准库、CPA SDK（`github.com/router-for-me/CLIProxyAPI/v7/sdk/{pluginabi,pluginapi}`），以及 `gopkg.in/yaml.v3`（配置解析）与 `github.com/tetratelabs/wazero`（可选，用于 Qoder 的签名 WASM）。插件自身不建立网络连接——HTTP 一律经 `host.http.*` 由宿主执行。

## 插件状态

| 插件 ID | 显示名 | 管理页面 | 配置项 | 说明 |
| --- | --- | --- | --- | --- |
| `codearts` | CodeArts Agent | 状态、登录 | 9 | 华为云 CodeArts，`SDK-HMAC-SHA256` 签名，DSML 工具调用 |
| `codebuddy` | CodeBuddy | 状态、登录、签到 | 9 | 腾讯 CodeBuddy／WorkBuddy，四个产品共用一套适配器 |
| `qoder` | Qoder | 状态、登录、签到 | 12 | 阿里 Qoder，设备码登录；加密推理需可选 WASM |
| `trae` | TRAE | 状态、登录、签到 | 13 | 字节 TRAE，OpenAI↔SOLO 双向载荷转换 |
| `lobsterai` | LobsterAI（有道） | 状态、登录、签到 | 5 | 有道 LobsterAI，本地回环回调 + authCode 换取 |
| `cline` | Cline | 状态、登录 | 9 | Cline（cline.bot），WorkOS 设备码登录，标准 OpenAI 兼容推理 |
| `loomy` | Loomy（讯飞） | 状态、登录、签到 | 10 | 讯飞 Loomy，短信验证码登录，两个积分池 + 新手任务 |
| `hub` | Jet Hub | 状态、一键签到 | 4 | 编排型插件：读取各 provider 状态并一次点击完成全部签到 |
| `codebuddy-intl` 等 | CodeBuddy／WorkBuddy | 状态、登录、签到 | 9 | 见下方「产品变体」——同一份代码按产品构建的独立插件 |

七个适配器都实现了完整方法面（另有一个编排型的 `hub`）：`auth.identifier`／`parse`／`login.start`／`login.poll`／`refresh`，`model.register`／`static`／`for_auth`，`executor.identifier`／`execute`／`execute_stream`／`count_tokens`，`request.translate`、`response.translate`，`management.register`／`handle`，以及 `quota.identifier`／`describe`／`fetch`／`reset`。执行器统一声明 `chat-completions` 入出格式且 `executor_model_scope=oauth`，跨协议转换由宿主完成，插件不重复实现。

### 选择平台／区域

同一厂商的区域版本互不相通，凭据也各自独立，用配置项选择：

| 插件 | 配置项 | 取值 |
| --- | --- | --- |
| `codebuddy` | `product` | `codebuddy`（国内，默认）、`codebuddy-intl`（国际）、`workbuddy-cn`、`workbuddy` |

> **想让国内版与国际版同时可用**，靠 `product` 做不到：CPA 由 `.so` 文件名决定插件 ID，而一个插件只能注册一个 provider key（它决定了凭据文件名、模型前缀与执行器路由）。
> 因此本仓库用**同一份代码构建多个「产品变体」**，每个变体在构建期把 provider key、显示名与默认产品固定下来：
>
> | 变体 ID | provider key | 默认产品 | 登录域名 |
> | --- | --- | --- | --- |
> | `codebuddy` | `codebuddy` | CodeBuddy 国内版 | `copilot.tencent.com` |
> | `codebuddy-intl` | `codebuddy-intl` | CodeBuddy 国际版 | `www.codebuddy.ai` |
> | `workbuddy-cn` | `workbuddy-cn` | WorkBuddy 国内版 | `copilot.tencent.com` |
> | `workbuddy` | `workbuddy` | WorkBuddy 国际版 | `www.workbuddy.ai` |
>
> `scripts/build.sh` 会把这四个变体连同其余插件一起产出；`plugins.configs` 里按变体 ID 分别启用即可，每个变体有自己的凭据与模型前缀。
> 变体由 `-ldflags -X main.ProviderKey=... -X main.DefaultProduct=...` 注入，构建期固定，不需要（也不建议）在配置里再写 `product`。
| `qoder` | `region` | `qoder`（国际，默认）、`qoder-cn` |
| `trae` | `region` | `trae`（国内，默认）、`trae-intl` |
| `codearts` | `flow` | `oauth`（浏览器 PKCE，默认）、`ticket`（旧版票据轮询） |

其余字段（模型发现开关、超时、Max 模式、签到开关、通道选择等）在管理面板里都有中文说明，或见各插件的 `ConfigFields()`。

### 两个需要知情的实现取舍

- **Qoder 加密推理**：加密请求的签名头由 Jet-Hub 的 `qoder-auth-wasm.wasm`（约 292 KB 第三方编译产物）生成。本仓库**不包含**该二进制——再分发属于仓库所有者的授权决定。把 `wasm_path` 指向你本地的副本即启用加密路径；留空则只走公开的 OpenAI 兼容端点。
- **Loomy（讯飞）与其余七个都不同源**，有三点必须知情：
  - 它是唯一用**短信验证码**登录的：没有可打开的登录 URL，验证码在插件自己的页面里输入。而 resource 路由只派发 GET，页面不能用表单，所以动作用查询串链接（含数字键盘式的验证码输入）。
  - 它是唯一**不能续期**的：后端没有 refresh 端点，`auth.refresh` 只做有效性探测，失效即提示重新登录。这是如实标记，不是遗漏。
  - 积分是**两个池**（永久积分 + 每日赠送，消耗后不回补），分开显示；鉴权头也分两套——`/chat/completions` 用 `Bearer`，而 `/models`、`/points/*`、`/onboarding/*` **只认小写 `token` 头**，带错的那个返回 **HTTP 200** 加 `code:100002`，只看状态码会误判成功。
  它的账号接口用 HMAC-SHA1 签名，密钥是**参考实现内置的客户端凭据**（不是你的账号凭据）。若上游轮换该密钥，短信登录会失效而其余接口不受影响；配置项 `account_ak` / `account_sk` 可在不重新构建的情况下替换。
- **`hub` 是怎么跨插件工作的**：CPA 里一个插件不能直接调用另一个插件，管理 API 又需要插件拿不到的密钥。但**插件的 resource 路由不受管理密钥保护且派发 GET**，于是 `hub` 通过宿主的 HTTP 客户端回环读取各 provider 自己的 `status?format=json`（渲染侧边栏里那一行「Jet Hub」的渠道总览），并在显式点击一键签到时回环调用各 provider 自己的签到页，再汇总成一张表。两个前提：各 provider 页面支持 `?format=json`；以及 `host_base_url` 配置项——**宿主不向插件暴露自己的 HTTP 端口**（`HostConfigSummary` 没有 port 字段），所以只能配置，默认 `http://127.0.0.1:8317`。
  因为走的是宿主的 HTTP 客户端，它**会受 CPA 出站代理设置影响**：若代理拦截 `127.0.0.1`，回环调用会失败。
- **Cline 的 `workos:` 前缀是承载语义的**：鉴权头是 `Authorization: Bearer workos:<token>`，前缀**不能剥离**——同一个凭据 `Bearer workos:eyJ…` 返回 200，剥掉前缀后返回 401，而且错误信息会误导成「请升级 Cline 客户端」。登录走 WorkOS **设备码轮询**，**不监听任何本地端口**，因此不受本文档前述容器回调问题的影响。
- **Cline 的余额单位是推断值**：接口返回 `balance: 500000`，参考实现按 ÷100000 当作美元（微美元）展示，但这**没有任何来源证据**。本仓库把它做成配置项 `balance_divisor`，状态页同时显示原始值，用真实账号跑一次即可确定。
- **配置解析**：用 `gopkg.in/yaml.v3` 解析为映射后逐键宽松取值，因此 block 与 flow 两种 YAML 风格都生效，`no`／`off`／`yes`／`on` 等写法也可用，且单个坏值只损失它自己的默认值，不会让整份配置回退。

## 构建

`go` 默认不在 `PATH` 中，先导出构建环境：

```bash
export PATH=/home/colle/.local/go/bin:$PATH
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=off
export GOFLAGS=-mod=mod
export GOTOOLCHAIN=local
```

构建全部插件：

```bash
./scripts/build.sh
```

脚本以仓库根为工作目录，设置 `CGO_ENABLED=1`，对 `codearts`、`codebuddy`、`qoder`、`trae`、`lobsterai` 逐个执行 `go build -buildmode=c-shared`，产物按 CPA 的发现目录布局写入 `dist/`：

```text
dist/<goos>/<goarch>/<plugin>-v<version>.so      # linux
dist/<goos>/<goarch>/<plugin>-v<version>.dylib   # darwin
```

版本号取自各插件源码里的 `PluginVersion`／`Version` 常量，因此产物名与 `plugin.register` 上报的版本一致。

文件名的形式不是随意的：CPA 从**文件名去掉扩展名的部分**推导插件 ID，只在 `-v<版本>` 前后两段都合法时才剥离该后缀（`internal/pluginhost/platform.go`）。所以 `codearts_linux_amd64.so` 会被注册成插件 ID `codearts_linux_amd64`，导致 `plugins.configs.<id>` 与 `auth.identifier` 全部对不上。脚本直接产出 `<id>-v<version>.<ext>`，整个目录树可以原样拷入 CPA 插件目录：

```bash
cp -r dist/linux/amd64/. /path/to/cpa/plugins/linux/amd64/
```

`go` 路径可用 `GO=/path/to/go ./scripts/build.sh` 覆盖，目标平台由 `GOOS`／`GOARCH` 决定。cgo 会在产物旁同时生成同名 `.h`。某个插件构建失败时，脚本仍继续构建其余插件，在汇总表标出 `FAILED`，最后以非零状态退出。

常用命令：

```bash
go build ./...                                       # 编译全部包
go vet ./plugins/...                                 # 静态检查
go test ./...                                        # 单元测试
GOOS=linux GOARCH=arm64 ./scripts/build.sh           # 交叉编译（需要交叉 cgo 工具链）
```

## 安装

### 直接放置

1. 把动态库放进 CPA 的插件目录。默认目录是 CPA 工作目录下的 `plugins/`，枚举时只扫描 `plugins/` 与 `plugins/<goos>/<goarch>/` 两层。
2. 文件名决定插件 ID（见上文「构建」）。`scripts/build.sh` 已按 `<id>-v<版本>.<ext>` 产出，直接拷入即可；手工改名时务必保证 ID 就是 `plugins.configs` 里的键，例如 `codearts-v0.1.0.so` → ID `codearts`。
3. 在 CPA 配置中启用动态插件（默认关闭）：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    codebuddy:
      enabled: true
```

Linux 与 FreeBSD 用 `.so`，macOS 用 `.dylib`，Windows 用 `.dll`。

### 从 Release 安装

仓库的 [Releases](https://github.com/collegeming/cpa-jethub-plugins/releases) 提供打包好的动态库，zip 内只有动态库本身：

```bash
# 以 codearts 为例，替换成需要的插件与平台
curl -LO https://github.com/collegeming/cpa-jethub-plugins/releases/download/v0.1.0/codearts_0.1.0_linux_amd64.zip
unzip -o codearts_0.1.0_linux_amd64.zip -d /path/to/cpa/plugins/linux/amd64/
```

解压得到的文件名就是插件 ID（`codearts.so` → `codearts`）。想保留版本号就重命名为 `codearts-v0.1.0.so`，ID 不变。装完在 `plugins.configs` 里启用并重启 CPA。

本仓库也能在本地产出同样的 zip：

```bash
VERSION=0.1.0 scripts/release.sh          # 构建 + 打包当前平台
scripts/release.sh --skip-build           # 只打包已有 dist/
```

产物落在 `release/<goos>/<goarch>/`，含每个插件的 `<id>_<version>_<goos>_<goarch>.zip`，以及覆盖全部 zip 的 `checksums.txt`（`sha256sum` 格式）：

```bash
cd release/linux/amd64 && sha256sum -c checksums.txt
```

多平台产物由 `.github/workflows/release.yml` 在推送 `v*` tag 时构建。

### 更新与卸载

- **更新**：用新版本动态库替换旧文件后重启 CPA。文件名必须以插件 ID 开头（`<id>.so` 或 `<id>-v<版本>.so`），其他写法会让宿主推导出错误的 ID。
- **卸载**：删除动态库并重启 CPA，再从 `plugins.configs` 去掉该条目。

## CPAMP 管理界面

**结论：可以，且不需要写前端。** 插件能在 CPA-Manager-Plus 里得到完整的登录／状态／签到界面，与 Jet-Hub 在 DSH 前端的效果对应。

CPA 会把 `management.register` 里**带 `Menu` 的路由**变成侧边栏菜单项；CPAMP 的 `collectPluginResourceEntries` 过滤出已启用插件，把**每个菜单渲染成一个可路由页面**，页面内容是一个指向该菜单路径的 iframe：

```
CPAMP 页面  /plugins/<id>/<menuIndex>
   └── iframe src = <apiBase>/v0/resource/plugins/<id>/<path>
```

两个关键点决定了插件侧要怎么做：

1. **路径经 CPAMP 自己的同源反向代理**（manager-server 的 allowlist 放行 `/v0/resource/plugins/...`），所以 iframe 与 CPAMP 同源，宿主才能对 iframe 注入样式。
2. **主题通过 CSS 自定义属性下发**：CPAMP 在 iframe 的 `<head>` 注入一份样式表，定义 `--bg-primary`／`--bg-secondary`／`--text-primary`／`--text-secondary`／`--border-color`／`--primary-color`／`--app-surface`／`--app-input-bg`／`--app-radius-*`／`--success-color`／`--warning-color`／`--danger-color` 等变量，并把 `body` 的底色、文字色与字体设为主题值。所以**插件只要用这些变量写普通 HTML，就能自动跟随 CPAMP 的浅色／深色主题**。

### 挂载规则（实测确认，写插件前必须知道）

宿主对两类路由的处理完全不同（`internal/pluginhost/management.go` 的 `routeDeclaresLegacyMenuResource`）：

| 声明方式 | 实际挂载点 | 侧边栏菜单 | 命名空间 |
| --- | --- | --- | --- |
| GET **且**带 `Menu` | `/v0/resource/plugins/<插件ID>/<路径>` | **每个菜单一项** | 含插件 ID，安全 |
| GET **且** `Menu` 为空 | `/v0/resource/plugins/<插件ID>/<路径>` | 无 | 含插件 ID，安全 |
| 其他任意方法 | `/v0/management/<路径>` | 无 | **全局共享**，与所有插件及宿主内置端点同处一个命名空间 |

三个后果：

1. **全仓库只有一个带 `Menu` 的路由，它属于 `hub`（"Jet Hub"）。** CPAMP 侧边栏是**扁平**的（`collectPluginResourceEntries` 把每个菜单直接 map 成一项，不分组），插件有几个菜单就占几行，还会与内置的「凭证管理」「OAuth 登录」并列；历史上 5 个插件各声明 2～3 个菜单，侧边栏出现 13 行重复条目。因此现在：**`hub` 用唯一的带 `Menu` 路由承载渠道总览与一键签到；每个 provider 插件的状态页、登录页、签到页全部改成 `Menu` 为空字符串的 `Resources` 条目**——`registeredPluginMenus()` 会跳过空 `Menu` 的记录，路径照旧可访问（`/v0/resource/plugins/<id>/status`），侧边栏只剩一行，总览页的每一行直接链接到对应 provider 的状态页。每个插件包的 `zz_menucheck_test.go` 都守着这条计数规则：provider 为 0，`hub` 为 1。
2. **不带 `Menu` 的路由路径必须自带插件前缀**，例如 `/codearts/checkin` 而不是 `/checkin`。冲突时宿主只打一条 `management route ... was skipped` 警告就丢弃该路由——静默失效，很难排查。
3. **带 `Menu` 的 GET 路由不会挂到管理 API 下。** 想同时提供网页和脚本接口时，网页用带 `Menu` 的 GET 路由，脚本接口用**不带 `Menu`** (`Resources`) 或非 GET 方法的路由。

另外，resource 路径**只以 GET 派发**（`management.go` 里 `Method: http.MethodGet` 是写死的）。所以管理页面里的操作不能是表单 POST，必须是携带查询参数的 GET 链接；本仓库的 `plugui.Action` 就按此设计（渲染成 `<a href="?action=...">`），并有单测断言页面里不出现 `<form>`。

因此插件侧只需：`management.register` 声明菜单（`Menu` 字段是分组名，`Description` 是副标题），`management.handle` 在这些路径上返回 `Content-Type: text/html`。本仓库把这件事做成了共享工具包 [`internal/jethub/plugui`](internal/jethub/plugui/plugui.go)——`Document`／`HTML`／`Card`／`Fields`／`Notice`／`Badge`／`Action`，渲染出的页面只消费上述宿主变量，自带浅色／深色适配，且对运行期取到的值（账号名、上游报错文本）做 HTML 转义。

设计约束（有意为之）：

- **两段式登录**：`auth.login.start` 立即返回授权 URL，前端把 URL 显示出来，用户在浏览器完成授权后由轮询收尾。绝不阻塞到用户操作完成——否则浏览器手势过期、弹窗被拦截，界面会退化。
- **幂等判据看响应体而不是 HTTP 状态码**：重复签到同样返回 200，页面必须读响应体字段（如 Qoder 的 `replayed:true`）来呈现真实结果。
- **不支持签到的 provider 如实显示"不支持"**，不臆造状态。
- 状态页在普通页面请求时返回 HTML，带 `?format=json` 时返回 JSON，便于脚本与排障复用同一路由。

## 容器部署：浏览器登录回调必须能被宿主机访问

**这是容器部署最容易踩的坑，且报错信息具有误导性。** 若不做配置，登录授权完成后浏览器会停在
`127.0.0.1:<随机端口>/...` 并显示 `ERR_CONNECTION_REFUSED`，页面提示"登录后自动跳转回调失败"。

原因：`codearts`／`trae`／`lobsterai` 三个平台的授权流程要求浏览器回跳到一个 **loopback 地址**
（`http://127.0.0.1:<port>/...`）。Jet-Hub 跑在 DSH 进程里、与浏览器同机，回环天然可达；而 CPA 插件跑在
**容器内部**——容器里的 `127.0.0.1` 不是宿主机的 `127.0.0.1`，且随机临时端口无法预先发布。CPA 官方 compose
里那行被注释掉的 `# - "1455:1455"` 正是同一问题（Codex 回调端口）。

**解法**：把回调端口固定下来，并让容器监听 `0.0.0.0`、在 compose 里发布同名端口——与 CPA 内置回调转发器
（`auth_files_oauth_callback.go` 绑定 `0.0.0.0:<port>`）的做法一致。

`config.yaml`（端口可自选，只要与 compose 一致）：

```yaml
plugins:
  enabled: true
  configs:
    codearts:
      enabled: true
      callback_port: 18091        # 固定端口；不设置则用 ≥10000 的随机端口
      callback_bind_host: "0.0.0.0"
    trae:
      enabled: true
      callback_port: 18092
      callback_bind_host: "0.0.0.0"
    lobsterai:
      enabled: true
      callback_port: 18093
      callback_bind_host: "0.0.0.0"
```

`docker-compose.yml`：

```yaml
services:
  cli-proxy-api:
    ports:
      - "8317:8317"
      - "18091:18091"   # codearts   登录回调
      - "18092:18092"   # trae       登录回调
      - "18093:18093"   # lobsterai  登录回调
```

三个注意点：

1. **`callback_bind_host` 必须是 `0.0.0.0`。** 只绑 `127.0.0.1` 时，即使发布端口也进不来——Docker 的
   `-p` 是把流量转发到容器的非回环地址。
2. **`callback_port` 要避开宿主机上其它服务占用的端口。** 被别的进程占用时登录直接失败并报
   `callback_listen`，不会静默改用其它端口——静默换端口会让浏览器拿到一个无法路由的地址。
3. **`codearts` 的端口必须 ≥10000。** 这是华为 portal 自身的约束（`login.ts:338-341`），低于此值的配置会被拒绝。

### 回调监听器是常驻的

每个插件**只绑定一次**回调监听器（首次登录时），此后一直保持监听，直到插件随 CPA 进程退出。这一点很关键：

用户在浏览器里完成授权、平台把浏览器跳回来的时刻，**和发起登录的时刻可能相隔很久**——会话可能已超时、
可能被更新的登录请求取代、CPA 也可能重启过。如果监听器跟着会话一起关闭，那一刻端口上就没人监听，
浏览器只会看到 `ERR_CONNECTION_REFUSED`，而授权码其实完全有效。所以监听器的生命周期**不属于任何一次登录**：
会话有自己的超时（用于面板轮询），但不会关掉端口。

由此带来两点行为：

- **「重试」可以反复点**，不会出现 `address already in use`：端口由常驻监听器持有，而不是每次登录重新绑。
- **同一插件同时只有一个"当前"登录会话**：新请求会取代上一个未完成的会话（后者被标记为「已被新的登录请求取代」，
  面板不再空转），但端口本身不受影响。

不改配置也能用：`qoder` 与 `codebuddy` 走**设备码流程**，不需要浏览器回调，在任何部署形态下都可用。
若浏览器不在宿主机上（远程访问），回环回调本身不可达，可参考 CPA 的
[`docs/management-devin-oauth.md`](https://github.com/router-for-me/CLIProxyAPI) 手动投递回调 URL 的做法。

`callback_public_host` / `callback_public_port` 用于反向代理等场景：前者是**浏览器实际访问的地址**，默认
`127.0.0.1` 与绑定端口一致，所以发布端口与容器内端口相同时无需设置。

### WSL2 + Podman：端口映射必须显式写 `0.0.0.0`

在 WSL2 里用 rootless Podman（netavark）跑 CPA、而浏览器在 Windows 上时，有一个会直接表现成
「回调失败」的陷阱：

```
ports:
  - "18091:18091"          # ✗ WSL2 里绑成 *:18091（IPv6）
  - "0.0.0.0:18091:18091"  # ✓ 绑成 0.0.0.0:18091（IPv4）
```

原因：省略主机地址时 Podman 把端口绑在 IPv6 通配地址上，WSL2 的 localhost 转发在 Windows 侧也只镜像出
IPv6 监听。于是 Windows 浏览器访问 `http://127.0.0.1:<port>`（IPv4）会得到 `ERR_CONNECTION_REFUSED`，
而 `http://localhost:<port>` 却能通——这非常有迷惑性，因为日常打开管理面板用的正是 `localhost`。

偏偏厂商 portal 的回调地址是**硬编码 IPv4 字面量**（CodeArts 只接受 `port` 参数，自己拼出
`http://127.0.0.1:<port>/oauth/callback`），改写不了。所以容器部署在 WSL2 下必须显式写 `0.0.0.0:`，
管理面板端口也一样（否则同样只有 `localhost` 能打开）。

排查方法：从 Windows 侧分别访问 `http://127.0.0.1:<port>/` 与 `http://localhost:<port>/`，若前者拒绝、
后者正常，就是这个原因。也可以用 `ss -ltn` 在 WSL2 里看端口绑在 `0.0.0.0` 还是 `*`。

### 登录后这些凭据怎么用

登录成功后凭据写入 `~/.cli-proxy-api/<provider>-*.json`（容器内为 `/root/.cli-proxy-api`，即 compose 里挂进去的 `auths` 目录），模型随之下线到 `/v1/models`。调用与内置 provider 完全一致，**不需要进「AI 提供商」页面**（那里只管 API-key 类条目）：

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"<provider>/<model>","messages":[{"role":"user","content":"hi"}]}'
```

其中 `$CPA_API_KEY` 是 `config.yaml` 里的 `api-keys` 之一。模型别名与禁用走 `oauth-model-alias` / `oauth-excluded-models`，键是 provider 名（`codearts`、`trae` 等），详见下节。

> **注意：不要在凭据的 `attributes` 里写 `api_key`。** 宿主把 `api_key` 属性视为「这是 API-key 型凭据」的依据，会让 `Auth.AuthKind()` 返回 `apikey`，进而让 `oauth-model-alias` 与 `oauth-excluded-models` **静默失效**（`OAuthModelAliasChannel` 对 `apikey` 返回空串）。本仓库曾因此在 `codearts`／`lobsterai` 上踩过这个坑，现分别改用 `access_key_id`／`uid`。

## 依赖的宿主能力

插件通过 `host.call` 使用宿主能力。HTTP 请求一律由宿主执行，代理、TLS 与请求日志仍归宿主控制；因此插件本身不建立网络连接。

## 验证状态

在真实 CPA 宿主中验证过的部分（用 `docker.io/eceasy/cli-proxy-api` 起临时容器装入本仓库产物）：

- **5 个插件可同时加载并注册**：宿主日志逐条输出 `pluginhost: plugin registered plugin_id=<id>`，管理 API 返回 `registered=true`、`effective_enabled=true`、`supports_oauth=true`、`supports_quota=true`；
- **侧边栏每个插件只占一行**（共 5 行）：管理 API 返回的每个插件恰有 1 条带 `Menu` 的路由，登录页/签到页以空 `Menu` 的 resource 路由提供，浏览器可直接访问但不进侧边栏；
- **容器内浏览器登录回调已端到端验证**：固定端口 `18091`、绑定 `0.0.0.0`、compose 发布后，从宿主机执行
  `GET http://127.0.0.1:18091/oauth/callback?code=...` 得到 **307**（插件已捕获并跳转），随后轮询返回上游
  STS 对假 code 的拒绝（`STS5.1805 invalid authorization code`）——证明 code 确实穿过容器边界抵达插件。
  未发布该端口时同一请求连接失败（`curl` exit 000），与用户报告的 `ERR_CONNECTION_REFUSED` 一致，构成负向对照；
- **`codearts` 走通凭据解析到模型目录的全链路**：注入测试凭据后宿主日志出现 `processing auth file` 与 `Registered new model ... from provider codearts`，`/v1/models` 返回 16 个模型；
- **方法面**逐个经 C `dlopen` 探针调用：`plugin.register`、`auth.identifier`、`model.register`、`model.static`、`quota.identifier`、`quota.describe`、`request.translate`、`response.translate`、`management.register` 在 5 个插件上全部返回成功 envelope；
- **`registry.json` 清单**用 CPA 真实解析器（`ParseRegistry` + `ValidateRegistry`）校验通过；发布产物按规范打包并校验 sha256。

**尚未验证的部分**：五个平台的上游协议都**没有对真实服务端跑通过**——这里没有它们的账号。签名算法、载荷转换、SSE 解析、登录状态机、错误分类由单元测试覆盖（`go test ./...`），但**真实登录授权、模型调用与每日签到需要你用真实账号各试一次**。已知的取舍与未移植项记录在 [docs/PORTING.md](docs/PORTING.md)。

## 移植说明

各插件与 Jet-Hub TypeScript 源文件的逐文件对应关系、已核实的端点、加密与登录流程、已知阻塞项，见 [docs/PORTING.md](docs/PORTING.md)。
