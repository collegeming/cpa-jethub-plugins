# Porting map: Jet-Hub TS adapters → CPA native Go plugins

This document maps the Jet-Hub TypeScript provider adapters in `/home/colle/dsh/jethub-src/src/`
to the Go files they must become under `plugins/<provider>/` in this repository.

## How to read this document

| Tag | Meaning |
| --- | --- |
| **[V]** | Verified by reading the TypeScript source. The cited `file:line` was opened and the claim comes from it. |
| **[D]** | Taken from `jethub-src/AGENTS.md` (the project's own documentation). Consistent with the source I read, but not re-verified line by line. |
| **[I]** | Inferred. My reasoning about the Go target, the CPA ABI, or effort. Not a source claim. |

Two rules were applied throughout:

1. **No endpoint is listed that I did not see in the TypeScript.** Every URL or path below carries a `file:line`.
2. **Nothing is claimed about an algorithm, key, or header that I did not read.** Where the source is silent, the row says so.

> **Status update.** The scaffolding phase this document was written for is over: all five
> adapters (`codearts`, `codebuddy`, `qoder`, `trae`, `lobsterai`) are now implemented and
> register the full method surface. The maps below remain the record of *what* was ported and
> *where each fact came from*; where an implementation diverged from the plan, the plugin's own
> code comments say so. See the README's "验证状态" section for what has and has not been
> exercised against a live server.
>
> The four-per-provider file split suggested in §1 was followed, with additions: every plugin
> also carries `management.go`/`pluginui.go` (the HTML management pages) and its own focused
> `*_test.go` files.

## 1. Target file convention

Each plugin directory compiles as a single `package main` (see
`plugins/codebuddy/main.go` for the ABI glue). Suggested file split, used by every
table below:

| Target file | Responsibility |
| --- | --- |
| `plugin.go` | `abiboot.Plugin` registration and the method table (already present) |
| `config.go` | provider key, display name, hosts, path constants, product variants |
| `oauth.go` | `auth.login.start` / `auth.login.poll`: build the login URL, run the poll loop |
| `auth.go` | `auth.parse` / `auth.refresh`: credential JSON shape, expiry, refresh request |
| `models.go` | `model.register` / `model.for_auth`: static and per-auth model catalogs |
| `adapter.go` | `executor.execute` / `executor.execute_stream`: request build, SSE consumption |
| `credits.go` | quota provider: balance and daily check-in |
| `errors.go` | upstream error classification and account-rotation predicates |

Provider-specific files are named in each section.

### 1.1 CPA method ↔ Jet-Hub counterpart

| CPA method | Jet-Hub counterpart | Notes |
| --- | --- | --- |
| `auth.identifier` | `export const PROVIDER` | Already implemented in all four scaffolds. |
| `auth.parse` | `parseCredential()` | Credential is a JSON blob; both sides keep the same field names. |
| `auth.login.start` | `fetchAuthState` / `buildXxxLoginUrl` | Must return the URL without blocking — two-step login. **[D]** |
| `auth.login.poll` | `loopGetToken` + `getAccount` / `pollXxxDeviceToken` / `exchangeXxxCallback` | CPA drives the polling loop; the plugin answers one poll per call. |
| `auth.refresh` | `refreshToken` / `applyXxxRefresh` | Must stay callable for a credential whose account is disabled. **[D]** |
| `model.register` | `listAllModels()` | Static/fallback catalog; includes models the user has hidden. |
| `model.for_auth` | `listModels()` | Per-credential catalog; returns `[]` (never an error) when no account is logged in. **[D]** |
| `executor.identifier` | `PROVIDER` | Currently `not_implemented` in the scaffolds. |
| `executor.execute` | non-streaming chat | |
| `executor.execute_stream` | `consumeSse` / `consumeOpenAiSse` | |
| `request.translate` | `serializeMessages` / `transformToSOLOBody` | TRAE requires a real body transform; the others build OpenAI bodies. |
| `response.translate` | chunk construction | TRAE and Qoder require it; CodeBuddy and LobsterAI relay OpenAI SSE. |

## 2. Shared Jet-Hub modules

These files are not owned by one provider. The "disposition" column says whether the Go side
needs them.

| TS file | Role | Disposition for the Go port |
| --- | --- | --- |
| `sse.ts` | SSE idle-timeout reader and tool-call pairing helpers: `readWithIdleTimeout`, `resolveToolPairing`, `normalizeToolArguments`, `isTruncatedArguments` **[V]** | Shared concern. `internal/jethub/sse/sse.go` already provides a `Scanner` and `Encode`; the idle-timeout and tool-pairing helpers still need a Go equivalent. |
| `openai-compat.ts` | Message serialization, `consumeOpenAiSse`, error classification **[V]** | Consumed **only** by `qoder-adapter.ts` **[V]**. Port for Qoder only. |
| `product.ts` | `BuddyProduct` config for the CodeBuddy/WorkBuddy family **[V]** | Port for CodeBuddy (`config.go`). |
| `credits.ts` | CodeBuddy/WorkBuddy daily check-in client **[V]** | Port for CodeBuddy (`credits.go`). |
| `llm-adapter.ts` | DSH `LlmAdapter` base class | Not ported: the CPA `executor.*` methods replace it. **[I]** |
| `account-pool.ts` | Multi-account pool persisted in the `jet-hub` settings namespace | Already ported for the Go side as `internal/jethub/accountpool` (selection and rate-limit bookkeeping; persistence is left to the caller) **[V]** |
| `refresh.ts` | Refresh lead/retry constants and expiry classification | Already ported for the Go side as `internal/jethub/credits` (`RefreshLeadMS`, `RetryDelay`, `FirstRefreshDelay`, `IsRefreshTokenExpired`) **[V]** |
| `service.ts`, `login.ts`, `oauth.ts`, `models.ts`, `sign.ts`, `types.ts` | CodeArts-specific (Huawei OAuth, DPoP, `SDK-HMAC-SHA256`, STS) **[V]** | Owned by `plugins/codearts/` (a different agent). |
| `index.ts`, `jet-hub-rpc.ts` | DSH plugin wiring and Jet Hub UI RPC | Not ported: replaced by CPA plugin registration and the management API. **[I]** |

### 2.1 Reusable Go helpers

`internal/` already contains Go ports of the shared Jet-Hub machinery. Implementers should use
these rather than re-deriving them per provider:

| Go package | Ports | Relevant to |
| --- | --- | --- |
| `internal/jethub/sse` | SSE frame scanning and `[DONE]` encoding | all four providers |
| `internal/jethub/openai` | OpenAI wire types (request, chunk, completion, usage) | all four providers |
| `internal/jethub/oauthcb` | Loopback callback listener: bind `127.0.0.1`, serve one callback path, hand the query to a blocking waiter, expose `Port()`/`RedirectURI()` | TRAE (port 18080), LobsterAI (random port) |
| `internal/jethub/accountpool` | Account selection and rate-limit bookkeeping | all four providers |
| `internal/jethub/credits` | Refresh scheduling policy and expiry classification | all four providers |

Still missing Go equivalents: the SSE idle-timeout reader and tool-call pairing helpers from
`sse.ts` (`readWithIdleTimeout`, `resolveToolPairing`, `normalizeToolArguments`,
`isTruncatedArguments`), and the Qoder-only `openai-compat.ts` message serializer.

## 3. CodeBuddy (`codebuddy`)

Jet-Hub provider ids `buddy`, `buddy-intl`, `workbuddy`, `workbuddy-cn` share one adapter and
differ only by product configuration **[V]** (`product.ts:74`). The CPA plugin uses the single
key `codebuddy`.

### 3.1 File inventory

| TS file | Lines | Purpose **[V]** |
| --- | --- | --- |
| `buddy.ts` | 910 | Endpoint/path/header constants, credential shape, JWT expiry and nickname helpers |
| `buddy-oauth.ts` | 589 | Login flow: auth state, token poll, account poll, refresh, model fetch, `/v3/config` |
| `buddy-auth.ts` | 407 | DSH service binding the flow to `ctx.credentials` |
| `buddy-adapter.ts` | 1492 | `LlmAdapter`: message serialization, chat request, model catalog, display-name disambiguation |
| `product.ts` | 492 | `BuddyProduct` config and fallback model tables |
| `credits.ts` | 490 | Daily check-in and credit balance |

### 3.2 Porting table

| TS file | Target Go file in `plugins/codebuddy/` | What must be ported | Difficulty | Notes |
| --- | --- | --- | --- | --- |
| `buddy.ts:20-95` | `config.go` | Hosts, `/v2/plugin/*` paths, `/v3/config`, UA and `X-*` header names **[V]** | easy | Pure constants. `API_ENDPOINT` = `https://copilot.tencent.com`, `WEBSITE_HOME` = `https://www.codebuddy.cn`. |
| `buddy.ts:97-236` | `auth.go` | Credential JSON, `expiresAt`/`expiresIn` normalization, JWT `iat`/`exp`/nickname parsing, request/auth header builders **[V]** | medium | `expiresIn` is resolved against the JWT `iat`, not wall-clock (`buddy.ts:263-306`) — an easy thing to get wrong. |
| `buddy-oauth.ts:137-330` | `oauth.go` | `fetchAuthState` → `loopGetToken` → `getAccount`, plus `refreshToken` **[V]** | medium | `POST /v2/plugin/auth/state?platform=ide` returns `{state, authUrl}`; token poll is `GET /v2/plugin/auth/token?state=`; account poll is `GET /v2/plugin/login/account?state=`. |
| `buddy-oauth.ts:348-500` | `models.go` | `fetchModels` through `/v3/config`, promotions merge, enterprise scope **[V]** | medium | `/v3/config` has UA validation: a wrong UA returns HTTP 200 with an error body **[D]**. |
| `buddy-oauth.ts:525-589` | `oauth.go` | `decorateLoginUrl`, `runBuddyLoginFlow` orchestration **[V]** | easy | Only WorkBuddy appends `version`+`loginSessionId`; CodeBuddy must not rebuild the server-issued URL. |
| `buddy-adapter.ts:206-360` | `adapter.go` | Message serialization, image collection, tool-result text **[V]** | medium | ~200 lines of block-to-wire conversion. |
| `buddy-adapter.ts:1100-1150` | `adapter.go` | Chat request: `POST {endpoint}/v2/chat/completions`, `Bearer`, `X-Product` attribution UA **[V]** | easy | Response is standard OpenAI SSE **[V]** (`buddy-adapter.ts:1133`). |
| `buddy-adapter.ts:1390-1479` | `models.go` | Model display names, variant disambiguation, `maxOutputTokens` pass-through **[V]** | medium | Same-name models must be disambiguated; `maxOutputTokens` is authoritative and must reach the request body **[D]**. |
| `product.ts:74-143`, `256-272` | `config.go` | `BuddyProduct` fields and the `CODEBUDDY` product **[V]** | easy | Keep `endpoint` and `apiDomain` separate: `X-Domain` gets the bare host. |
| `credits.ts:33-51` | `credits.go` | Check-in and balance endpoints **[V]** | medium | `POST /v2/billing/meter/checkin-activity-status`, `POST /v2/billing/meter/daily-checkin`, `GET|POST /v2/billing/meter/get-user-resource`. Idempotency is a body field, not an HTTP status **[D]**. |
| `buddy-auth.ts:33-88` | `plugin.go` | Service wiring | n/a | Not ported: `abiboot` dispatch replaces the DSH service. **[I]** |

### 3.3 Verified endpoints and headers

All from `buddy.ts` unless noted:

| Item | Value | Source |
| --- | --- | --- |
| API host | `https://copilot.tencent.com` | `buddy.ts:20` |
| Auth state | `POST /v2/plugin/auth/state?platform=ide` | `buddy.ts:29`, `buddy-oauth.ts:142` |
| Token poll | `GET /v2/plugin/auth/token?state=<state>` | `buddy.ts:31`, `buddy-oauth.ts:191` |
| Account poll | `GET /v2/plugin/login/account?state=<state>` | `buddy.ts:33`, `buddy-oauth.ts:245` |
| Refresh | `POST /v2/plugin/auth/token/refresh` | `buddy.ts:35` |
| Accounts | `GET /v2/plugin/accounts` | `buddy.ts:37` |
| Model config | `GET /v3/config` | `buddy.ts:39` |
| Chat | `POST https://copilot.tencent.com/v2/chat/completions` | `buddy-adapter.ts:42`, `:1133` |
| Check-in paths | `/v2/billing/meter/checkin-activity-status`, `/v2/billing/meter/daily-checkin`, `/v2/billing/meter/get-user-resource` | `credits.ts:33,35,51` |
| Header names | `X-Domain`, `X-Enterprise-Id`, `X-Tenant-Id`, `X-No-Authorization`, `X-No-User-Id`, `X-No-Enterprise-Id`, `X-No-Department-Info`, `X-Refresh-Token`, `X-Auth-Refresh-Source`, `X-Product`, `X-Product-Code` | `buddy.ts:61-71` |
| Default UA | `CodeBuddyIDE/1.106.1` | `buddy.ts:77` |
| Product code | `codebuddy` | `buddy.ts:79` |
| Login website | `https://www.codebuddy.cn` | `buddy.ts:26` |

### 3.4 Crypto and credentials

- **No signing.** Authentication is `Bearer <access_token>` plus the `X-*` identity headers **[V]** (`buddy.ts:209-236`).
- The only crypto calls are `crypto.randomUUID()` for the chat session id and the WorkBuddy
  `loginSessionId` **[V]** (`buddy-adapter.ts:561`, `buddy-oauth.ts:532`). Go's `crypto/rand` covers both.
- Credential expiry is derived from the access-token JWT (`iat`/`exp`) and the relative
  `expiresIn` field **[V]** (`buddy.ts:263-306`) — no secret material is involved.

### 3.5 Login flow **[V]**

1. `POST /v2/plugin/auth/state?platform=ide` with no-authorization headers → `{state, authUrl}`.
2. For WorkBuddy only, append `version` and `loginSessionId` to `authUrl`; other products use it unchanged (`buddy-oauth.ts:525-538`).
3. Open `authUrl` in a browser and poll `GET /v2/plugin/auth/token?state=` every second for up to 5 minutes; upstream code `11217` means "not ready yet" (`buddy.ts:55`, `buddy-oauth.ts:198-220`).
4. Poll `GET /v2/plugin/login/account?state=` with the bearer token; code `12151` means "not ready yet" (`buddy.ts:57`, `buddy-oauth.ts:255-288`).

### 3.6 Blockers

| Blocker | Detail |
| --- | --- |
| Interactive browser login | The user must complete the login page in a browser (`authUrl` is server-issued and redirects to `www.codebuddy.cn`). `auth.login.start`/`auth.login.poll` model this correctly, so it is not fatal — but it cannot be automated. **[V]** for the URL source, **[D]** for the QR/WeChat step. |
| Region products collapse to one key | Four Jet-Hub provider ids collapse into `codebuddy`. Model pools differ per `endpoint` **[D]**; the Go config must keep the product table even though only one key is registered. **[V]** for the table, **[I]** for the single-key consequence. |
| `/v3/config` UA gating | Wrong UA yields HTTP 200 with an error body **[D]**. Model parsing must validate the body, not the status. |

## 4. Qoder (`qoder`)

### 4.1 File inventory

| TS file | Lines | Purpose **[V]** |
| --- | --- | --- |
| `qoder.ts` | 342 | PKCE pair, device session, auth/poll URL builders, credential shape, token parsing, refresh body, request headers |
| `qoder-oauth.ts` | 255 | Login flow: browser open, device-token polling, flow result packaging |
| `qoder-auth.ts` | 481 | DSH service: credential parse, login, refresh, model refresh |
| `qoder-product.ts` | 415 | `QoderProduct` for the global and CN sites |
| `qoder-wasm.ts` | 613 | WASM glue: runtime auth fields, catalog decryption, encrypted inference client |
| `qoder-envelope.ts` | 140 | Strips the extra SSE envelope on the encrypted inference endpoint |
| `qoder-credits.ts` | 481 | Usage/campaigns endpoints, balance, campaigns, daily check-in |
| `qoder-adapter.ts` | 486 | `LlmAdapter`: model catalog, encrypted inference path, SSE consumption |
| `qoder-auth-wasm.wasm` | 298,606 bytes | The WASM module the glue loads |

### 4.2 Porting table

| TS file | Target Go file in `plugins/qoder/` | What must be ported | Difficulty | Notes |
| --- | --- | --- | --- | --- |
| `qoder.ts:14-142` | `config.go` | Timeouts, PKCE alphabet, device-session shape, URL builders **[V]** | easy | Paths: `/device/selectAccounts`, `/api/v1/deviceToken/poll`, `/api/v1/deviceToken/refresh`, `/api/v1/userinfo`, `/model/v1/chat/completions`. |
| `qoder.ts:56-78` | `session.go` | PKCE `S256` challenge: `base64url(sha256(verifier))` without padding **[V]** | easy | `crypto/sha256` + `encoding/base64.RawURLEncoding`. |
| `qoder.ts:109-134` | `oauth.go` | Auth URL on `authBase`, poll URL on `openApiBase` **[V]** | easy | Different hosts on purpose: polling `qoder.com` returns 401 (`qoder.ts:123-125`). |
| `qoder.ts:144-341` | `auth.go` | Credential double-write of `security_oauth_token`/`access_token`, `machine_id` persistence, token payload parsing, expiry, refresh body, bearer header **[V]** | medium | `machine_id` must persist; `uid` is required by encrypted inference (`qoder.ts:154-161`). |
| `qoder-oauth.ts:119-254` | `oauth.go` | Device-token poll loop and flow result **[V]** | easy | Plain HTTP polling with retry/failure budget. |
| `qoder-oauth.ts:205-243` | `plugin.go` | `startQoderLoginFlow` split into start/poll **[V]** | easy | Maps directly onto `auth.login.start` / `auth.login.poll`. |
| `qoder-product.ts:319-405` | `config.go` | `QODER` and `QODER_CN` hosts, client ids, client metadata **[V]** | easy | Global: `encryptedInferBase` `api2.qoder.sh` vs `inferBase` `api2-v2.qoder.sh`. CN: both are `gateway.qoder.com.cn`. |
| `qoder-envelope.ts:34-70` | `envelope.go` | Strip the `{headers,body,statusCodeValue}` wrapper from each SSE frame **[V]** | easy | The inner `body` is **not** encrypted; this is JSON unwrapping only (`qoder-envelope.ts:13`). |
| `qoder-credits.ts:75-77`, `162-481` | `credits.go` | Usage, campaigns, claim and check-in **[V]** | medium | `GET /sash/api/v2/me/usage`, `GET /sash/api/v1/me/campaigns`. Claim idempotency is a body field (`replayed:true`) **[D]**. |
| `qoder-adapter.ts:162-200` | `models.go` | Model catalog with promotion windows and fallback table **[V]** | medium | Promotion state must be computed from the schedule for "now", not from an `enabled` snapshot **[D]**. |
| `qoder-adapter.ts:284-320` | `adapter.go` | Encrypted inference: build via WASM context, pass WASM headers through verbatim **[V]** | **hard** | The WASM-generated `Authorization: Bearer COSY.<payload>.<sig>` must not be replaced; overriding it yields `403 Signature invalid` (`qoder-wasm.ts:491-496`). |
| `qoder-adapter.ts:338` | `adapter.go` | `consumeOpenAiSse` over the unwrapped stream **[V]** | medium | `openai-compat.ts` is qoder-only (`openai-compat.ts` imported by `qoder-adapter.ts` alone). |
| `qoder-wasm.ts:43-199` | `wasm.go` | Load `qoder-auth-wasm.wasm`, `COSY_VERSION` `1.1.49`, glue imports **[V]** | **hard** | Needs an embedded WASM runtime; the module is 292 KB. |
| `qoder-wasm.ts:391-432` | `wasm.go` | `generate_runtime_auth_fields`, `model_cache_decrypt` calls **[V]** | **hard** | `model_cache_decrypt` requires `machineId` as the second argument; omitting it surfaces as `AES-GCM decrypt failed` (`qoder-wasm.ts:414-416`). |
| `qoder-wasm.ts:440-608` | `wasm.go` | `QoderEncryptedInfer`: `qodercontext_new`, `prepareInfer`, request body shape **[V]** | **hard** | Body must replicate the official `G4A()` structure; an empty `chat_context` yields a runtime failure (`qoder-wasm.ts:503-505`). |

### 4.3 Verified endpoints

| Item | Value | Source |
| --- | --- | --- |
| Device select | `/device/selectAccounts` | `qoder.ts:23` |
| Token poll | `/api/v1/deviceToken/poll` | `qoder.ts:25` |
| Refresh | `/api/v1/deviceToken/refresh` | `qoder.ts:27` |
| User info | `/api/v1/userinfo` | `qoder.ts:29` |
| Public chat | `/model/v1/chat/completions` | `qoder.ts:31` |
| Usage | `/sash/api/v2/me/usage` | `qoder-credits.ts:75` |
| Campaigns | `/sash/api/v1/me/campaigns` | `qoder-credits.ts:77` |
| Global hosts | authBase `https://qoder.com`, openApiBase `https://openapi.qoder.sh`, inferBase `https://api2-v2.qoder.sh`, encryptedInferBase `https://api2.qoder.sh` | `qoder-product.ts:323-326` |
| CN hosts | authBase `https://qoder.cn`, openApiBase `https://openapi.qoder.com.cn`, inferBase = encryptedInferBase = `https://gateway.qoder.com.cn` | `qoder-product.ts:380-384` |
| Global client id | `e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb` | `qoder-product.ts:327` |
| WASM path | `qoder-auth-wasm.wasm` next to the module | `qoder-wasm.ts:43` |
| Client version constant | `COSY_VERSION = '1.1.49'` | `qoder-wasm.ts:66` |

I did **not** find a literal encrypted-inference request path in the TypeScript: `prepareInfer`
returns a URL produced by the WASM context (`qoder-wasm.ts:497`). Do not assume it is
`/model/v1/chat/completions`.

### 4.4 Crypto and signing

| Item | Detail | Source |
| --- | --- | --- |
| PKCE challenge | `sha256(verifier)` → base64url, no padding | `qoder.ts:45,63` |
| Device identity | `randomUUID` + `randomBytes` for `machine_id`/session | `qoder.ts:10,89` |
| Encrypted inference signing | Performed **inside the WASM**; the plugin only passes the resulting headers through | `qoder-wasm.ts:478-496` |
| Runtime auth fields | `generate_runtime_auth_fields` returns `encrypt_user_info` and `key` | `qoder-wasm.ts:391-404`, `:75-77` |
| Catalog decryption | `model_cache_decrypt(ciphertext, machineId)` | `qoder-wasm.ts:422-431` |

The signature algorithm is not implemented in TypeScript, so it cannot be ported by reading
TypeScript. It lives in `qoder-auth-wasm.wasm`, which is shipped as a binary asset.

### 4.5 Login flow **[V]**

1. Generate a PKCE pair and a device session carrying `nonce` and `machine_id` (`qoder.ts:56-106`).
2. Open `{authBase}/device/selectAccounts?challenge&challenge_method=S256&nonce&machine_id&client_id` (`qoder.ts:109-118`).
3. Poll `{openApiBase}/api/v1/deviceToken/poll?nonce&verifier&challenge_method=S256` (`qoder.ts:127-134`).
4. Exchange/persist the token; refresh via `/api/v1/deviceToken/refresh` (`qoder.ts:27`).

### 4.6 Blockers

| Blocker | Detail | Confidence |
| --- | --- | --- |
| **WASM runtime is mandatory for encrypted inference** | `qoder-wasm.ts` compiles and instantiates the module in-process (`qoder-wasm.ts:207,368`). A Go port needs a WASM runtime. `wazero` is the standard choice, but the task constraint allows **no new external dependencies** — so encrypted inference is blocked until that constraint is lifted or the host exposes a WASM facility. | **[V]** for the WASM requirement, **[I]** for the dependency conflict |
| Encrypted inference needs `uid` | `generate_runtime_auth_fields` takes `uid`; the credential must carry it from the device-token response (`qoder.ts:154-161`, `:222-226`). | **[V]** |
| Two hosts, two model namespaces | The encrypted path (`api2.qoder.sh`) accepts catalog keys; the public path (`api2-v2.qoder.sh`) accepts generic names. Mixing them returns 404. | **[D]**, consistent with `qoder-product.ts:323-326` **[V]** |
| Public endpoint falls back | `listModels` uses a fallback table because the signed catalog endpoint needs WASM (`qoder-wasm.ts:419-420`). | **[V]** |

## 5. TRAE (`trae`)

### 5.1 File inventory

| TS file | Lines | Purpose **[V]** |
| --- | --- | --- |
| `trae.ts` | 1714 | Path constants, credential, request headers, device-id derivation, SOLO body transform, exchange/user-info parsing, model metadata, SSE line parsing, OpenAI chunk builder |
| `trae-oauth.ts` | 773 | Login URL, callback parsing, callback HTTP server, `ExchangeToken` + `GetUserInfo` |
| `trae-auth.ts` | 546 | DSH service: parse, login, refresh, model refresh |
| `trae-adapter.ts` | 1396 | `LlmAdapter`: message serialization, channel routing, request build, SOLO SSE → OpenAI chunks, history trimming |
| `trae-product.ts` | 385 | `TraeProduct` for CN and international sites |
| `trae-credits.ts` | 382 | Check-in status/claim and credit balance |
| `trae-errors.ts` | 157 | Error classification, account rotation and rate-limit predicates |

### 5.2 Porting table

| TS file | Target Go file in `plugins/trae/` | What must be ported | Difficulty | Notes |
| --- | --- | --- | --- | --- |
| `trae.ts:44-65` | `config.go` | All path constants | easy | See the endpoint table in 5.3. |
| `trae-product.ts:162-202`, `304-380` | `config.go` | CN/INTL hosts, `clientId`, `ideVersion`, `pluginVersion` | easy | Same `clientId` on both sites; hosts differ. |
| `trae.ts:89-175` | `auth.go` | Credential shape, expiry/refreshable predicates | easy | |
| `trae.ts:176-295` | `headers.go` | `traeSOLOHeaders`, `traeUgHeaders`, `traeCheckinHeaders`, `traeOAuthHeaders` | medium | `Cloud-IDE-JWT <token>` plus ~10 `X-*` identity headers **[D]**, header builders **[V]**. |
| `trae.ts:296-379` | `session.go` | `deriveDeviceId15`, `deriveMarketUserId`, `deriveSessionId`, `seededStream`, `seededDigits`, `uuidV4`, `randomHex` | medium | `seededStream` is a `sha256`-based deterministic byte stream (`trae.ts:325-349`). `machine_id` must never be regenerated on refresh; `device_id` must differ per account **[D]**. |
| `trae.ts:380-524` | `auth.go` | `parseTraeExchangeResponse`, `parseTraeUserInfoResponse`, `buildTraeCredential`, `applyTraeRefresh` | medium | |
| `trae-oauth.ts:71-127` | `oauth.go` | `machineTraceId`, `buildTraeLoginURL` | easy | Console `/authorization` with `login_trace_id`, `auth_callback_url`, `machine_id`, `device_id`. |
| `trae-oauth.ts:216-364` | `oauth.go` | `fixNicknameMojibake`, `parseTraeCallback`, `parseTraeCallbackDetailed` | medium | Callback carries `refreshToken` and a user JWT; nickname needs mojibake repair. |
| `trae-oauth.ts:365-440` | `oauth.go` | `exchangeTraeCallback`: `ExchangeToken` then `GetUserInfo` | medium | `GetUserInfo` failure must not abort the flow (`trae-oauth.ts:405-430`). |
| `trae-oauth.ts:40`, `471-773` | `oauth.go` | Local callback server on port 18080 with fallback | **hard** | The listener itself is covered by `internal/jethub/oauthcb`; TRAE-specific is the fixed port 18080 plus the fallback policy. Listen errors must become a rejected promise, not a process crash (`trae-oauth.ts:442-470`). |
| `trae.ts:525-810` | `models.go` | Model metadata: context window, max output, reasoning effort, `isTraeModelCallable` | medium | Model callability is channel-dependent **[D]**. |
| `trae.ts:1342-1539` | `translate.go` | `transformToSOLOBody`: OpenAI → SOLO body conversion, tools serialized as JSON strings | **hard** | Must run **after** DSH-native block serialization (`trae-adapter.ts:269-309`). |
| `trae.ts:1540-1636` | `solo.go` | `parseTraeSSELine`, `buildOpenAIChunk`, `OPENAI_DONE`, SOLO event handling | **hard** | SOLO is a custom SSE protocol (`output`/`token_usage`/`done`/`error`) **[D]**. |
| `trae-adapter.ts:304-393` | `translate.go` | `serializeTraeMessages` | medium | |
| `trae-adapter.ts:485-1000` | `adapter.go` | Request build, channel selection, max-mode fields, image capability | **hard** | Channel choice is per model; a wrong channel yields stream-local `4001` **[D]**. |
| `trae-adapter.ts:1052-1310` | `adapter.go` | `consumeSse` → OpenAI `StreamChunk` | **hard** | |
| `trae-adapter.ts:1311-1384` | `adapter.go` | History trimming (`resolveMaxHistoryChars`, `wireMessageSize`, `trimTraeHistory`) | medium | |
| `trae-credits.ts:186-380` | `credits.go` | Check-in status/claim and balance | medium | |
| `trae-errors.ts:80-157` | `errors.go` | Error classification, rotate/rate-limit/terminal predicates | easy | Small and self-contained. |
| `trae-auth.ts:52-546` | `plugin.go` | Service wiring | n/a | Not ported. **[I]** |

### 5.3 Verified endpoints

All from `trae.ts` unless noted.

| Item | Value | Source |
| --- | --- | --- |
| Chat | `/api/agent/v3/llm_utils_chat` | `trae.ts:44` |
| Model detail | `/api/ide/v1/get_detail_param` | `trae.ts:46` |
| Model batch detail | `/api/ide/v1/batch_get_detail_param` | `trae.ts:53` |
| Exchange token | `/cloudide/api/v3/trae/oauth/ExchangeToken` | `trae.ts:55` |
| User info | `/cloudide/api/v3/trae/GetUserInfo` | `trae.ts:57` |
| Check-in status | `/trae/api/v2/ug/checkin_credits/status` | `trae.ts:59` |
| Check-in claim | `/trae/api/v2/ug/checkin_credits/claim` | `trae.ts:61` |
| Entitlement usage | `/trae/api/v2/pay/ide_user_ent_usage` | `trae.ts:63` |
| Callback path | `/authorize` | `trae.ts:65` |
| Login page | `{consoleHost}/authorization` | `trae-oauth.ts:126` |
| CN hosts | agent `https://trae-api-cn.mchost.guru`, ug `https://api.trae.cn`, oauth `https://api.trae.com.cn`, console `https://www.trae.cn` | `trae-product.ts:162-168` |
| INTL hosts | agent `https://api5-normal-alisg.mchost.guru`, ug `https://api.trae.ai`, oauth `https://api.trae.ai`, console `https://www.trae.ai` | `trae-product.ts:196-202` |
| Client id | `en1oxy7wnw8j9n` | `trae-product.ts:312`, `:357` |
| IDE / plugin version | `0.1.52` / `2.3.62834` | `trae-product.ts:314,324` |
| Callback port | `18080` | `trae-oauth.ts:40` |

The batch detail endpoint is `batch_get_detail_param`, not `get_detail_param` — both constants
exist and the adapter must pick the batch one for catalogs **[V]** (`trae.ts:46,53`), **[D]** for the
routing consequence.

### 5.4 Crypto **[V]**

- `createHash('sha256')` is used for the seeded byte streams that derive device/market/session
  identifiers (`trae.ts:335,1149,1176`) and for `machineTraceId` (`trae-oauth.ts:71-98`).
- `crypto.getRandomValues` fills random identifiers (`trae.ts:370,1098,1116`).
- **No request signing**: authentication is `Cloud-IDE-JWT` plus identity headers **[D]**; I found
  no HMAC or signature construction in `trae*.ts` **[V]**.

### 5.5 Login flow **[V]**

1. Build `{consoleHost}/authorization` with `auth_type=local`, `login_trace_id`, `auth_callback_url`,
   `machine_id`, `device_id` (`trae-oauth.ts:99-127`).
2. Listen on `127.0.0.1:18080` (with fallback) for `{callback}` carrying query params and a user JWT
   (`trae-oauth.ts:442-470`).
3. Parse the callback; `POST {oauthHost}/cloudide/api/v3/trae/oauth/ExchangeToken` with
   `{ClientID, RefreshToken, ClientSecret:"-", UserID:""}` (`trae-oauth.ts:375-393`).
4. `POST {oauthHost}/cloudide/api/v3/trae/GetUserInfo` to fill `uid`/nickname/enterprise; failure
   falls back to callback values (`trae-oauth.ts:412-430`).

### 5.6 Blockers

| Blocker | Detail | Confidence |
| --- | --- | --- |
| Request **and** response translation | The adapter converts OpenAI → SOLO on the way out and SOLO SSE → OpenAI chunks on the way back. `request.translate` and `response.translate` must be real implementations, not pass-through. | **[V]** for the import of `transformToSOLOBody` and `consumeSse`; **[D]** for protocol details |
| Message serialization order | DSH-native content blocks must be serialized first; feeding them to `transformToSOLOBody` produces no error but hides tool calls and results from the model. | **[D]**, supported by `trae-adapter.ts:269-309` **[V]** |
| Two sites, two hosts | CN and INTL hosts and login states are separate; credential and model availability differ. | **[V]** for hosts |
| Callback server on a fixed port | Port 18080 may be occupied; the fallback and the error-to-rejection handling are both required. `internal/jethub/oauthcb` supplies the listener but not the port-fallback policy. | **[V]** |
| Channel-scoped models | A model is callable only on the channel that listed it; the wrong channel fails inside the stream with `4001`. | **[D]** |
| Max mode | Only models whose remote config sets `max_mode === true` may receive the Max fields. | **[D]**, consistent with `trae-adapter.ts:583-584` **[V]** |

## 6. LobsterAI (`lobsterai`)

### 6.1 File inventory

| TS file | Lines | Purpose **[V]** |
| --- | --- | --- |
| `lobsterai.ts` | 604 | Path constants, credential, envelope parsing, token payload, uid derivation, request header builders, client-version resolver |
| `lobsterai-oauth.ts` | 358 | Login session, login URL, auth-code exchange, random-port callback server |
| `lobsterai-auth.ts` | 603 | DSH service: parse, login, refresh, model refresh |
| `lobsterai-adapter.ts` | 1344 | `LlmAdapter`: thinking config, model parsing, message serialization, chat, SSE |
| `lobsterai-product.ts` | 223 | API/portal hosts, client version API, capabilities, UA |
| `lobsterai-credits.ts` | 372 | Activity slot/context, check-in claim, credit balance |
| `lobsterai-errors.ts` | 171 | Error classification and rotation predicates |

### 6.2 Porting table

| TS file | Target Go file in `plugins/lobsterai/` | What must be ported | Difficulty | Notes |
| --- | --- | --- | --- | --- |
| `lobsterai.ts:32-57` | `config.go` | Path constants | easy | See 6.3. |
| `lobsterai-product.ts:63-129`, `199-219` | `config.go` | API/portal hosts, version API, fallback version, capabilities, UA | easy | `https://lobsterai-server.youdao.com`, `https://lobsterai.youdao.com`. |
| `lobsterai.ts:79-133` | `auth.go` | Credential shape, envelope `{code,message,data}` parsing | easy | |
| `lobsterai.ts:195-232` | `auth.go` | Expiry and refreshable predicates | easy | |
| `lobsterai.ts:234-274` | `auth.go` | `lobsteraiKeyfromBody`, `lobsteraiRefreshBody` | medium | `firstKeyfrom`/`latestKeyfrom` must round-trip with the credential. |
| `lobsterai.ts:275-420` | `auth.go` | Token payload parsing, uid resolution, `buildLobsteraiCredential`, `applyLobsteraiRefresh` | medium | `uid` is `sha256(accessToken)` hex truncated to 16 chars (`lobsterai.ts:334`). |
| `lobsterai.ts:421-516` | `headers.go` | Auth, chat, models and anonymous header builders | easy | `Bearer` + `X-LobsterAI-Client-*`; **no signing** **[D]**. |
| `lobsterai.ts:517-604` | `version.go` | `parseClientVersion`, `parseClientVersionFromUpdate`, `LobsteraiClientVersionResolver` | medium | Version is fetched at runtime and cached; the exchange request needs it (`lobsterai-oauth.ts:138-144`). |
| `lobsterai-oauth.ts:83-116` | `oauth.go` | Login session (`uuid`, `firstKeyfrom`), login URL | easy | `{portalBase}/portal#/login?source=electron&redirect_uri&state`. |
| `lobsterai-oauth.ts:130-185` | `oauth.go` | `exchangeLobsteraiAuthCode` | medium | Body must carry exactly `authCode`, `firstKeyfrom`, `latestKeyfrom`, `uuid`, `version`; no `Authorization` header. |
| `lobsterai-oauth.ts:232-358` | `oauth.go` | Callback server on a random port, start/poll split | medium | `http://127.0.0.1:<port>/auth/callback`. `internal/jethub/oauthcb` covers the listener (`Start`, `Port()`, `RedirectURI()`, `Wait`); the start/poll split maps onto `auth.login.start`/`auth.login.poll`. |
| `lobsterai.ts:119-133`, `162-193` | `envelope.go` | Success/failure envelope parsing | easy | All endpoints wrap payloads in `{code,message,data}`. |
| `lobsterai-adapter.ts:104-305` | `models.go`, `thinking.go` | Thinking config parsing (`level` vs `openclawLevel`), model array parsing, models query | hard | The wire value is `openclawLevel`; the display name is `level`. The remote maps `level:'max'` → `openclawLevel:'xhigh'` (`lobsterai-adapter.ts:55-58,98-103`). |
| `lobsterai-adapter.ts:428-533` | `adapter.go` | Message serialization | medium | |
| `lobsterai-adapter.ts:597-1306` | `adapter.go` | Chat request and SSE consumption | medium | Endpoint and headers are OpenAI-compatible; `delta.content`/`delta.reasoning_content` are explicitly nullable **[D]**. |
| `lobsterai-credits.ts:36-40`, `163-370` | `credits.go` | Activity slot/context, check-in claim, balance | medium | `POST /api/client-activities/slot`, `GET /api/client-activities`, `GET /api/user/profile-summary`. |
| `lobsterai-errors.ts:100-171` | `errors.go` | Error classification, rotation predicates, hard-credit and dead-session markers | easy | |
| `lobsterai-auth.ts:55-603` | `plugin.go` | Service wiring | n/a | Not ported. **[I]** |

### 6.3 Verified endpoints

All from `lobsterai.ts` unless noted.

| Item | Value | Source |
| --- | --- | --- |
| Exchange | `/api/auth/exchange` | `lobsterai.ts:32` |
| Refresh | `/api/auth/refresh` | `lobsterai.ts:34` |
| Models | `/api/models/available` | `lobsterai.ts:36` |
| Chat | `/api/proxy/v1/chat/completions` | `lobsterai.ts:45` |
| Callback | `/auth/callback` | `lobsterai.ts:47` |
| Activity slot | `/api/client-activities/slot` | `lobsterai-credits.ts:36` |
| Activity context | `/api/client-activities` | `lobsterai-credits.ts:38` |
| Profile summary | `/api/user/profile-summary` | `lobsterai-credits.ts:40` |
| API host | `https://lobsterai-server.youdao.com` | `lobsterai-product.ts:63` |
| Portal host | `https://lobsterai.youdao.com` | `lobsterai-product.ts:77` |
| Version API | `https://api-overmind.youdao.com/openapi/get/luna/hardware/lobsterai/prod/update` | `lobsterai-product.ts:85-86` |
| Fallback version | `2026.9.4` | `lobsterai-product.ts:98` |
| User agent | `LobsterAI/0.1.0` | `lobsterai-product.ts:129` |
| Client capabilities | `kimi-k3-agentic-v1,thinking-level-control-v1` | `lobsterai-product.ts:116-117` |
| Slot constants | placement `desktop_sidebar`, API version `2`, platform `win32` | `lobsterai-credits.ts:50-52` |

### 6.4 Crypto **[V]**

- `sha256(accessToken)` hex, first 16 characters, is used as the resolved `uid`
  (`lobsterai.ts:334`).
- No request signing: authentication is `Bearer` plus `X-LobsterAI-Client-*` headers
  (`lobsterai.ts:421-516`).
- No WASM, no asymmetric crypto, no HMAC anywhere in `lobsterai*.ts` **[V]**.

### 6.5 Login flow **[V]**

1. Create a login session with a random `uuid` and a `firstKeyfrom` value
   (`lobsterai-oauth.ts:83-102`).
2. Open `{portalBase}/portal#/login?source=electron&redirect_uri=http://127.0.0.1:<port>/auth/callback&state=...`
   (`lobsterai-oauth.ts:104-116`).
3. Receive the authorization code on the local callback server.
4. `POST {apiBase}/api/auth/exchange` with `{authCode, firstKeyfrom, latestKeyfrom, uuid, version}` and
   no `Authorization` header (`lobsterai-oauth.ts:130-159`, `:138-144`).

### 6.6 Blockers

| Blocker | Detail | Confidence |
| --- | --- | --- |
| Client version is a fetch-time dependency | The exchange body needs `version`, which is resolved dynamically from the update endpoint with a cached fallback. The port must implement the resolver before login can work. | **[V]** |
| Random-port local callback | The redirect URI embeds the port the plugin is listening on, so start and poll must share session state across calls. `internal/jethub/oauthcb` returns the bound port for exactly this purpose. | **[V]** |
| Thinking level naming | Sending `reasoning_effort: 'max'` behaves like the server default; the wire value must be `openclawLevel` (`xhigh`). | **[D]**, consistent with `lobsterai-adapter.ts:98-103` **[V]** |
| Nullable SSE deltas | `delta.content` and `delta.reasoning_content` may be explicit `null`; type-checking before use is required. | **[D]** |

## 7. Cross-provider notes

| Topic | Detail | Confidence |
| --- | --- | --- |
| No dependency additions | The port must stay on stdlib plus `sdk/{pluginabi,pluginapi}`. Qoder's WASM and any HTTP/2 or fingerprinting need would require new modules; that is a constraint conflict to resolve before those parts can be built. | **[I]** |
| Shared SSE helper | `internal/jethub/sse` already exists with a frame `Scanner` and `Encode`/`DoneEvent`. All four providers consume SSE; the idle-timeout and tool-pairing logic from `sse.ts` is still missing there. | **[V]** for the Go package, **[V]** for the TS helpers |
| Credential field names | Jet-Hub credential JSON keeps provider-specific snake_case keys (`access_token`, `refresh_token`, `security_oauth_token`, `machine_id`, `expire_time`). Keeping the same names makes existing auth material interchangeable. | **[V]** for CodeBuddy/Qoder/LobsterAI shapes |
| Two-step login is mandatory | `auth.login.start` returns the URL; `auth.login.poll` advances the flow. A blocking implementation breaks the browser gesture and the settings page. | **[D]** |
| Refresh must ignore "disabled" | Refresh must be driven by "is this credential refreshable", not by the account's enabled flag. | **[D]** |

## 8. Verification ledger

Read in full or in the cited ranges for this document:

- `qoder.ts`, `qoder-envelope.ts`, `qoder-wasm.ts` (lines 385-514), `qoder-adapter.ts` (targeted), `qoder-product.ts` (targeted), `qoder-credits.ts` (targeted), `qoder-oauth.ts` (targeted)
- `buddy.ts` (targeted), `buddy-oauth.ts` (lines 137-256, 510-589), `buddy-adapter.ts` (targeted), `product.ts` (lines 74-143, 256-315), `credits.ts` (targeted)
- `trae.ts` (targeted), `trae-oauth.ts` (lines 96-135, 365-474), `trae-product.ts` (targeted), `trae-adapter.ts` (targeted), `trae-credits.ts` (targeted), `trae-errors.ts` (targeted)
- `lobsterai.ts` (targeted), `lobsterai-oauth.ts` (lines 104-193), `lobsterai-product.ts` (targeted), `lobsterai-adapter.ts` (targeted), `lobsterai-credits.ts` (targeted), `lobsterai-errors.ts` (targeted)
- `sse.ts` (header), `openai-compat.ts` (header and export list), `account-pool.ts` (header), `refresh.ts` (header), `service.ts` (header), `login.ts` (header), `oauth.ts` (header)

Not verified, and therefore not asserted anywhere above:

- The encrypted-inference request path (produced inside the WASM at runtime).
- The signature algorithm and key derivation inside `qoder-auth-wasm.wasm` (binary asset; not read).
- Any endpoint, header, or algorithm in `antigravity*` (out of scope) or in the CodeArts files
  owned by `plugins/codearts/`.
- Runtime behaviour of the endpoints. Every path above is a constant read from source; none was
  exercised against a live server.
