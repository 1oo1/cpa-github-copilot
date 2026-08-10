# CLIProxyAPI GitHub Copilot Plugin

Go `c-shared` 插件，将 GitHub Copilot 订阅作为 CLIProxyAPI v7 的账户级 OAuth
provider `github-copilot` 接入；配置 ID 为 `github-copilot-go`。

| 客户端 API | 路径 |
|---|---|
| OpenAI Chat Completions | `/v1/chat/completions` |
| OpenAI Responses | `/v1/responses` |
| Anthropic Messages | `/v1/messages` |

插件根据当前账户的 Copilot 模型目录选择上游协议，必要时调用 CLIProxyAPI
translator 转换请求与响应。HTTP wire 基线固定为 VS Code `1.132.0`、Copilot Chat
`0.60.0`、`@vscode/copilot-api@0.4.3` 和 API version `2026-06-01`；`pi` 只用于
旧凭据兼容。

> Copilot token broker 和模型接口不是稳定的通用第三方 API。使用前请确认符合订阅
> 条款和组织策略。

## 前置条件

- 兼容当前 ABI/translator 的 CLIProxyAPI v7；
- 有效的 GitHub Copilot 订阅；
- 源码构建需要 Go 1.26+、CGO、C 编译器及相邻的 `../CLIProxyAPI` checkout。

## 安装

推荐将本仓库的 [registry.json](registry.json) 加入 CLIProxyAPI Plugin Store：

```yaml
plugins:
  enabled: true
  dir: "/path/to/plugins"
  store-sources:
    - "https://raw.githubusercontent.com/1oo1/cpa-github-copilot/main/registry.json"
  configs:
    github-copilot-go:
      enabled: true
      priority: 100
```

重载后在 Management Center 安装 `GitHub Copilot`，或调用：

```bash
curl -X POST \
  -H "Authorization: Bearer <MANAGEMENT_KEY>" \
  "http://127.0.0.1:<PORT>/v0/management/plugin-store/github-copilot-go/install"
```

追加 `?version=X.Y.Z` 可安装指定版本（不带 `v`）。手工安装时运行 `make build`，
再把生成的动态库放入 `plugins.dir` 对应平台目录；准确产物路径和可覆盖变量以
[Makefile](Makefile) 为准。

## 配置

Store 与手工安装共用 `plugins.configs.github-copilot-go`：

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `client_id` | `Iv1.b507a08c87ecfe98` | Device Flow public client ID，不是 secret |
| `github_host` | `github.com` | GitHub.com 或受信任的 GitHub Enterprise 主机名 |
| `enable_models` | `true` | 登录后 best-effort 启用已知模型 policy |
| `model_cache_ttl_seconds` | `300` | 非空账户模型目录缓存秒数；`0` 表示每次刷新 |
| `max_stream_buffer_bytes` | `4194304` | 未完成 SSE event 的缓存上限，范围 64 KiB–64 MiB |
| `web_search_model` | `gpt-5.6-terra` | 仅将 Anthropic 原生 Web Search 独占请求分流到该 Responses 模型；空值禁用 |
| `enable_responses_context_management` | `true` | 为符合条件的原生 Responses 请求启用服务端 compaction |

Enterprise 主机必须是 HTTPS DNS 主机名，不能含用户信息、端口、路径、查询、
fragment 或 IP，并需要该实例可用的 OAuth public client ID。

Claude Code 的 `web_search_20250305`/`web_search_20260209` 是提供方执行的服务端工具，
Copilot Claude Messages 路由不执行它。插件只在工具列表全部为原生 Web Search 时使用
`web_search_model`，再将 Responses 结果转换回 Anthropic 事件；普通请求不切换，混合工具请求在本地拒绝。

## 登录与调用

启用插件并启动 CLIProxyAPI 后，通过 Management API 发起 Device Flow：

```http
GET /v0/management/github-copilot-auth-url
```

完成响应中 `url` 指向的 GitHub 授权，再轮询：

```http
GET /v0/management/get-auth-status?state=<state>
```

成功后，使用 CLIProxyAPI 原有鉴权访问 `/v1/models` 和上表三个推理端点。模型 ID
应来自当前账户的 `/v1/models`：

```json
{
  "model": "<MODEL_ID>",
  "messages": [{"role": "user", "content": "Explain this function"}],
  "stream": true
}
```

## 客户端契约与限制

- 插件不保存 conversation history。Responses 客户端负责保存并重交
  `previous_response_id`、compaction item 和 encrypted reasoning；包含 opaque state
  的跨协议转换会以 `format_mismatch` 拒绝。
- 不支持独立 `/v1/responses/compact`；使用 `/v1/responses` 的 inline
  `context_management`。
- Responses 流必须包含真实的 completed、incomplete、failed 或 error 终态；普通 EOF
  不会被合成为成功。
- 合法 UUID 形式的 `X-Client-Request-Id` 会映射为上游 request/task ID；调用方不能
  覆盖 Copilot Authorization、身份或 API version。

## 安全与诊断

- 所有上游 HTTP 和 streaming 都经 CLIProxyAPI host callback；推理仅允许访问凭据
  解析出的 HTTPS API origin。
- GitHub access token 只用于 broker/GitHub account，Copilot session token 只用于模型、
  policy 和推理。
- token 仅持久化在 provider-owned `StorageJSON`；凭据文件是明文 JSON，应保护
  CLIProxyAPI auth 目录及备份。
- 插件不记录 token、Authorization、`RawJSON`、`StorageJSON`、device/user code 或
  上下游正文。`debug: true` 仅增加结构化诊断字段。

## 开发

```bash
make test
make vet
go test -race ./...
make integration
```

`make integration` 会构建本机动态库，并通过真实 CLIProxyAPI loader 和 host callback
执行注册及请求路径。构建目标与参数以 [Makefile](Makefile) 为准，测试范围以
[src](src/) 中的 colocated tests 为准。

兼容性文档：

- [VSCODE_COPILOT_1_132_ARCHITECTURE.md](VSCODE_COPILOT_1_132_ARCHITECTURE.md)：
  pinned 上游实现依据；
- [PI_GITHUB_COPILOT_COMPARISON.md](PI_GITHUB_COPILOT_COMPARISON.md)：
  事实优先级、产品边界、legacy 范围与升级流程。
