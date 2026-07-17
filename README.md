# CLIProxyAPI GitHub Copilot Plugin

该插件把 GitHub Copilot 订阅作为 CLIProxyAPI 的原生 OAuth provider
`github-copilot` 接入。它通过 GitHub Device Flow 登录，用 GitHub access token
换取短期 Copilot session token，发现账号实际可用的模型，并提供以下入口：

- OpenAI Chat Completions (`/v1/chat/completions`)
- OpenAI Responses (`/v1/responses`)
- Anthropic Messages (`/v1/messages`)

插件会依据 Copilot `/models.supported_endpoints` 选择模型实际使用的上游协议。
入口协议不匹配时，使用 CLIProxyAPI 内置 translator 转换请求、非流式响应和
SSE 流，因此 Chat Completions 客户端也可以调用只支持 Responses 或 Messages
的 Copilot 模型。

> GitHub Copilot 的 token broker 和模型接口并非面向通用第三方客户端的稳定
> 公共 API，可能随 GitHub 服务更新而变化。使用前请确认符合你的订阅条款和
> 组织策略。

## 架构与安全边界

插件同时声明 `auth_provider`、`model_provider` 和 OAuth `executor`：

```text
GitHub Device Flow
  -> GitHub access token（长期，持久化）
  -> Copilot token broker（宿主 HTTP 桥）
  -> Copilot session token（短期，提前 5 分钟刷新）
  -> /models（账号模型发现）
  -> /chat/completions | /responses | /v1/messages
```

所有登录、轮询、刷新、模型发现、policy 和推理请求都调用
`host.http.do` / `host.http.do_stream`，不会绕过 CLIProxyAPI 的代理和请求日志
策略。插件自身不记录 `RawJSON`、`StorageJSON`、Authorization、access token、
session token、device code 或上游响应正文。

token 只保存在 provider-owned `StorageJSON`。`Metadata`、`Attributes`、标签和
错误消息只包含 provider、GitHub 主机与账号名等非敏感信息。凭据文件仍是明文
JSON，应保护 CLIProxyAPI 的 auth 目录和备份。

## 前置条件

- Go 1.26 或更高版本
- 可用的 C 编译器（Go `-buildmode=c-shared` 需要 CGO）
- 与该项目相邻的当前 CLIProxyAPI v7 源码：

```text
parent/
  CLIProxyAPI/
  cpa-github-copilot/
```

`go.mod` 的本地 `replace` 用于和当前插件 ABI/translator 保持一致。独立发布时可
改成已经包含这些 API 的 CLIProxyAPI v7 正式版本。

## 构建与测试

```bash
make test
make vet
make build
make integration
```

构建产物位于：

- macOS: `bin/github-copilot-go.dylib`
- Linux: `bin/github-copilot-go.so`
- Windows: `bin/github-copilot-go.dll`

`make integration` 会使用 CLIProxyAPI 的真实动态库 loader 装载产物，并验证
注册、`auth.parse` 的 `Handled` 行为和按凭据模型提供方能力。

## 安装与配置

把动态库放入 `plugins.dir` 或其当前平台子目录。动态库文件名决定插件配置 ID，
默认产物对应 `github-copilot-go`：

```yaml
plugins:
  enabled: true
  dir: "/path/to/plugins"
  configs:
    github-copilot-go:
      enabled: true
      priority: 100
      client_id: "Iv1.b507a08c87ecfe98"
      github_host: "github.com"
      enable_models: true
      model_cache_ttl_seconds: 300
      max_stream_buffer_bytes: 4194304
```

配置项：

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `client_id` | pi 使用的 GitHub OAuth 公共 client ID | Device Flow client ID，不是 client secret |
| `github_host` | `github.com` | GitHub.com 或管理员信任的 GitHub Enterprise 主机名 |
| `enable_models` | `true` | 登录后 best-effort 启用已知模型 policy |
| `model_cache_ttl_seconds` | `300` | 非空账号模型目录的复用时间；`0` 表示每次发现都刷新 |
| `max_stream_buffer_bytes` | `4194304` | 跨协议转换时单个未完成 SSE 事件的最大缓存 |

Enterprise 主机只能通过插件配置指定，必须是 HTTPS DNS 主机名，不能包含用户
信息、端口、路径、查询参数或 IP 地址。Enterprise 部署还需要配置可用于该实例
的 OAuth public client ID。

## 登录

启用插件并启动 CLIProxyAPI 后，使用现有 Management API 认证访问：

```http
GET /v0/management/github-copilot-auth-url
```

响应包含 `url` 和 `state`。打开 `url` 完成 GitHub 授权；插件已把 `user_code`
加入验证 URL 查询参数。随后按 CLIProxyAPI 现有 OAuth UI/客户端流程轮询：

```http
GET /v0/management/get-auth-status?state=<state>
```

插件严格遵守 device flow 的首次等待、`authorization_pending`、`slow_down`、过期
和拒绝状态。成功后宿主保存类似 `github-copilot-<login>.json` 的凭据文件。

`auth.parse` 只认顶层 `type: github-copilot`：不相关或无法识别的 JSON 返回
`Handled:false`；已识别但缺少 GitHub access token 的文件返回
`Handled:true` 且凭据被禁用，不会误交给其他 provider parser。

## API 使用

模型列表来自当前账号的 Copilot `/models`，并过滤掉 picker 未启用、policy 禁用或
明确不支持 tool calls 的项目：

```http
GET /v1/models
```

OpenAI Chat Completions 示例：

```json
{
  "model": "gpt-5.4",
  "messages": [{"role": "user", "content": "Explain this function"}],
  "stream": true
}
```

同一个模型也可从 Responses 入口调用；Claude 模型可从 Messages 入口原生调用。
插件会保护上游 Authorization，前端请求头不能覆盖 Copilot session token。

## 凭据兼容

新凭据使用语义化字段 `github_access_token`、`copilot_session_token`、
`refresh_after`、`github_host` 和 `models`。解析器也接受 pi 风格的旧字段：

- `refresh` -> `github_access_token`
- `access` -> `copilot_session_token`
- `expires` -> `refresh_after`
- `enterpriseUrl` -> `github_host`
- `availableModelIds` -> 迁移为带推断协议的模型条目

详细设计、风险审查和验证矩阵见 [PLAN.md](./PLAN.md)。
