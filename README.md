# cpa-jethub-plugins-plugins

CLIProxyAPI（CPA）原生 Go 插件集合。每个插件把 Jet-Hub 的一个平台适配器移植为 `-buildmode=c-shared` 动态库，经 CPA 的插件 ABI 以 JSON envelope 提供登录、凭据续期、模型目录、执行器与配额能力。

## 架构

| 层 | 位置 | 职责 |
| --- | --- | --- |
| 共享 ABI 层 | `internal/abiboot` | envelope 编解码、能力注册、方法分发、宿主回调（`host.http.*`、`host.auth.*`、`host.log`） |
| 插件实现 | `plugins/<provider>/` | `main.go` 导出 4 个 C 符号；`plugin.go` 返回 `Registration` 与方法表 |
| 分发清单 | `registry.json` | CPA 插件商店 manifest（schema v2，`direct` 安装） |
| 构建 | `scripts/build.sh` | 逐插件产出 c-shared 动态库 |

每个插件目录是**单一 Go 包 `package main`**：`main.go` 承载 cgo 胶水并导出 `cliproxy_plugin_init`、`cliproxyPluginCall`、`cliproxyPluginFree`、`cliproxyPluginShutdown`，其余 `.go` 文件承载业务逻辑。这样 `go build -buildmode=c-shared ./plugins/<provider>` 可以从该目录直接产出动态库。

依赖约束：只使用标准库与 `github.com/router-for-me/CLIProxyAPI/v7/sdk/{pluginabi,pluginapi}`。

## 插件状态

| 插件 ID | 显示名 | 状态 | 说明 |
| --- | --- | --- | --- |
| `codearts` | CodeArts Agent | 已实现 | 登录、凭据、模型目录、执行器、配额 |
| `codebuddy` | CodeBuddy | 脚手架 | 仅注册与 `auth.identifier`，其余方法返回 `not_implemented` |
| `qoder` | Qoder | 脚手架 | 同上 |
| `trae` | TRAE | 脚手架 | 同上 |
| `lobsterai` | LobsterAI | 脚手架 | 同上 |

四个脚手架的 `Registration` 已按最终能力集声明（模型注册、模型提供、认证提供、执行器、管理 API、配额提供），方法表已注册 `auth.identifier`／`auth.parse`／`auth.login.start`／`auth.login.poll`／`auth.refresh`／`model.register`／`model.for_auth`／`executor.identifier`／`executor.execute`／`executor.execute_stream`／`request.translate`／`response.translate`，上游协议实现尚未迁移。除 `auth.identifier` 外，这些方法当前一律返回 `not_implemented`。

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

### 第三方源安装

`registry.json` 是官方商店格式的 `schema_version: 1` 清单，只有元数据，产物由本仓库的 GitHub Release 解析。把它作为额外源加入 CPA 即可在管理面板的插件商店里看到并一键安装：

```yaml
plugins:
  enabled: true
  store-sources:
    - https://raw.githubusercontent.com/collegeming/cpa-jethub-plugins/main/registry.json
```

### 提交到官方插件商店

官方商店仓库是 [`router-for-me/CLIProxyAPI-Plugins-Store`](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store)，**只维护一个 `registry.json`**，插件二进制留在作者自己的仓库里。完整流程：

**1. 先发布一个符合要求的 Release。** 商店 README 的硬性要求：

| 要求 | 说明 |
| --- | --- |
| Release tag | 必须是 `v<版本>`，版本为点分数字，例如 `v0.1.0` |
| 资产 | 每个支持的平台一个 zip，外加**一个** `checksums.txt` |
| zip 命名 | `<id>_<version>_<goos>_<goarch>.zip`，例如 `codearts_0.1.0_linux_amd64.zip` |
| zip 内容 | 动态库必须放在 **zip 根目录**，且就叫 `<id>.so`／`<id>.dylib`／`<id>.dll`（不带版本后缀、不能嵌套目录） |
| checksums.txt | `sha256sum` 格式：`<sha256>  <文件名>`（两个空格） |

这些约束由商店 README 明确列出；CPA 安装器会拒绝嵌套动态库、绝对路径、zip-slip 路径、文件名不匹配以及一个 zip 里含多个动态库的情况。

本仓库已把这一步自动化：

```bash
# 本地打包（当前平台）
VERSION=0.1.0 scripts/release.sh
# 多平台：推送 tag 后由 .github/workflows/release.yml 构建并创建 Release
git tag v0.1.0 && git push origin v0.1.0
```

工作流在 ubuntu 上构建 linux/amd64、linux/arm64（`gcc-aarch64-linux-gnu`）、windows/amd64（`gcc-mingw-w64-x86-64`），在 macOS runner 上构建 darwin/amd64、darwin/arm64（c-shared 依赖 cgo，无法单机交叉），收集全部 zip 后统一生成覆盖所有资产的 `checksums.txt`，再创建 Release。

**2. 向商店仓库提 PR。** 只改 `registry.json`。PR 里要写明：

- 插件的 GitHub 仓库地址；
- 最新的 release tag，形如 `v0.1.0`；
- 证明所需的 zip 资产与 `checksums.txt` 确实存在于该 release；
- 该插件新增的能力，一句话说明。

条目形状（必填 `id`／`name`／`description`／`author`／`repository`）：

```json
{
  "id": "codearts",
  "name": "CodeArts Agent",
  "description": "…",
  "author": "collegeming",
  "repository": "https://github.com/collegeming/cpa-jethub-plugins",
  "homepage": "https://github.com/collegeming/cpa-jethub-plugins",
  "license": "MIT",
  "tags": ["Provider", "Huawei", "CodeArts"]
}
```

校验规则（来自商店 README，已用 CPA 真实解析器 `ParseRegistry` + `ValidateRegistry` 验证过本仓库的清单）：`schema_version` 必须是 `1`；`id` 首字符为 ASCII 字母或数字，其余仅限字母、数字、`.`、`_`、`-`，总长 ≤128；`id` 唯一；`version` 若存在**不能以 `v` 开头**；`repository` 必须严格等于 `https://github.com/{owner}/{repo}`。

> 本仓库一个 repo 承载多个插件，靠资产名里的 `<id>` 区分，因此 5 个条目共用同一个 `repository`。建议**只把真正实现的插件**提交到官方商店（当前是 `codearts`），脚手架发布的空壳没有价值。发布新版本只需推新 tag，商店清单无需改动。

## CPAMP 管理界面

**结论：可以，且不需要写前端。** 插件能在 CPA-Manager-Plus 里得到完整的登录／状态／签到界面，与 Jet-Hub 在 DSH 前端的效果对应。

CPA 会把 `management.register` 注册的每条路由变成菜单项；CPAMP 的 `collectPluginResourceEntries` 过滤出已启用插件，把**每个菜单渲染成一个可路由页面**，页面内容是一个指向该菜单路径的 iframe：

```
CPAMP 页面  /plugins/<id>/<menuIndex>
   └── iframe src = <apiBase>/v0/resource/plugins/<id>/<path>
```

两个关键点决定了插件侧要怎么做：

1. **路径经 CPAMP 自己的同源反向代理**（manager-server 的 allowlist 放行 `/v0/resource/plugins/...`），所以 iframe 与 CPAMP 同源，宿主才能对 iframe 注入样式。
2. **主题通过 CSS 自定义属性下发**：CPAMP 在 iframe 的 `<head>` 注入一份样式表，定义 `--bg-primary`／`--bg-secondary`／`--text-primary`／`--text-secondary`／`--border-color`／`--primary-color`／`--app-surface`／`--app-input-bg`／`--app-radius-*`／`--success-color`／`--warning-color`／`--danger-color` 等变量，并把 `body` 的底色、文字色与字体设为主题值。所以**插件只要用这些变量写普通 HTML，就能自动跟随 CPAMP 的浅色／深色主题**。

因此插件侧只需：`management.register` 声明菜单（`Menu` 字段是分组名，`Description` 是副标题），`management.handle` 在这些路径上返回 `Content-Type: text/html`。本仓库把这件事做成了共享工具包 [`internal/jethub/plugui`](internal/jethub/plugui/plugui.go)——`Document`／`HTML`／`Card`／`Fields`／`Notice`／`Badge`／`Action`，渲染出的页面只消费上述宿主变量，自带浅色／深色适配，且对运行期取到的值（账号名、上游报错文本）做 HTML 转义。

设计约束（有意为之）：

- **两段式登录**：`auth.login.start` 立即返回授权 URL，前端把 URL 显示出来，用户在浏览器完成授权后由轮询收尾。绝不阻塞到用户操作完成——否则浏览器手势过期、弹窗被拦截，界面会退化。
- **幂等判据看响应体而不是 HTTP 状态码**：重复签到同样返回 200，页面必须读响应体字段（如 Qoder 的 `replayed:true`）来呈现真实结果。
- **不支持签到的 provider 如实显示"不支持"**，不臆造状态。
- 状态页在普通页面请求时返回 HTML，带 `?format=json` 时返回 JSON，便于脚本与排障复用同一路由。

## 依赖的宿主能力

插件通过 `host.call` 使用宿主能力。HTTP 请求一律由宿主执行，代理、TLS 与请求日志仍归宿主控制；因此插件本身不建立网络连接。

## 移植说明

各插件与 Jet-Hub TypeScript 源文件的逐文件对应关系、已核实的端点、加密与登录流程、已知阻塞项，见 [docs/PORTING.md](docs/PORTING.md)。
