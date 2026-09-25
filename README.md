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

面向公网的内置防护：

- 同一来源 10 分钟内连续 10 次错误的管理密钥或 API Key，会被暂停该类认证 15 分钟（`429` + `Retry-After`），期间即使猜中也拒绝；此前已成功认证的密钥不受影响，不带密钥的请求不计数。经反向代理访问时，须在 `trusted-proxies` 中填写代理地址，否则所有客户端共享代理的地址。
- 推理请求体上限 16 MiB（`413`）；并发处理的请求体总量按进程内存上限自动限制，排队超过 30 秒返回 `503`。容器内自动读取 cgroup 内存限制并将 Go 堆软上限设为其 80%，显式设置 `GOMEMLIMIT` 时以其为准。
- 请求头 10 秒、完整请求 5 分钟、空闲连接 2 分钟超时；下游停止读取响应超过 2 分钟即断开，释放上游流。
- 管理面板带严格的 Content-Security-Policy，只允许内嵌脚本运行，禁止被嵌入框架。

客户端用配置中的 API Key：

```bash
curl http://127.0.0.1:8317/v1/models \
  -H "Authorization: Bearer $COPILOT_PROXY_API_KEY"
```

Messages 入口也支持 `x-api-key`。管理 API 仅接受独立的管理 Bearer key，不使用 Cookie。管理密钥不会持久化在浏览器；新建的 API Key 只在当前页面显示，列表只显示掩码。

## Docker Compose 部署

仓库提供以下可直接使用的部署文件：

- `compose.yaml`：详细注释的服务、端口、持久化、非 root 用户、健康检查及资源限制。
- `config.example.yaml`：**全部应用 YAML 参数**、默认值/取值范围及行为说明。
- `.env.example`：全部 Compose 插值变量，不包含密钥。
- `Dockerfile`：多阶段构建，运行镜像只包含独立程序、CA 证书、时区及健康检查所需工具，不包含 Go/Bun 或凭据。

### 首次部署

需要 Docker Engine/Desktop 和 Docker Compose v2+。在放置上述三个示例文件的目录操作；推荐使用普通宿主用户，避免 root 运行容器：

```bash
cp .env.example .env
# 编辑 .env：把 COPILOT_UID / COPILOT_GID 改为以下命令的结果，并按需固定镜像版本。
id -u
id -g

# 默认 ./data 由当前用户创建。若使用不同 UID/GID 或数据目录，需相应调整所有权。
mkdir -p data
chmod 700 data
cp config.example.yaml data/config.yaml
chmod 600 data/config.yaml

# 执行两次，各生成一个独立随机密钥；分别填入 config.yaml 的
# management-key 和 api-keys，不要把终端输出发到日志、截图或版本库中。
openssl rand -hex 32
openssl rand -hex 32

# 确认两个 REPLACE_* 占位符已替换，并保留容器内 listen: "0.0.0.0:8317"。
docker compose config --quiet
docker compose pull
docker compose up -d --wait
docker compose ps
```

示例占位符故意不足 16 字符，**原样启动会拒绝运行**。程序不会展开应用 YAML 中的 `${ENV_VAR}`；只有 Compose 会读取 `.env`。如需固定使用镜像的默认 UID/GID `10001:10001`，管理员应将整个数据目录移交给它，例如 `sudo chown -R 10001:10001 data`，并在 `.env` 保持相同值。已有部署切换 UID 时也需调整配置、认证及状态文件所有权；不要使用 `chmod 777`。

默认访问 `http://127.0.0.1:8317/`，使用私有配置文件中的管理密钥登录。宿主端口可用 `.env` 的 `COPILOT_PORT` 调整；容器内端口仍为 8317。生产外部访问应经 HTTPS 反向代理，代理需关闭 SSE 响应缓冲并允许长时间连接。`/healthz` 仅检查本地 HTTP 服务，不代表 GitHub 可用或有可用账号；Compose 不会仅因 `unhealthy` 自动重启。

**必须挂载整个可写的 `/data`，不要单独挂载 `config.yaml`。** 面板操作会原子替换配置文件，凭据刷新及预算锁也需要写权限。`read_only: true` 仅限制镜像根文件系统，`/data` 绑定挂载仍可写。请备份整个数据目录，包括 `config.yaml`、`auths/`、`state/` 和 `usage.json`；配置经面板重写后注释可能丢失，原始示例不会改变。

容器中的 `127.0.0.1` 不是宿主机。使用宿主机代理时，按 `compose.yaml` 注释启用 `host.docker.internal:host-gateway`，并在应用配置中填写对应 `proxy-url`。应用默认监听只有在 `--init` 自动生成配置时才是回环地址；**容器不能直接沿用未修改监听地址的自动生成配置**。

### 更新、日志、回滚与本地构建

```bash
docker compose logs --tail=100 -f
docker compose pull
docker compose up -d --wait       # 替换容器，保留 ./data
docker compose down             # 停止服务，不删除绑定挂载的数据目录

# 尚未发布镜像，或者希望本地构建当前源码时：
docker build --build-arg VERSION=dev -t copilot-proxy:local .
COPILOT_IMAGE=copilot-proxy:local docker compose up -d --wait
```

本地有可用 Docker 守护进程时，可运行 `bash scripts/smoke-docker.sh copilot-proxy:local`，验证镜像内 ELF 架构，以及实际 Compose 配置的健康检查、内嵌面板、鉴权隔离、配置原子写入及容器重启后持久化。可选第二个参数 `linux/amd64` 或 `linux/arm64` 显式选择运行平台（非本机架构需要 Docker 支持模拟运行）。该脚本使用临时数据和合成密钥，结束后清理测试容器，不访问 GitHub。

生产建议将 `COPILOT_IMAGE` 固定为版本或 digest；回滚时改为之前的兼容版本后重新 `up -d`，**不要删除预算状态来恢复可用性**。镜像代码回滚不代表任意版本间的数据格式兼容，升级前先备份。

导入旧 CPA 目录时先停止新服务，然后把源目录额外挂载为只读；不要覆盖 `/data`：

```bash
docker compose stop copilot-proxy
docker compose run --rm --no-deps \
  -v /absolute/path/to/old-auths:/import:ro \
  copilot-proxy --config /data/config.yaml --import-auth-dir /import
docker compose up -d --wait
```

导入进程仍以配置的 UID/GID 运行，需要对源文件有读取权限。源目录保持不变；只迁移 Copilot 凭据及匹配的预算锁，详见下一节。

### GitHub Actions / GHCR 发布

镜像地址默认是 `ghcr.io/1oo1/cpa-github-copilot`；fork 会自动使用小写的 `ghcr.io/<owner>/<repository>`。工作流会先跑 Go/前端测试、构建原生二进制，再构建 Docker 镜像并运行真实 Compose 冒烟测试（合成密钥、空账号、无 GitHub 推理）。通过后构建 `linux/amd64` 与 `linux/arm64` 镜像。

| 触发方式 | 镜像行为 |
|---|---|
| PR | 构建、Compose 冒烟测试、多架构构建；不登录、不推送 GHCR |
| 推送 `vX.Y.Z` 稳定标签 | 发布 `X.Y.Z`、`X.Y`、`latest` 和 `sha-<完整提交 SHA>` |
| 推送 `vX.Y.Z-rc.1` 等预发布标签 | 发布完整预发布版本及 SHA；不覆盖稳定 `latest`/`X.Y` |
| 默认分支手动运行 | 发布 `edge` 和 SHA；不更新 `latest` |
| 其他分支手动运行 | 仅构建和测试，不推送 |

标签必须符合工作流接受的 `vX.Y.Z[-prerelease]` 格式，不使用 `+build` 元数据。手动选择已有版本标签运行时按版本发布规则执行。工作流使用内置 `GITHUB_TOKEN` 的 `packages: write` 权限，不需要在仓库配置个人 PAT；仓库/组织策略仍须允许写入包。

首次成功推送后，在 GitHub 的 **Packages → 对应容器包 → Package settings** 检查可见性和 Actions 访问权限。希望匿名 `docker compose pull` 时需将包设为 public；容器包可能默认 private，源码公开不等于镜像自动公开。私有包的部署机器需要用具有 `read:packages` 的凭据通过 `docker login ghcr.io --password-stdin` 登录；凭据不要写进 Compose 或提交到仓库。工作流不会自动修改包可见性。

OCI source/version/revision 标签、SBOM 和最小构建 provenance 随发布生成。成功发布后的工作流摘要给出实际镜像标签与 digest。修改工作流本身不会立即创建镜像；首次部署前需要先成功运行一次发布，或使用本地构建。

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
| `trusted-proxies` | 可信反向代理 IP/CIDR；仅来自这些地址的 `X-Forwarded-For` 用于认证失败限流。部署场景的详细填法见 [TRUSTED_PROXIES.md](TRUSTED_PROXIES.md) |
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
- 用量统计是本机观察到的 tokens，不用于推算 GitHub Credits。累计统计每秒最多落盘一次并在停止时写入，最近 500 条结构化日志仅保留在内存中。

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
