# ZCode (智谱 z.ai) native CPA plugin

This directory is a single `package main` that builds as a CLIProxyAPI native
plugin. It implements the ZCode free-quota channel without the obsolete browser /
Aliyun-captcha inference machinery.

## Protocol facts

- **Inference endpoint:** `POST https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages`.
  This is Anthropic Messages with streaming SSE. There is no OpenAI-shaped
  `/v1/chat/completions` path under `zcode-plan` (it returns 404).
- **Executor format:** input and output are both `anthropic`. CPA maps that literal
  to its Claude format and performs any OpenAI↔Anthropic conversion itself. The
  plugin only injects the identity structure and relays complete Anthropic SSE
  frames.
- **Identity gate:** `system` is an array of separate text blocks in the order
  `official CLI prefix → official stable sections → # Environment → caller
  system`, and the first user turn begins with the official `<system-reminder>`
  date block. Flattening the first ~2900 characters into one block still triggers
  upstream code 3012; the structure is required. The constants in
  `identity_constants.go` were programmatically extracted from the TypeScript
  reference. Do not reformat them.
- **3012 warning:** a mismatch carries an account penalty — 30 minutes, 24 hours
  from the third occurrence within 24 hours, disablement on the fifth. The plugin
  never automatically retries a 3012, and no test contacts the live endpoint.
- **Captcha:** empirical verification shows inference succeeds without any captcha
  header; a deliberately bogus captcha parameter also succeeds. The official
  client now uses the captcha header only on `billing/claim`, reactively. This
  plugin never opens a browser and never mints captchas. A claim is attempted
  without the header and reports an explicit 3007 error if the server demands one.
- **Models:** only `GLM-5.3` and `GLM-5.3-Flash` are exposed. The other two ids in
  the upstream pool return empty responses under Start Plan (measured 0/3 versus
  3/3 for GLM-5.3). Context/output/vision/reasoning data is read from
  `client/configs`, with the measured table as a fallback.
- **Thinking level wire field:** `output_config.effort`, not
  `reasoning_effort`. Upstream's own `client/configs` publishes that path.
- **Credential:** `zcode_jwt` is a long-lived JWT with no `exp` claim.
  `user_id` is the stable account identity; `device_mid` is locally generated
  per login and must not be used for de-duplication. It is nevertheless a hard
  request requirement: omitting `X-Device-Mid` yields HTTP 400 / code 3001.
- **Login:** server-mediated device authorization — `/oauth/cli/init`, user opens
  the authorize URL, `/oauth/cli/poll/<flow_id>`. No local callback listener.
- **Quota/check-in:** `billing/balance`; then for the daily allowance, report
  `app_launch` and `app_daily_active` before `billing/preview`, then claim each
  returned plan. Balance units are rendered as tokens, using the upstream
  `unit_type` / `meter` fields.

## Optional official-client credential import

The management login page can optionally adopt the official client's
`~/.zcode/v2/credentials.json` when `import_client_credential: true`.
The primary path remains the plugin's own login.

An encrypted value has the exact shape:

```text
enc:v1:<base64url(iv)>.<base64url(authTag)>.<base64url(ciphertext)>
```

with AES-256-GCM, a 12-byte IV, and a 16-byte authentication tag. The key is:

```text
sha256("zcode-credential-fallback:" + platform + ":" + homedir + ":" + username)
```

or `sha256($ZCODE_CREDENTIAL_SECRET)` when that variable is set. Any deviation in
prefix, platform spelling, home directory, username (whose failure fallback is
the literal `unknown`), separator or order produces a GCM authentication failure.
Values without the `enc:v1:` prefix are plaintext and are returned unchanged.

## Network rule

The plugin never opens a network connection. Every HTTP request goes through
`(*abiboot.Host).HTTPDo`, inheriting CPA's proxy, TLS and request logging.
`net/http` is used only for method/header/status types.

## Offline tests

`testdata/captured_stream.json` is a scrubbed fixture matching the verified live
capture shape (identity block, `GLM-5.3-Flash`, `max_tokens: 64`, Anthropic SSE
with message start, ping, thinking/signature deltas, text delta, block stops,
usage and message stop). It contains placeholders, not a live token. All tests
are deterministic and use an in-process fake host transport.
