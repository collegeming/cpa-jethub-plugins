# cpa-jethub-plugins

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

### 插件商店

`registry.json` 是 schema v2 清单，`install.type` 为 `direct`，为每个插件声明 linux/amd64、linux/arm64、darwin/arm64 三个产物。清单里的 `sha256` 是占位值（64 个 `0`），发布产物前必须替换为真实摘要。

产物 URL 的文件名与安装后的文件名无关：商店安装时 CPA 自行命名为 `<id>-v<版本><扩展名>`（`internal/pluginstore/install.go` 的 `versionedPluginFileName`），因此不可能因资产命名而注册出错误的插件 ID。清单只需保证每个 `goos`／`goarch` 对应的 URL 指向该平台的产物。

把本仓库的清单作为第三方源加入 CPA：

```yaml
plugins:
  enabled: true
  store-sources:
    - https://raw.githubusercontent.com/cpa-jethub/plugins/main/registry.json
```

### 依赖的宿主能力

插件通过 `host.call` 使用宿主能力。HTTP 请求一律由宿主执行，代理、TLS 与请求日志仍归宿主控制；因此插件本身不建立网络连接。

## 移植说明

各插件与 Jet-Hub TypeScript 源文件的逐文件对应关系、已核实的端点、加密与登录流程、已知阻塞项，见 [docs/PORTING.md](docs/PORTING.md)。
