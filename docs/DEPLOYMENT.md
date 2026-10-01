# 部署指南：给 CPA 安装 Jet-Hub 插件

本指南覆盖从零开始的完整流程：下载插件 → 安装到 CPA → 配置模型/别名/优先级 → 验证。需要让多个下游 Key 各看一批渠道时，接第 3.6 节。

---

## 1. 下载插件

从 [GitHub Release](https://github.com/collegeming/cpa-jethub-plugins/releases) 取**最新发布版**（Releases 页面置顶的那个）。命令里的 `<版本>` 换成实际 tag（去掉前导 `v`，如 tag 为 `v0.3.1` 则 `VERSION=0.3.1`）：

```bash
# 下载所有插件（以 linux/amd64 为例）
VERSION=<版本>          # 例如 0.3.1；以 Releases 页面为准
BASE="https://github.com/collegeming/cpa-jethub-plugins/releases/download/v${VERSION}"
for p in cline codearts codebuddy codebuddy-intl hub lobsterai loomy qoder raccoon trae workbuddy workbuddy-cn; do
  curl -LO "$BASE/${p}_${VERSION}_linux_amd64.zip"
done
# 校验
curl -LO "$BASE/checksums.txt"
sha256sum -c checksums.txt 2>/dev/null | grep -v FAILED
```

每个 zip 包含一个 `.so` 文件（解压后在 zip 根目录，文件名即插件 ID）。

**需要安装的插件**（按需选择）：

| 插件 | 渠道 | 说明 |
|---|---|---|
| `cline` | Cline | WorkOS 设备码登录 |
| `codearts` | 华为云 CodeArts | 额度包计费 |
| `codebuddy` | 腾讯 CodeBuddy | 签到 + 积分 |
| `codebuddy-intl` | WorkBuddy（国际） | 无签到，按需 |
| `hub` | 渠道总览 | 管理面板入口，**建议安装** |
| `lobsterai` | LobsterAI（有道） | 签到 + 积分 |
| `loomy` | Loomy | 仅探测，无续期 |
| `qoder` | Qoder | 含内嵌 WASM 签名，免费模型可用 |
| `raccoon` | Raccoon（SenseNova） | 免费模型多 |
| `trae` | TRAE（字节） | 需配置登录回调端口 |
| `workbuddy` / `workbuddy-cn` | WorkBuddy 变体 | 按需 |

---

## 2. 安装到 CPA

### 2.1 找到插件目录

CPA 配置文件 `config.yaml` 中 `plugins.dir` 指定的路径。常见位置：

| 部署方式 | 插件目录 |
|---|---|
| 容器（Podman/Docker） | 宿主机上挂载到容器 `/CLIProxyAPI/plugins` 的目录 |
| 直接运行 | `config.yaml` 中 `plugins.dir` 的值 |

### 2.2 解压插件

```bash
# 容器部署示例（插件目录挂载在宿主机 /data/CLIProxyAPI/plugins）
PLUGIN_DIR=/data/CLIProxyAPI/plugins

for p in cline codearts codebuddy hub lobsterai loomy qoder raccoon trae; do
  unzip -o ${p}_${VERSION}_linux_amd64.zip -d ${PLUGIN_DIR}/linux/amd64/
done
```

目录结构应为：

```
<插件目录>/
└── linux/
    └── amd64/
        ├── cline.so
        ├── codearts.so
        ├── codebuddy.so
        ├── hub.so
        ├── lobsterai.so
        ├── loomy.so
        ├── qoder.so
        ├── raccoon.so
        └── trae.so
```

### 2.3 重启 CPA

```bash
podman restart cli-proxy-api    # 容器部署
# 或
systemctl restart cli-proxy-api # systemd 部署
```

---

## 3. 配置 CPA（config.yaml）

### 3.1 启用插件

在 `config.yaml` 的 `plugins.configs` 下启用需要的渠道：

```yaml
plugins:
  enabled: true
  dir: "/CLIProxyAPI/plugins"
  configs:
    cline:
      enabled: true
      model_prefix: false        # 关闭账号 ID 前缀
    codearts:
      enabled: true
      model_prefix: false
    codebuddy:
      enabled: true
      model_prefix: false
    codebuddy-intl:
      enabled: false
      model_prefix: false
    lobsterai:
      enabled: true
      model_prefix: false
    loomy:
      enabled: true
      model_prefix: false
    qoder:
      enabled: true
      model_prefix: false
      region: qoder-cn              # `qoder`=国际版；`qoder-cn`=国内版
      machine_token_path: /CLIProxyAPI/auths/machine_token.json  # 官方客户端设备身份（可选）
    raccoon:
      enabled: true
      model_prefix: false
    zcode:
      enabled: true
      model_prefix: false
      app_version: "3.14.4"       # 官方客户端版本；推理无需 captcha
    minimax:
      enabled: true
      model_prefix: false
      region: cn                  # 当前只接入中国版
    atomcode:
      enabled: true
      model_prefix: false
      # gateway_base: https://api-ai.gitcode.com/v1   # 默认值。不要改成官方默认的
      # llm-api.atomgit.com/v1：那个网关要求闭源请求签名，无签名一律 403 ATOMCODE_SIG_MISSING
    trae:
      enabled: true
      model_prefix: false
      callback_port: 18092       # TRAE 登录回调端口
      callback_bind_host: "0.0.0.0"
```

**`model_prefix: false`** 是关键：关闭后模型列表只显示模型名，不会出现 `<账号ID>/<模型名>` 的冗余条目。

### 3.2 登录各渠道账号

每个渠道需要通过各自的登录流程获取凭据：

| 渠道 | 登录方式 | 管理页路径 |
|---|---|---|
| cline | WorkOS 设备码 | `/v0/resource/plugins/cline/login` |
| codearts | 华为云 OAuth | `/v0/resource/plugins/codearts/login` |
| codebuddy | 浏览器登录 | `/v0/resource/plugins/codebuddy/login` |
| lobsterai | 浏览器两步式 | `/v0/resource/plugins/lobsterai/login` |
| loomy | 浏览器登录 | `/v0/resource/plugins/loomy/login` |
| qoder | 设备码（PKCE） | `/v0/resource/plugins/qoder/login` |
| raccoon | 浏览器登录 | `/v0/resource/plugins/raccoon/login` |
| zcode | CLI 授权 URL + 轮询（可导入官方客户端登录态） | `/v0/resource/plugins/zcode/login` |
| minimax | OAuth 设备码 + PKCE（登录与推理已在真实服务端验证；**续期尚未观察到**，见下） | `/v0/resource/plugins/minimax/login` |
| trae | 回调端口登录 | `/v0/resource/plugins/trae/login` |
| atomcode | 浏览器 OAuth（AtomGit 账号，两步式；已走通登录→推理全链路） | `/v0/resource/plugins/atomcode/login` |

在浏览器中打开管理面板（CPAMP），进入「插件管理」→ 对应渠道 → 「登录」，按页面提示完成授权。

每个渠道的状态页都带三个入口，缺一个都会让用户走进死路：

| 入口 | 作用 | 位置 |
|---|---|---|
| 「新建账号」 | 添加**第二个**账号（链接到登录页并带 `add=1`） | 状态页「全部账号」卡片 |
| 「重新登录」 | **替换**已有账号的凭据（带 `auth_index`，覆盖同一条） | 状态页账号卡片 |
| 「去登录」 | 一个账号都没有时的首次登录 | 状态页空账号卡片 |

#### ZCode 的签到为什么必然失败

`claim` 是官方客户端**唯一**还挂阿里云验证码的端点，而服务端是**强制**的：不带验证码令牌一律返回

```
400 {"code":3007,"msg":"captcha verify failed"}
```

响应里没有可解的挑战，令牌只能由阿里云 JS 产出——也就是官方客户端那套浏览器机制。本插件不产出验证码，所以**ZCode 的一键签到无法完成，重试也不会有不同结果**。推理通道不受影响（实测无需验证码）。

做法：ZCode 的额度请在**官方客户端或网页**里领取。HUB 会把这一条如实报成「失败」并写清原因，而不是伪装成一次成功。

#### Raccoon 的一键签到为什么只领一次性奖励

Raccoon **没有每日签到**：每日 300 积分由服务端自动发放，没有可调用的端点。唯一可领的是**一次性的桌面端登录奖励**（3000 积分，每号一次，服务端幂等）。

一键动作会领它，但有**两道闸**，所以放在每天都会按的按钮后面是安全的：

1. 先读奖励状态（由账单历史推导），已领过就直接报「已领取」，**一次写请求都不发**；
2. 服务端的 `granted` 标志兜底：重复调用返回 `granted:false`，映射为「已领取」而不是第二次「领取成功」。

实测（2026-10-01）：领取后余额 276 → 3276（reward 桶 0 → 3000）；再次运行报「已领取」，余额不变。

#### AtomCode 的网关与签名

AtomCode 官方客户端会给聊天请求加一套闭源签名（`atomcode-codingplan-crypto`，开源源码树里只有 `unreachable!()` 占位），官方默认网关 `llm-api.atomgit.com` 因此**不能**在 CPA 里使用。

实测（同一 token、同一请求体，只改 `User-Agent`）：

| `User-Agent` | 结果 |
|---|---|
| `atomcode/5.2.0`（官方客户端） | `403 {"detail":{"code":"ATOMCODE_SIG_MISSING"}}` |
| 任意其它值，含本插件的 `cpa-jethub-atomcode/0.1.0` | `200`，正常返回推理结果 |

所以插件有三条硬性约定，改动前请先看 `plugins/atomcode/config.go` 的注释：

- 请求标识固定为 `cpa-jethub-atomcode/<版本>`，**不得**改成 `atomcode/<版本>`；
- 网关固定为 `https://api-ai.gitcode.com/v1`（同一服务、同一令牌、同一模型目录，但不校验签名）；
- `models-v2` 里每个模型自带的 `base_url` 指向需要签名的官方网关，插件**只展示不采用**，路由一律走 `gateway_base`。

模型目录是服务端驱动的：`GET /coding-plan/models-v2?plan_type=<档位>`，只有 `plan_available=true` 的条目会被注册；档位由 `status-v2` 的套餐名推断（`auto`，当前账号为 `Lite`）。检索不到时回退内置快照（`qwen3.8-27b` / `glm5.3-flash` / `deepseek-flash`）。平台没有把这些模型写进 `/v1/models`，两套名单并不一致。

#### 凭据到期时间的两种写法（所有渠道都要注意）

实测确认：**插件写入 `expires_at` 后，CPA 会把它规范化成 RFC3339 再存回**。以 MiniMax 为例，插件写的是毫秒数字串，落盘后变成 `"2026-09-30T17:48:18Z"`。

这条规范化曾经造成一次静默故障：MiniMax 的到期解析只接受纯数字，读不懂 RFC3339 就按「没有到期时间」处理，而该渠道把「没有到期时间」定义为「未过期」——于是凭据看起来永远健康，续期一次也没跑，直到上游返回 401。

现在 `minimax`、`loomy`、`raccoon` 三个插件都同时接受**数字（毫秒/秒）与 RFC3339** 两种写法，并各有测试守着（含反向验证）。`codearts` / `trae` / `codebuddy` / `lobsterai` 原本就接受多种写法。

给新渠道作者的一条规则：**写入 `expires_at` 的格式不代表读回来时还是那个格式**，解析必须同时容忍数字与 RFC3339。

> **MiniMax 的一处固有限制**：服务端不下发账号标识（访问令牌不是 JWT，也没有 `nickname`），所以**同一账号重复点「新建账号」会得到多条记录**（凭据文件名取自令牌前缀，每次登录都会变）。给已有账号换凭据请用「重新登录」。这是上游参考实现同样存在的缺口——它把凭据存在单个固定 ref 下，因此不暴露这个问题。

#### MiniMax 的验证状态

状态页的 JSON（`?format=json`）会如实公布三条断言，代码里是单一常量来源（`plugins/minimax/config.go`）：

| 字段 | 值 | 含义 |
|---|---|---|
| `login_verified` | `true` | 设备码授权已在真实服务端走通，取回的 `mmoat_`/`mmort_` 凭据被推理端点接受 |
| `inference_verified` | `true` | `MiniMax-M2.7` 与 `MiniMax-M3.1-Flash-Preview` 均经 CPA 端到端返回 200（含 reasoning 与 usage） |
| `refresh_verified` | `true` | 续期**已实测**（2026-10-01）：把凭据改到距到期 120 秒（落在 300 秒续期窗口内）后发一次推理，请求返回 200 **且** 落盘的 access_token 变成了新的——说明续期在该请求之前已经跑完；随后在窗口之外再发一次，令牌不再轮换，说明续期是按需触发而不是每请求一次 |

#### Qoder 的两个版本（`region`）

| | 国际版 `qoder` | 国内版 `qoder-cn` |
|---|---|---|
| 认证站点 | qoder.com | qoder.cn |
| clientId | `e883ade2-…` | `732aef47-…`（不同！） |
| 推理 | 加密端点（内嵌 WASM） | 加密端点（**无公开端点**） |
| 模型目录 | 17 条 | 14 条（独有 `q37fmodel`/`gm51model`，`mmodel`=MiniMax-M2.7） |
| 免费模型 | `qfmodel`、`qmodel_38max` | 同 |

#### 设备身份（machine_token，强烈建议配置）

Qoder 服务端要求请求携带**官方客户端的设备身份**（`Cosy-MachineToken` + `Cosy-MachineType` 成对出现），否则：积分页看不到每日领取活动、推理会话更容易被风控作废。

设备身份来自本机 Qoder IDE 的 `machine_token.json`（由官方 `runtime-info` 生成，插件无法自造）：

- Windows：`%APPDATA%\Qoder\SharedClientCache\cache\machine_token.json`
- macOS：`~/Library/Application Support/Qoder/SharedClientCache/cache/machine_token.json`

把它复制到 CPA 的 `auths/` 目录（容器内路径 `/CLIProxyAPI/auths/` 或 `/root/.cli-proxy-api/`，按挂载为准）并配置 `machine_token_path`。该文件与账号无关（设备级），**可跨机器复用**，旧文件也依然有效。

### 3.3 配置模型排除（oauth-excluded-models）

用于隐藏不需要的模型（如付费模型、重复模型）。支持 `*` 通配符：

```yaml
oauth-excluded-models:
  cline:
    - "cline-pass/*"
    # ... 按需排除
  lobsterai:
    - "kimi-k3"           # x20 倍率
  loomy:
    - "qwen-3.8-max"      # x12 倍率
    - "MiniMax-M3"        # x4.0 倍率
  qoder:
    - "kmodel_latest"     # x1.4 倍率
  raccoon:
    - "sn-kimi-k3"
```

### 3.4 配置模型别名（oauth-model-alias）

把各渠道的上游模型名收敛成一套统一名字，客户端只按这套名字调用。同一模型名由多个渠道提供时，各渠道的别名写成同一个名字，CPA 合并为一个模型条目，再按优先级档选渠道。

> ⚠️ **CPA 会跳过仅大小写不同的别名**（`applyOAuthModelAliasEntries` 里对 `name`/`alias` 做 `EqualFold` 相等判断后 `continue`）。因此 `glm-5.3 → GLM-5.3` 这类"只改大小写"的映射**写进配置也不生效**，公开列表里会同时出现两种拼写。上游原始 ID 与目标名仅大小写/横杠不同的模型，由插件自己发布规范 ID 并在执行时映射回上游 ID（codebuddy、codearts、lobsterai 已内置），配置里只放**结构性改名**（如 `sn-` 前缀、`cline-free/` 前缀、`qmodel_38max` 这类目录键）。

```yaml
oauth-model-alias:
  cline:
    - name: "cline-free/deepseek-v4.1-flash"
      alias: "DeepSeek-V4.1-Flash"
    - name: "cline-free/gemini-3.8-flash"
      alias: "Gemini-3.8-Flash"
    - name: "cline-free/mimo-v2.6-pro"
      alias: "MiMo-V2.6-Pro"
    - name: "qwen/qwen3.8-27b:free"
      alias: "Qwen3.8-27B"
  codebuddy:
    - name: "kimi-k3-1"
      alias: "Kimi-K3"
    - name: "deepseek-v4.1-flash"
      alias: "DeepSeek-V4.1-Flash"
    - name: "glm-5.3-flash"
      alias: "GLM-5.3-Flash"
  lobsterai:
    - name: "deepseek-flash"
      alias: "DeepSeek-V4.1-Flash"
    - name: "glm-5.3"
      alias: "GLM-5.3"
    - name: "qwen3.8-flash"
      alias: "Qwen3.8-Flash-Next"
  qoder:
    - name: "gmodel"
      alias: "GLM-5.3"
    - name: "gfmodel"
      alias: "GLM-5.3-Flash"
    - name: "mmodel"
      alias: "MiniMax-M3"
    - name: "qmodel_38max"
      alias: "Qwen3.8-Max"
  raccoon:
    - name: "sn-deepseek-v4.1-flash"
      alias: "DeepSeek-V4.1-Flash"
    - name: "sn-glm-5.3"
      alias: "GLM-5.3"
    - name: "sn-sensenova-6-8-flash"
      alias: "SenseNova-6.8-Flash"
  loomy:
    - name: "glm-5.3-flash"
      alias: "GLM-5.3-Flash"
    - name: "qwen3.8-flash"
      alias: "Qwen3.8-Flash-Next"
```

**命名约定**：别名不带渠道后缀。渠道之间的区分交给下游 Key（见 §3.6），模型名只表达模型本身；给别名加 `-Oauth` 之类的后缀，会让同一个 Key 内本可合并的同名模型拆成两条。

> ⚠️ **别名与上游名仅大小写不同时，CPA 视为无操作**（内部按 `strings.EqualFold` 判断），此时该渠道的模型以上游原名的形式进入 `/v1/models`。
> 例：上游 `glm-5.3` 配别名 `GLM-5.3` 不生效，列表里会同时存在 `glm-5.3`（这条渠道的原名）与 `GLM-5.3`（能真正改名的渠道，如 `gmodel` → `GLM-5.3`）。
> 两个名字都能调用；要让名字严格唯一，别名的拼写必须与上游名有大小写之外的差异。

需要同时保留原名与别名时，在该条目上加 `fork: true`。

### 3.5 设置凭据优先级

在每个凭据 JSON 文件（`auths/` 目录下）中添加 `priority` 字段。**优先级语义**：CPA 选择渠道时只取该模型可用的最高档，档内轮询；高档不可用时自动降档。

| 档位 | 渠道 | 说明 |
|---|---|---|
| **6** | cline | 最高档（免费模型多） |
| **5** | codearts, codebuddy, lobsterai | 第二档 |
| **4** | qoder, raccoon, loomy | 第三档 |

```bash
cd <CPA数据目录>/auths
python3 -c "
import json, os, stat
prio = {
    'cline-*.json': 6,
    'codearts-*.json': 5,
    'codebuddy-*.json': 5,
    'lobsterai-*.json': 5,
    'qoder-*.json': 4,
    'raccoon-*.json': 4,
    'loomy-*.json': 4,
}
import glob
for pattern, p in prio.items():
    for f in glob.glob(pattern):
        st = os.stat(f)
        d = json.load(open(f))
        d['priority'] = p
        json.dump(d, open(f, 'w'), ensure_ascii=False, indent=1)
        os.chmod(f, stat.S_IMODE(st.st_mode))
        print(f'{f} -> priority={p}')
"
```

> **注意**：插件自动续期时会重写凭据文件。v0.3.0+ 的插件已修复为保留 `priority` 等宿主托管字段，无需重复设置。

API Key 渠道（`openai-compatibility`）的档位写在该渠道的配置项里，与 OAuth 凭据共用同一套档位语义：

```yaml
openai-compatibility:
  - name: CommandCode
    priority: 4          # 缺省为 5
    base-url: https://api.commandcode.ai/provider/v1
    # ...
```

**同名模型落到哪个渠道**：一次请求先排除当前不可用的凭据，再只保留**最高优先级档**的候选，档内轮询；该档没有可用凭据时才降档。所以一个模型名被多个渠道提供时，实际接单的渠道由各渠道的 `priority` 决定。给同名模型加别名后缀不改变这条规则，改 `priority` 才会。

> ⚠️ 某个渠道整体不可用时（连不上、超时、模型下线），要把它移出同名模型所在的档位，否则请求会一直卡在这个渠道上直到超时。做法是降低该渠道的 `priority`（或直接 `disabled: true`），让它退出最高档。

> ⚠️ 按 §3.6 做 Key 隔离时，这条档位规则默认**先于** Key 策略生效：某个 Key 允许的渠道若在低档、而被它拒绝的渠道占了更高档，该 Key 请求这些同名模型会返回 403 `no allowed upstream profile is available for this API key`。处理方式见 §3.6 步骤 4。

### 3.6 按下游 Key 隔离渠道（可选）

**适用场景**：一个 CPA 实例发给多个下游 Key，每个 Key 只能访问指定渠道（例如 Key 1 只走 OAuth 渠道，Key 2 只走某几个 API Key 渠道）。此时同名模型不需要再用别名后缀区分渠道。

**为什么必须用插件**：CPA 的 `api-keys` 是字符串列表，一个 Key 只有"认证通过/不通过"两种状态，没有权限字段。按 Key 限制渠道要由插件在认证之后过滤上游凭据，本文以 `key-provider-access` 为例。

| 概念 | 含义 |
| --- | --- |
| `caller_scope` | 下游 Key 的派生标识，策略以它为主键，策略文件不落明文 Key |
| profile | 一条上游凭据。OAuth 渠道为 `auths/` 下的文件名；API Key 渠道为 `openai-compatibility:<渠道名>:<hash>` |
| `allow_profiles` | 允许清单，非空即白名单模式 |
| `deny_profiles` | 禁止清单，优先级高于 allow |
| 通配符 | `*` 匹配任意长度字符，`?` 匹配单字符 |

`caller_scope` 不是 Key 的裸 SHA-256，取法（`<下游 Key>` 换成实际值，输出即策略里的 `caller_scope`）：

```bash
python3 -c "import hashlib,sys; print(hashlib.sha256(b'cli-proxy-api:caller-scope:v1\x00'+sys.argv[1].strip().encode()).hexdigest())" '<下游 Key>'
```

**前置条件**

- 已按第 1～3 节装好 OAuth 插件，`auths/` 下已有可用凭据。
- 已列出每个 Key 各自允许的渠道，以及其余全部渠道。

> ⚠️ **风险与限制**
> - `allow` 与 `deny` 必须互补、覆盖全部渠道。插件在 `allow` 与当前候选池无交集时会临时放行"非 deny"的候选，只写 allow 或只写 deny 都可能让某个 Key 访问到不该访问的渠道。
> - 策略文件里是渠道级授权，不含配额、限流、计费。

1. 安装插件，放进 CPA 的插件目录 `plugins/linux/amd64/`。
   - 预期结果：`key-provider-access-<版本>.so` 出现在该目录。
2. 在 `config.yaml` 的 `plugins.configs` 下启用，并指定策略文件路径：

   ```yaml
   plugins:
     configs:
       key-provider-access:
         enabled: true
         priority: 100                              # 调度类插件全局只生效一个，取优先级最高者
         version: 2
         policy_file: /CLIProxyAPI/plugins/key-provider-access/config.toml
         policies: []
   ```

3. 重启 CPA，确认插件已加载且策略条数正确。

   ```bash
   curl -s -H "Authorization: Bearer <管理密钥>" \
     http://localhost:8317/v0/management/plugins/key-provider-access/status
   ```

   - 预期结果：`"policy_count"` 等于策略条数，`"last_error"` 为空；`"version"` 见步骤 4 的说明。
4. 确认插件版本声明了跨优先级档的调度能力（`scheduler_across_priorities`）。
   - 预期结果：`status` 的 `version` 为 `0.0.5-cpamp-v3` 或更高。该标识对应上游 `v0.0.5` 加两处本地补丁：配置页支持 CPAMC 的 `enc::v2::` 会话（否则页面恒提示"未找到可复用的 CPAMC 会话"）、`capabilities.scheduler_across_priorities: true`，并让 `Pick` 在策略过滤后的允许集合内自行按"最高档 + 档内轮询"选择（与 CPA 原生档位语义一致）。补丁与构建步骤见仓库的 `patches/key-provider-access/`。
   - 看不到该版本时先不要继续：CPA 默认只把**最高优先级档**的凭据交给调度插件，策略过滤发生在裁剪之后。Key 允许的渠道处于低档、被拒绝的渠道占更高档时，请求会返回 403 `no allowed upstream profile is available for this API key`——策略本身没错，是候选集在策略生效前就被裁掉了。
5. 写策略文件并使其生效。

   ```toml
   # 两条策略的 allow / deny 互补：每个渠道要么进某个 Key 的 allow，要么进另一个 Key 的 deny
   version = 2

   [[policies]]
     caller_scope = "<Key 1 的 caller_scope>"
     allow_profiles = ["*.json", "openai-compatibility:modelscope:*", "openai-compatibility:nvidia:*"]
     deny_profiles = ["openai-compatibility:commandcode:*", "openai-compatibility:ollama:*", "openai-compatibility:天才程序员:*"]

   [[policies]]
     caller_scope = "<Key 2 的 caller_scope>"
     allow_profiles = ["openai-compatibility:commandcode:*", "openai-compatibility:天才程序员:*"]
     deny_profiles = ["*.json", "openai-compatibility:modelscope:*", "openai-compatibility:nvidia:*", "openai-compatibility:ollama:*"]
   ```

   示例只列了部分渠道。实际配置按互补规则写全：只属于 Key 1 的渠道必须出现在 Key 2 的 `deny_profiles` 里（而不是只出现在 Key 1 的 `allow_profiles` 里）；两个 Key 都不该访问的渠道要同时出现在两份 `deny_profiles` 里。

   ```bash
   # 首次落盘策略文件（把当前内存策略迁移到 policy_file）
   curl -s -X POST -H "Authorization: Bearer <管理密钥>" -H "Content-Type: application/json" \
     -d '{"plugins_dir": "/CLIProxyAPI/plugins"}' \
     http://localhost:8317/v0/management/plugins/key-provider-access/initialize-storage
   # 改完策略文件后使其生效（不重启）
   curl -s -X POST -H "Authorization: Bearer <管理密钥>" \
     http://localhost:8317/v0/management/plugins/key-provider-access/reload
   ```

   - 预期结果：`reload` 返回成功；再次查询 `status`，`"source"` 指向 `policy_file`，`"persistent_updates"` 为 `true`。
6. 用同一个模型名分别以两个 Key 请求一次。
   - 预期结果：允许方返回 200，被拒绝方返回 403，错误体为 `{"error":{"message":"no allowed upstream profile is available for this API key","type":"permission_error","code":"insufficient_quota"}}`。

**失败处理**

- 被拒绝方仍返回 200 → 该 Key 的 `caller_scope` 与策略不匹配（按 `unconfigured_key_action` 全量放行），或 `deny` 未覆盖该渠道。先核对 `caller_scope`，再按互补规则补齐 `deny`。
- 允许方返回 403 → 先看步骤 4 的版本与 `runtime_warning`：出现 `profile_match_failed` 说明该请求被策略拒绝过，按 §5 的排查行处理。

**渠道变更后的动作**

| 变更 | 需要做的事 |
| --- | --- |
| 新增渠道并分配给某个 Key | 加入该 Key 的 `allow`，同时加入另一个 Key 的 `deny` |
| 新增渠道且两个 Key 都不该访问 | 加入两个 Key 的 `deny` |
| 渠道换 API Key | 不改策略：profile ID 里的 hash 由前缀通配 `*` 覆盖 |
| 新增 OAuth 凭据 | 不改策略：`*.json` 已覆盖 |
| 新增下游 Key | 写入 `api-keys`；需要限制则新建对应策略，否则按 `unconfigured_key_action` 全量可用 |

---

## 4. 验证

### 4.1 检查插件加载

```bash
curl -s -H "Authorization: Bearer <管理密钥>" \
  http://localhost:8317/v0/management/plugins | python3 -c "
import sys, json
for p in json.load(sys.stdin)['plugins']:
    if p.get('effective_enabled'):
        print(f\"  ✓ {p['id']}\")
"
```

### 4.2 检查模型列表

```bash
curl -s -H "Authorization: Bearer <API密钥>" \
  http://localhost:8317/v1/models | python3 -c "
import sys, json, collections
d = json.load(sys.stdin)
by_owner = collections.defaultdict(list)
for m in d['data']:
    by_owner[m.get('owned_by')].append(m['id'])
print(f'总模型数: {len(d[\"data\"])}')
for owner in sorted(by_owner): print(f'  {owner:26s} {len(by_owner[owner])}')
"
```

- 预期结果：每个启用的渠道都出现在列表里，条目数与各渠道 `models` + `oauth-model-alias` 的配置相符。
- 同一模型名由多个渠道提供时只登记一条，`owned_by` 显示最后注册的渠道，不代表该模型只能由这个渠道服务。

### 4.3 检查凭据优先级

```bash
curl -s -H "Authorization: Bearer <管理密钥>" \
  http://localhost:8317/v0/management/auth-files | python3 -c "
import sys, json
for a in sorted(json.load(sys.stdin).get('files') or [], key=lambda x:-(x.get('priority') or 0)):
    print(f\"  {a['name']:48s} priority={a.get('priority')}\")
"
```

### 4.4 测试推理

```bash
curl -s -X POST http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <API密钥>" \
  -H "Content-Type: application/json" \
  -d '{"model":"DeepSeek-V4.1-Flash","messages":[{"role":"user","content":"说三个字"}],"max_tokens":16}'
```

### 4.5 测试流式

```bash
curl -s -N -X POST http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <API密钥>" \
  -H "Content-Type: application/json" \
  -d '{"model":"Kimi-K3","messages":[{"role":"user","content":"hi"}],"max_tokens":8,"stream":true}'
```

确认响应是标准 SSE 格式（`data: {...}` 帧 + `data: [DONE]`），无双重 `data: data:` 前缀。

### 4.6 确认思考内容没有被拆成逐字分段

```bash
curl -s -N -X POST http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <API密钥>" \
  -H "Content-Type: application/json" \
  -d '{"model":"DeepSeek-V4.1-Flash","messages":[{"role":"user","content":"用三句话解释哈希表"}],"max_tokens":200,"stream":true,"reasoning_effort":"high"}' \
  | grep -c '"tool_calls":\[\]'
```

- 预期结果：输出 `0`（`grep` 无命中时退出码为 1，属正常）。
- 输出非 `0` 说明插件版本过旧，见第 5 节对应条目。
- 该模型必须由 CodeBuddy 渠道提供才具备判定意义；先按第 4.2 节确认 `owned_by` 为 `codebuddy`。

---

## 5. 常见问题

| 问题 | 原因 | 解决 |
|---|---|---|
| 模型列表出现 `<账号ID>/<模型名>` | `model_prefix` 未设为 `false` | 在 `plugins.configs.<渠道>.model_prefix: false` |
| 凭据优先级续期后丢失 | 插件版本过旧 | 升级到含凭据字段保留修复的版本（v0.3.0 引入） |
| 流式响应 `data: data:` 双重前缀 | 插件版本过旧 | 升级到含流式分片裸 payload 修复的版本（v0.3.0 引入） |
| qoder 推理返回 `Unsupported model` | 走了公开端点 | 升级到包含内嵌 WASM 的版本（自动走加密端点） |
| qoder 推理返回 `quota exceeded` | 账号 0 额度 | 免费模型（qfmodel/qmodel_38max）可用；或充值 |
| cline 续期后 token 过期 | 有效期未随新 token 更新 | 升级到含 JWT exp 修复的版本（v0.3.0 引入） |
| 插件登录回调不通 | 容器端口未映射 | 确保 `callback_port` 映射到宿主机 `0.0.0.0` |
| 别名模型调用返回 `model not found` | 上游模型名不匹配 | 检查 `oauth-model-alias` 中的 `name` 是否与上游一致 |
| qoder 每日领取显示「无可领取活动」 | 未配置设备身份 | 配置 `machine_token_path` 指向官方客户端的 machine_token.json |
| qoder 凭证反复失效（重登录后几小时又 401） | 会话被风控作废（自造设备身份易触发） | 配置 `machine_token_path` 复用官方客户端设备身份；避免与 IDE 频繁交替登录 |
| 公开模型列表同时出现 `glm-5.3` 与 `GLM-5.3` | 仅大小写不同的别名被 CPA 跳过（EqualFold） | 升级到含"插件发布规范 ID"的版本（v0.3.2 引入），或改用结构性别名 |
| codearts 报 `Message role cannot empty`（HTTP 500） | 上游不认 OpenAI 的 `developer` 角色 | 升级到含 developer 角色归一的版本（v0.3.1 引入） |
| lobsterai 报 `角色信息不正确`（HTTP 502） | 同上 | 同上 |
| 客户端（如 DSH）把系统提示词发成 `developer` 角色时的通用说明 | 部分上游只认 system | 全部插件已在请求侧把 developer 归一为 system（v0.3.1 引入） |
| 思考内容在客户端里逐字显示（每行一个词，如 ZCode 出现几十行「思考」） | CodeBuddy 渠道在每个分片里都带 `"tool_calls":[]`，基于分段渲染的客户端把每个分片当成一段独立的思考 | 升级到含空 `tool_calls` 剥离的版本（v0.3.3 引入），重启 CPA；判定方法见第 4.6 节 |
| 别名配了但 `/v1/models` 里仍是上游原名（如 `glm-5.3` 与 `GLM-5.3` 并存） | 别名与上游名仅大小写不同，CPA 视为无操作 | 让别名与上游名有大小写之外的差异，或直接按原名调用——两个名字都能路由 |
| 某个 Key 请求同名模型返回 403 `no allowed upstream profile is available for this API key`，而该渠道确实写在该 Key 的 `allow_profiles` 里 | 候选集在 Key 策略生效前就被裁到最高优先级档（见 §3.6 步骤 4） | 用声明 `scheduler_across_priorities` 的 `key-provider-access`（`0.0.5-cpamp-v3` 起）；或把该渠道的 `priority` 调到与被拒绝渠道同档 |
| 同名模型被路由到非预期的渠道（例如免费 OAuth 模型走了付费 API Key 渠道） | 同名模型按优先级档选渠道，最高档优先，名字后缀不参与选择 | 调 `priority`；需要按 Key 区分渠道时按 §3.6 配置 |
| 某渠道的模型整段超时（请求 20s 以上无响应） | 该渠道上游不可用 | 用 `/v1/models` 确认该模型是否还有其它渠道；必要时降低该渠道 `priority` 让它退出共享模型名所在档，或直接停用该渠道 |

---

## 6. 更新插件

```bash
# 下载新版本 zip，解压覆盖，重启
unzip -o <插件>_<版本>_linux_amd64.zip -d <插件目录>/linux/amd64/
podman restart cli-proxy-api
```

**注意**：替换 `.so` 文件必须重启 CPA；改 `config.yaml` 无需重启（热加载）。

---

## 7. 从源码构建（可选）

仅在需要修改插件代码或目标平台无预编译产物时使用。

**前置条件**：Go ≥ 1.23（需 CGO）、GCC。

```bash
git clone git@github.com:collegeming/cpa-jethub-plugins.git
cd cpa-jethub-plugins

export CGO_ENABLED=1
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=off

# 构建
bash scripts/build.sh        # 产物在 dist/linux/amd64/

# 打包发布
VERSION=<版本> bash scripts/release.sh --skip-build
```

交叉编译：`GOOS=darwin GOARCH=arm64 bash scripts/build.sh`（Apple Silicon）。
