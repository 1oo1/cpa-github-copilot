# Copilot Proxy

只支持 **GitHub Copilot** 的独立 API 代理。原 Copilot 插件、必要的协议转换与账号管理能力、专用管理面板现在位于同一个仓库，发布为**一个无需 CGO 的可执行文件**。

运行时不需要 CLIProxyAPI、动态插件、Node.js、Bun 或独立前端服务器。配置、凭据和预算状态保存在私有数据目录，不能编入二进制。

## 保留与移除

保留 Copilot Device Flow 登录、多账号、优先级与轮询、会话亲和、API Key、代理、模型别名/排除、用量统计、结构化日志、订阅额度和预算保护。

已移除原 C ABI、插件加载/商店/注册、其他 OAuth 登录、其他上游执行器、Home 分布式调度、多后端凭据存储、独立面板下载器。管理 API 是精简后的专用接口，不承诺兼容完整 CPA 管理 API 或配置文件。

以下是**客户端协议**，不是额外的上游提供商：

| 客户端协议 | 路径 |
|---|---|
| OpenAI Chat Completions | `/v1/chat/completions` |
| OpenAI Responses | `/v1/responses` |
| Anthropic Messages | `/v1/messages` |

`GET /v1/models` 来自当前 Copilot 账号的模型目录。Copilot 提供的 Claude、GPT、Gemini、Grok 等模型仍可用；保留模型名、协议转换及签名验证不代表开放相应厂商的独立登录或服务。

## 部署

从发布产物选择对应平台的 `copilot-proxy` 二进制：

```bash
./copilot-proxy --init --config ./data/config.yaml
./copilot-proxy --config ./data/config.yaml
```

第一次初始化自动生成管理密钥和 API Key，写入权限为 `0600` 的配置文件；不会打印密钥。默认监听 `127.0.0.1:8317`，打开 `http://127.0.0.1:8317/`，使用配置文件里的 `management-key` 登录。

部署到服务器时，可以把 `listen` 改为 `0.0.0.0:8317`，但必须通过 HTTPS 反向代理和防火墙保护访问。不要在公共明文 HTTP 上发送管理密钥、API Key 或凭据。升级时替换二进制并保留整个数据目录。

客户端用配置中的 API Key：

```bash
curl http://127.0.0.1:8317/v1/models \
  -H "Authorization: Bearer $COPILOT_PROXY_API_KEY"
```

Messages 入口也支持 `x-api-key`。管理 API 仅接受独立的管理 Bearer key，不使用 Cookie。管理密钥不会持久化在浏览器；新建的 API Key 只在当前页面显示，列表只显示掩码。

## 从旧 CPA 迁移

先初始化一个**新的数据目录**，再复制导入原认证文件：

```bash
./copilot-proxy --init --config ./data/config.yaml
./copilot-proxy --config ./data/config.yaml --import-auth-dir /path/to/old-cpa-auths
```

只接受 `type: github-copilot` 的 JSON；源目录保持不变。也可以在“账号与额度”页面导入单个认证 JSON。旧 `refresh`、`access`、`expires`、`enterpriseUrl`、`availableModelIds` 字段仍由原安全校验与迁移逻辑处理。不要直接覆盖原 CPA 数据目录。

CPA 的配置不能直接沿用。把需要的 API Key、代理、优先级/模型设置迁到新配置；不再存在 `plugins.configs.github-copilot-go`。迁移前应停掉旧服务。目录导入会读取相邻 `.cds` 文件中可验证的 Copilot 预算锁，并转换为新服务状态；其他普通冷却不会迁入。若旧状态与凭据不匹配或损坏，导入会报错而不是静默解除保护。通过网页导入单个 JSON 不包含旧 `.cds` 状态：请先确认额度查询成功，再放开客户端流量。

## 配置

`--init` 生成可运行的完整配置。常用字段：

| 字段 | 说明 |
|---|---|
| `listen` | 默认 `127.0.0.1:8317`，修改后重启 |
| `management-key` | 独立管理密钥，至少 16 字符 |
| `api-keys` | 客户端密钥列表，至少一个，每个至少 16 字符 |
| `auth-dir` | 可选，默认配置文件旁的 `auths/`；相对路径以配置文件目录为基准 |
| `proxy-url` | 全局 HTTP/HTTPS/SOCKS5 代理；账号的 `proxy_url` 优先 |
| `routing-strategy` | `round-robin` 或 `fill-first`；先按账号 `priority` 排序 |
| `session-affinity` | 开启后按客户端 API Key + `X-Session-Id` 绑定账号，30 分钟过期 |
| `model-aliases` | `{公开别名: Copilot 模型 ID}` |
| `excluded-models` | 排除模型的 glob 列表 |
| `copilot` | 下列 Copilot 专用选项 |

`copilot` 保留 `client_id`、`github_host`、`enable_models`、`model_cache_ttl_seconds`、`max_stream_buffer_bytes`、`web_search_model` 和 `enable_responses_context_management`。配置改动除面板明确支持的代理和调度设置外均需重启。GitHub Enterprise 主机必须通过 HTTPS DNS 主机校验，并配置适用的 OAuth public client ID。

## 额度与预算保护

认证卡片显示 Copilot 套餐、Credits、Additional Budget 和浏览器本地时区的重置日期。没有 Additional Budget 时不伪造 `0% used`；旧计费显示 Premium requests。

- Credits 使用 `100 - percent_remaining`，不能用 `credits_used / entitlement` 替代。
- Additional Budget 使用 `overage_count / overage_entitlement`。界面向下取整，保护判断使用精确十进制比例：**99% 不触发，99.01% 触发**。
- 后台独立检查，最多复用 60 秒快照；推理前检查缓存。GitHub 限流时退避，普通查询失败不停止尚未触发的账号。
- 触发后暂停账号所有模型，切换其他可用账号，不改手动禁用状态。
- 保护状态持久化在 `state/` 中。必须到达重置时间并得到新周期的有效、未超阈值额度后才恢复。查询失败、同周期提高预算、凭据刷新或重启均不能解除。
- 没有可信重置时间时不猜测恢复日期；状态损坏时拒绝加载该状态，持久化失败时暂停账号。保护好整个数据目录及备份。

已开始的请求不强行中断；60 秒缓存、并发和上游计量延迟意味着这不是严格的消费上限。

## 协议边界

- 流式和非流式请求均支持；Responses 必须出现真实终态，不会把 EOF 伪造成成功。
- 会话历史、Responses continuation 和 compaction 重放由客户端负责。无法安全转换的 opaque state 仍拒绝跨协议转换。
- 不提供独立 `/v1/responses/compact` 或 token counting。
- Copilot 原生 Web Search 的专用分流和原有请求头、同源、规范路径及 token 校验继续保留。
- 所有外发 HTTP 经共享 transport 和 Copilot `hostClient`；只接受 HTTPS 上游，拒绝自动重定向。不会记录请求正文、授权头或 Copilot token。
- 用量统计是本机观察到的 tokens，不用于推算 GitHub Credits。累计统计持久化，最近 500 条结构化日志仅保留在内存中。

## 构建与验证

需要 Go 1.26+、Bun 1.3.14（仅构建前端时需要）。不需要 C 编译器或相邻仓库。

```bash
make build       # 前端单文件构建 + go:embed + 本机独立二进制
make verify      # Go test/vet/race + 前端测试和构建
make integration # 本进程 HTTP、额度保护、重启恢复与 SSE 集成测试
make release     # Linux amd64/arm64、macOS arm64、Windows amd64
```

所有发布产物在 `bin/`。构建使用 `CGO_ENABLED=0`；`make release VERSION=1.0.0` 设置版本。开发时可分别运行 Go 服务和前端构建；交付时只发布最终可执行文件及校验和。

前端位于 `web/`，Copilot 核心位于 `internal/copilot/`，共享 transport 位于 `internal/transport/`，账号与 HTTP 服务位于 `internal/app/`。`internal/conversion/` 仅为兼容协议转换，不包含其他上游 provider。

## 上游维护

[兼容性基线与升级流程](PI_GITHUB_COPILOT_COMPARISON.md) 记录 VS Code/Copilot Chat 依据。以后不再跟随完整 CPA 的功能升级；仍需维护 HTTP 安全、依赖和客户端协议兼容。

迁入代码的 MIT 许可证见 `LICENSES/`。管理面板已裁剪为本服务专用界面，不再向 CPA 面板发布地址请求更新。
