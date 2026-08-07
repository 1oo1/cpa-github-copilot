# CLIProxyAPI GitHub Copilot Plugin

Go `c-shared` 插件，将 GitHub Copilot 订阅作为 CLIProxyAPI v7 的账户级 OAuth
provider `github-copilot` 接入；插件配置 ID 为 `github-copilot-go`。

| 客户端入口 | 路径 |
|---|---|
| OpenAI Chat Completions | `/v1/chat/completions` |
| OpenAI Responses | `/v1/responses` |
| Anthropic Messages | `/v1/messages` |

插件按账号 `/models.supported_endpoints` 选择真实上游协议；入口与上游不一致时，
使用 CLIProxyAPI translator 转换请求、非流式响应和 SSE 流。

> Copilot token broker 和模型接口不是面向通用第三方客户端的稳定公共 API，可能
> 随 GitHub 服务变化。使用前请确认符合订阅条款和组织策略。

## 工作方式

```text
GitHub Device Flow
  -> GitHub access token（长期）
  -> Copilot token broker
  -> Copilot session token（短期，到期前 10 分钟刷新）
  -> 账号 /models
  -> /chat/completions | /responses | /v1/messages
```

- 同时提供 `auth_provider`、`model_provider` 和 OAuth `executor`。
- 模型目录按 picker、policy、tool calls 和可路由 endpoint 过滤；Individual 账号在
  picker 结果为空时回退到 enabled policy。
- session 刷新成功但 `/models` 暂时失败时，保留新 session 和旧模型状态。
- 所有 GitHub/Copilot HTTP 与流请求都经过 `host.http.*`；token 仅进入
  provider-owned `StorageJSON` 和上游 Authorization。

## 前置条件

- 运行：兼容当前 ABI/translator 的 CLIProxyAPI v7，以及有效的 Copilot 订阅。
- 源码构建：Go 1.26+、CGO 和 C 编译器；构建 Linux 双架构还需对应交叉编译器或 Docker。
- 本仓库与 CLIProxyAPI 源码相邻；`go.mod` 通过 `replace` 引用 `../CLIProxyAPI`。

## 安装

### Plugin Store（推荐）

将本仓库的 [registry.json](registry.json) 加入 CLIProxyAPI Store source：

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

重载后可在 Management Center 安装 `GitHub Copilot`，或调用 Management API：

```bash
curl -X POST \
  -H "Authorization: Bearer <MANAGEMENT_KEY>" \
  "http://127.0.0.1:<PORT>/v0/management/plugin-store/github-copilot-go/install"
```

追加 `?version=0.1.3` 可安装指定版本（版本号不带 `v`）。Store 会把产物写入
`plugins.dir/<goos>/<goarch>/`、记录版本并接管后续更新；既有手工安装也应通过
Store 安装一次，确认版本化文件生效后再删除旧的无版本动态库。

### 源码构建与手工安装

```bash
make test          # 单元与契约测试
make vet           # 静态检查
make build         # Linux amd64 + arm64
make integration   # 本机 c-shared 产物 + 真实 CPA loader
```

`make build` 每次先清空 `bin/`，生成 `bin/linux/{amd64,arm64}/github-copilot-go.so`；
也可使用 `make build-linux-amd64`、`make build-linux-arm64` 或 `make build-native`。
Linux 交叉编译器不可用时自动回退到 Docker；可通过 `LINUX_*_CC`、
`DOCKER_GO_IMAGE` 和 `VERSION` 覆盖默认值。

手工安装时，将动态库放入 `plugins.dir` 或对应平台子目录。文件名决定配置 ID，
默认产物对应 `github-copilot-go`。

## 配置

Store 与手工安装共用 `plugins.configs.github-copilot-go`。除 `enabled`、`priority`
外，可配置：

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `client_id` | `Iv1.b507a08c87ecfe98` | Device Flow public client ID，不是 secret |
| `github_host` | `github.com` | GitHub.com 或管理员信任的 GitHub Enterprise 主机名 |
| `enable_models` | `true` | 登录后 best-effort 启用已知模型 policy |
| `model_cache_ttl_seconds` | `300` | 非空账号模型目录的复用时间；`0` 表示每次发现都刷新 |
| `enable_remote_compatibility` | `true` | 从固定 manifest 应用较新的受限兼容覆盖 |
| `remote_compatibility_cache_ttl_seconds` | `14400` | manifest 成功检查后的缓存时间；`0` 表示每次检查 |
| `max_stream_buffer_bytes` | `4194304` | 未完成 SSE 事件的缓存上限；允许 64 KiB 到 64 MiB |

Enterprise 主机必须是 HTTPS DNS 主机名，不能含用户信息、端口、路径、查询、
fragment 或 IP；同时需要该实例可用的 OAuth public client ID。

### 远端兼容清单

- 固定来源为项目 main 分支的 [compatibility.json](https://raw.githubusercontent.com/1oo1/cpa-github-copilot/main/compatibility.json)；请求不带 GitHub/Copilot 凭据。
- 只覆盖账号 `/models` 已返回的同 ID 模型；协议、token 上限、reasoning 和已实现能力均按严格 schema 校验。
- 仅允许四个客户端身份 header；manifest 不能控制 origin、任意路径、Authorization、OAuth 或 request body。
- 使用 ETag 和 `StorageJSON` 缓存；失败时回退最后有效版本或内置规则，旧于内置版本的远端数据不生效。
- 远端文件与源码共享仓库信任边界且无独立签名；关闭开关即可只使用随二进制发布的规则。

## 登录与调用

启用插件并启动 CLIProxyAPI 后，使用现有 Management API 认证访问：

```http
GET /v0/management/github-copilot-auth-url
```

响应包含预填 `user_code` 的 `url` 和 `state`。完成 GitHub 授权后轮询：

```http
GET /v0/management/get-auth-status?state=<state>
```

Device Flow 支持首次等待、`authorization_pending`、两种 `slow_down`、过期和拒绝；
成功后宿主保存 `github-copilot-<login>.json`。随后使用 CLIProxyAPI 原有鉴权访问：

```http
GET /v1/models
POST /v1/chat/completions
POST /v1/responses
POST /v1/messages
```

Chat Completions 请求示例；模型 ID 应来自当前账号的 `/v1/models`：

```json
{
  "model": "<MODEL_ID>",
  "messages": [{"role": "user", "content": "Explain this function"}],
  "stream": true
}
```

插件按模型能力选择上游并在必要时跨协议转换；客户端 header 不能覆盖 Copilot
Authorization。

## 安全与诊断

- GitHub access token 只访问 broker 和 GitHub account；Copilot session token 只访问
  模型、policy 与推理端点。
- token 仅持久化在 provider-owned `StorageJSON`；凭据文件是明文 JSON，应保护 CPA
  auth 目录及备份。
- 推理限制在凭据解析出的 API origin；调用方和兼容清单都不能重定向 bearer token。
- 插件不记录 `RawJSON`、`StorageJSON`、Authorization、token、device/user code、请求
  或响应正文。
- `debug: true` 时可查看 `auth.*`、`models.*`、`inference.*` 结构化事件及宿主附加的
  `request_id`；默认日志仍保留关键 info/warn 状态。

`auth.parse` 只认顶层 `type: github-copilot`，并兼容 pi 旧字段：`refresh`、`access`、
`expires`、`enterpriseUrl`、`availableModelIds` 会分别迁移到语义化 token、host 和
模型字段；已识别但缺少长期 token 的凭据会被禁用，不会落入其他 parser。

## 开发与文档

涉及 auth、路由或流处理时，提交前运行：

```bash
make test
go test -race ./...
make vet
make integration
```

发布 tag 使用 `vX.Y.Z`；Release workflow 生成 Store 所需双架构资产和 checksum，
[registry.json](registry.json) 无需随版本更新。

模块职责、完整技术路线、安全边界、与 `pi` 的实现映射及后续同步手册见
[PI_GITHUB_COPILOT_COMPARISON.md](PI_GITHUB_COPILOT_COMPARISON.md)。
