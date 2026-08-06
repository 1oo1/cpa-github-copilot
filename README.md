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
  -> Copilot session token（短期，提前 10 分钟刷新）
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
- 构建全部 Linux 产物时，使用 x86-64 和 ARM64 Linux C 编译器（默认分别为
  `x86_64-linux-gnu-gcc` 和 `aarch64-linux-gnu-gcc`），或使用 Docker
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

`make build` 会先清空 `bin/`，然后构建全部支持的 Linux 平台架构，产物位于：

- Linux x86-64: `bin/linux/amd64/github-copilot-go.so`
- Linux ARM64: `bin/linux/arm64/github-copilot-go.so`

也可以用 `make build-linux-amd64` 或 `make build-linux-arm64` 只构建一个架构。
交叉编译器路径可通过 `LINUX_AMD64_CC` 和 `LINUX_ARM64_CC` 覆盖。每个
`build*` 目标都会先清空 `bin/`。找不到对应交叉编译器时，构建会自动使用
Docker 中的 `golang:1.26-bookworm`；镜像可通过 `DOCKER_GO_IMAGE` 覆盖。
构建版本默认取当前 checkout 的精确 Git tag；普通开发构建使用 `0.0.0-dev`，
也可以显式指定，例如 `make build VERSION=0.1.3`。

`make integration` 会使用 CLIProxyAPI 的真实动态库 loader 装载产物，并验证
注册、`auth.parse` 的 `Handled` 行为和按凭据模型提供方能力。该目标会自动构建
当前主机平台的测试产物。

## 通过 Plugin Store 安装与更新

本仓库自带 [registry.json](registry.json)，无需登记到官方 Plugin Store。在
CLIProxyAPI 配置中把它的 GitHub Raw URL 追加为 Store source：

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

重载配置后，从 Management Center 的 Plugin Store 找到 `GitHub Copilot` 并安装。
首次 Store 安装会把动态库写入 `plugins.dir/<goos>/<goarch>/`，并在插件配置中记录
该自定义 source 和已安装版本；之后发布新的 GitHub Release，CPA 就能检测并
安装更新，不需要手工复制动态库。

也可以通过 Management API 安装或更新到最新版本：

```bash
curl -X POST \
  -H "Authorization: Bearer <MANAGEMENT_KEY>" \
  "http://127.0.0.1:<PORT>/v0/management/plugin-store/github-copilot-go/install"
```

指定版本时使用不带 `v` 的版本号：

```bash
curl -X POST \
  -H "Authorization: Bearer <MANAGEMENT_KEY>" \
  "http://127.0.0.1:<PORT>/v0/management/plugin-store/github-copilot-go/install?version=0.1.3"
```

如果当前是手工安装，也应在配置 source 后从 Plugin Store 执行一次安装，让 CPA
写入 store manifest 并接管该插件。确认版本化文件正常加载后，可以删除原先无
版本的 `github-copilot-go.so`。

发布 tag 必须使用 `v<数字版本>`，例如 `v0.1.3`。Release workflow 会注入
`0.1.3` 作为插件上报版本，并生成 CPA 安装器要求的资产：

```text
github-copilot-go_0.1.3_linux_amd64.zip
github-copilot-go_0.1.3_linux_arm64.zip
checksums.txt
```

每个 ZIP 根目录只包含 `github-copilot-go.so`。发布新版本后无需修改本仓库的
registry，CPA 会从 GitHub Latest Release 获取版本和资产。

## 手工安装与配置

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
      enable_remote_compatibility: true
      remote_compatibility_cache_ttl_seconds: 14400
      max_stream_buffer_bytes: 4194304
```

配置项：

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `client_id` | pi 使用的 GitHub OAuth 公共 client ID | Device Flow client ID，不是 client secret |
| `github_host` | `github.com` | GitHub.com 或管理员信任的 GitHub Enterprise 主机名 |
| `enable_models` | `true` | 登录后 best-effort 启用已知模型 policy |
| `model_cache_ttl_seconds` | `300` | 非空账号模型目录的复用时间；`0` 表示每次发现都刷新 |
| `enable_remote_compatibility` | `true` | 从项目固定 manifest 应用较新的受限兼容覆盖 |
| `remote_compatibility_cache_ttl_seconds` | `14400` | manifest 成功检查后的缓存时间；`0` 表示每次检查 |
| `max_stream_buffer_bytes` | `4194304` | 跨协议转换时单个未完成 SSE 事件的最大缓存 |

Enterprise 主机只能通过插件配置指定，必须是 HTTPS DNS 主机名，不能包含用户
信息、端口、路径、查询参数或 IP 地址。Enterprise 部署还需要配置可用于该实例
的 OAuth public client ID。

### 远端兼容清单

启用 `enable_remote_compatibility` 后，模型列表刷新会通过宿主 HTTP 桥检查固定地址：

```text
https://raw.githubusercontent.com/1oo1/cpa-github-copilot/main/compatibility.json
```

请求不携带 GitHub 或 Copilot 凭据，只包含 JSON `Accept`、插件 `User-Agent` 和可选
ETag。成功响应缓存在 provider-owned `StorageJSON`；网络或解析失败时继续使用最后一次
有效缓存或内置规则。远端 `generated_at` 早于当前二进制内置 manifest 时不会生效。

清单只能覆盖 GitHub `/models` 已返回的同 ID 模型。完整目录使用严格 schema 解析，其中
协议格式枚举、context window、最大输出 token、reasoning levels，以及 adaptive thinking、
temperature、eager tool input 和 xhigh 等已实现能力会应用到本地路由。请求 headers 仅允许
`User-Agent`、`Editor-Version`、`Editor-Plugin-Version` 和 `Copilot-Integration-Id` 四个客户端
身份字段；未知 header、换行值或 `Authorization`、`Host` 会使整份清单被拒绝。`baseUrl`、
provider、cost 等目录元数据不会控制请求；协议格式只能映射到本地固定的
`/chat/completions`、`/responses` 或 `/v1/messages`，不能提供 URL、凭据、OAuth 配置或原始
request body。
GitHub `/models` 明确报告的 capability 仍是事实来源；例如远端
`force_adaptive_thinking: false` 只取消本地强制规则，不会否定 GitHub 已报告的
`adaptive_thinking: true`。

该清单目前与项目源码和发布流程共享 GitHub 仓库信任边界，尚未使用独立数字签名。默认
启用远端检查；对 main 分支远端更新策略不满意的部署可显式关闭，仅使用随二进制发布的
内置规则。

## 诊断日志

插件通过宿主的 `host.log` 写结构化日志。CLIProxyAPI 配置 `debug: true` 后，
`main.log` 会包含完整的 debug 事件；info/warn 级别的关键状态在默认日志级别也会
保留。常用事件包括：

- `auth.parsed`：凭据是否启用、session 是否过期、刷新是否到期以及缓存模型数
- `auth.refresh.started/completed/failed`：刷新阶段、稳定错误码和 HTTP 状态
- `models.discovery.*` / `models.resolved`：发现结果、模型 ID，以及是否回退旧缓存
- `inference.*`：模型、协议路由、转换路径、上游状态和流关闭结果

有请求上下文时，宿主会自动附加 `request_id`。插件日志不会包含 `RawJSON`、
`StorageJSON`、请求/响应正文、Authorization、GitHub access token、Copilot
session token、device code 或 user code；只记录时间、布尔状态、字节数、模型/
格式以及稳定错误分类。

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

模型列表来自当前账号的 Copilot `/models`，默认过滤掉 picker 未启用、policy 禁用
或明确不支持 tool calls 的项目。若 Individual endpoint 的 picker 结果为空，则回退
到 `policy.state == "enabled"` 且支持 tool calls 的项目；Business 和 Enterprise
仍使用严格 picker 语义：

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

同一个模型也可从 Responses 入口调用；`grok-4.5`、GPT-5、OSWE 和 MAI 模型走
Responses 上游，Claude 模型可从 Messages 入口原生调用。
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
