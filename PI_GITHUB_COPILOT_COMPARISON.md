# VS Code 1.132.0 对齐基线与 pi 历史参考

<!-- comparison-baseline
verified_at: 2026-08-07T10:34:19Z
plugin_revision: c4b9392f4b9e9cd96180ff68abf39f1d0733eb6c
cliproxyapi_revision: 9e9230a19efc555375416d49577cdc9bcd2cc9a6
vscode_tag: 1.132.0
vscode_revision: df53daabb18cd157bdb08c7f01c34df936cf12f4
copilot_chat_version: 0.60.0
copilot_api_package: 0.4.3
review: GPT-5.6 Terra final APPROVE
-->

> 文件名为兼容旧链接而保留。`pi` 已不再是本插件的实现参考或同步基线。

## 1. 结论

当前插件的 GitHub Copilot HTTP 行为以 VS Code `1.132.0` 内置 Copilot Chat `0.60.0` 和 `@vscode/copilot-api@0.4.3` 为上游事实来源。相邻 CLIProxyAPI revision 固定 ABI、host callback 和 translator 行为。本仓库源码与测试决定最终可执行结果。

`pi` 只剩两个历史角色：

1. `auth.parse` 读取旧凭据文件时识别 `refresh`、`access`、`expires`、`enterpriseUrl`、`availableModelIds`；
2. 作为非权威交叉参考，帮助发现可能需要回到 VS Code/CAPI 或 GitHub provider 证据重新核对的协议变化。

`pi` 的模型目录、静态身份、payload builder、SSE parser、UI、文件锁和 provider composition 都不再直接驱动本插件实现。`pi` 更新本身不构成同步理由。

## 2. 事实来源优先级

| 优先级 | 来源 | 决定的事实 |
|---:|---|---|
| 1 | 本仓库当前源码与 tests | 插件实际发送、接受、拒绝和记录什么 |
| 2 | VS Code `1.132.0` `extensions/copilot` | 官方客户端的模型能力、HTTP body、history/state 和 terminal 语义 |
| 3 | `@vscode/copilot-api@0.4.3` | CAPI endpoint 映射、最终身份 header 和 API version 覆盖 |
| 4 | CLIProxyAPI `9e9230a...` | ABI v1、host callback、public executor adapter 和 translator contract |
| 5 | GitHub Copilot 运行时 `/models` | 当前账户可见模型、endpoint 和 capability |
| 6 | 本仓库受限 `compatibility.json` | 已发现模型的白名单补充，不能扩大 origin 或 identity trust |
| 7 | `pi` | legacy credential migration 与非权威交叉检查 |

发现冲突时，不以较低优先级覆盖较高优先级。尤其不能用 `pi` 静态目录覆盖账户 `/models.supported_endpoints`，也不能用 compatibility manifest 或 caller header 覆盖 pinned identity、Authorization 或 API origin。

## 3. 固定基线

| 组件 | 固定值 |
|---|---|
| VS Code | tag `1.132.0`, revision `df53daabb18cd157bdb08c7f01c34df936cf12f4` |
| Copilot Chat extension | `0.60.0` |
| CAPI SDK | `@vscode/copilot-api@0.4.3` |
| CLIProxyAPI | revision `9e9230a19efc555375416d49577cdc9bcd2cc9a6` |
| HTTP API version | `2026-06-01` |
| 插件 ABI | CLIProxyAPI plugin ABI v1 |

上游源码分析见 [VSCODE_COPILOT_1_132_ARCHITECTURE.md](VSCODE_COPILOT_1_132_ARCHITECTURE.md)；当前运行时架构见 [CURRENT_PLUGIN_ARCHITECTURE.md](CURRENT_PLUGIN_ARCHITECTURE.md)。

## 4. 产品边界映射

VS Code 是完整聊天客户端，本项目是 stateless provider plugin。不能逐文件移植，只能映射可移植责任。

| VS Code 责任 | 插件或宿主对应 |
|---|---|
| Copilot account token 与 `/models` | 插件 `auth.go`、`models.go` |
| endpoint/model capability | 插件 `models.go` route snapshot |
| Chat/Responses/Messages body | CLIProxyAPI translator + 插件 `executor.go` normalizer |
| HTTP identity 和 interaction headers | 插件 `headers.go` |
| HTTP fetch | CLIProxyAPI host callback，经插件 `hostClient` |
| SSE protocol processor | 插件 `stream.go` + CLIProxyAPI stream translator |
| conversation history | 调用客户端；插件不保存 |
| stateful marker 与 compaction latest selection | 调用 Responses 客户端；插件只保证 wire 完整性 |
| public OpenAI/Anthropic endpoints | CLIProxyAPI host |
| credential files 和 refresh scheduler | CLIProxyAPI host |
| WebSocket connection manager | 不移植；插件只对齐 HTTP |
| telemetry、UI、agent tool loop | 不移植 |

## 5. 身份与请求上下文

### 5.1 最终 HTTP identity

```http
User-Agent: GitHubCopilotChat/0.60.0
Editor-Version: vscode/1.132.0
Editor-Plugin-Version: copilot-chat/0.60.0
Copilot-Integration-Id: vscode-chat
X-GitHub-Api-Version: 2026-06-01
```

这些值来自 pinned VS Code/CAPI HTTP path，不来自 `pi`。caller 与 compatibility manifest 不能在发送时覆盖它们。

插件不伪造正式 VS Code runtime 才能提供的 `VScode-SessionId`、`VScode-MachineId`、`Editor-Device-Id` 或 fetcher library version。

### 5.2 Correlation 与 intent

- 每次请求生成 UUIDv4，同时用于 `X-Request-Id` 与 `X-Agent-Task-Id`。
- caller `X-Interaction-Id` 只有 UUID-shaped 值才保留，否则回退 request ID。
- `X-Interaction-Type` 只接受 VS Code 1.132 固定词表；缺省按输出协议选择 `responses-proxy`、`messages-proxy` 或 `conversation-other`。
- `Openai-Intent` 与受控 interaction type 相同。
- `X-Initiator` 只接受 `user|agent`；缺省时才按最后一个 message/input role 推断。

这是代理环境对 VS Code turn context 的明确适配，不假装拥有 VS Code UI location 或完整 agent lifecycle。

## 6. 模型与路由

主数据流：

```text
Copilot GET /models
  -> picker / policy / tool-call / endpoint filter
  -> StorageJSON model snapshot
  -> restricted compatibility overlay
  -> ModelInfo + modelRoute
  -> request normalizer and headers
```

route 保存 family、prompt/output limits、vision、reasoning levels、streaming、tool search 和 context editing。`supported_endpoints` 优先于模型名称推断；只有服务未声明 endpoint 时才用 legacy ID fallback。optional capability 缺失时关闭对应行为。

`knownCopilotModels` 只用于 best-effort policy enable，不是模型暴露白名单。新模型能否路由取决于账户 `/models` 和安全 endpoint 规则，不取决于 `pi` catalog。

## 7. 三协议 wire 对齐

### 7.1 `/chat/completions`

| 契约 | 当前实现 |
|---|---|
| model/stream | 按 route 和请求固定 |
| `store` | 删除 |
| `reasoning_effort` | 只有 route 声明对应 level 时保留 |
| developer role | 转为 system 兼容 Copilot |
| vision | route capability 与 payload image 同时成立才发送 header |
| stream capability | 必须由账户模型显式声明 true |

### 7.2 `/responses`

| 契约 | 当前实现 |
|---|---|
| `store` | 固定 false |
| `truncation` | caller 缺省时 `disabled` |
| encrypted reasoning include | caller 缺省时加入 |
| reasoning | 清理空/none/off；GPT-5 minimal 归一为 low |
| context management | eligible 原生 route 默认 feature-on，可配置关闭 |
| opaque state | 原生无损；跨格式 fail closed |
| separate compact route | 明确不支持 |

### 7.3 `/v1/messages`

| 契约 | 当前实现 |
|---|---|
| `max_tokens` | 缺省 4096 |
| thinking/effort | adaptive 或 budget route 分别归一 |
| tool schema/ID | object schema、合法字段和 call/result 配对兼容 |
| cache/context editing | capability 与 body 联合 gate |
| eager tool input | route compatibility 控制 |
| beta | 只生成 pinned 四种，不透传 caller 任意值 |

与 pinned VS Code 源码一致的已知例外：非 adaptive Messages route 总是发送 interleaved-thinking beta，即使当前 body 未明确开启 thinking。源码已有对应 TODO，本插件不擅自改变。

## 8. Responses compaction 责任

### 8.1 插件负责

- eligible native route 缺省加入 compaction context management；
- threshold 为 $\left\lfloor0.9\times\text{MaxPromptTokens}\right\rfloor$，无有效 window 时回退 50000；
- 保留 caller 已给出的合法 context management；
- 原样发送和接收 compaction id、encrypted content、output index 和顺序；
- 保留 `previous_response_id`；
- 拒绝任何会经过跨格式 translator 的 opaque continuation；
- 不把独立 `/responses/compact` 猜测成普通 inference。

### 8.2 客户端负责

- 收集 output item 与 terminal output 中的 compaction item；
- 只选择最新 item；
- history slicing 与 marker 生命周期；
- 下一轮把最新 compaction item 放在增量 input 前；
- 去重与持久化。

测试中的最小 Responses 客户端已经证明两轮 round-trip，但生产插件不包含该状态机。这是 stateless 代理的边界，不是功能缺口。

## 9. Terminal authenticity

### 9.1 Stream

Responses 输出必须出现 `response.completed`、`response.incomplete`、`response.failed` 或 `error`。`[DONE]` 和普通 EOF 不是授权终态。跨格式 source priority 为：

$$
\text{failed/error} > \text{incomplete} > \text{completed}
$$

late error 覆盖 earlier finish；Chat `length`、`content_filter` 和 Claude truncation 被保留为 authoritative incomplete；translator 不能凭自身输出授权 completed。

### 9.2 Non-stream

native Responses 只接受三类精确结构：bare Response；event type 与 nested Response 一致且外层无 object/status/error 的 `response.completed`、`response.incomplete`、`response.failed` wrapper；以及带非空 error object、无 response/object/status 冲突字段的 typed 或 untyped standalone error。completed、incomplete、failed 的 error/details 条件必须匹配；未知、大小写/空白变体、null/non-string discriminant、矛盾外层字段、`in_progress`、malformed 或 trailing JSON 都失败。

在 map 解码前，token scanner 递归拒绝每个 object 的 duplicate key，包括 Unicode escape 后等价 key。这避免 Go `encoding/json` last-key-wins 后 validator 看见 success、而原始 bytes 仍含非终态或 secret error 的歧义。

合法 native payload 按原字节返回。normal `Execute` 和 canonical raw `POST /responses` `stream:false` 使用同一 validator；raw `stream:true` 是 SSE body，不应用非流 JSON 检查。

## 10. 安全差异

本插件相对于完整客户端承担 provider token 代理责任，因此保留更严格边界：

- token `proxy-ep` 必须满足 trusted suffix；
- raw HTTP 必须 HTTPS same-origin；
- inference method/path 必须 canonical，不能带 query 或 fragment；
- caller 不能注入 Authorization、API key、identity、API version 或任意 beta；
- compatibility manifest 不能改变 origin、path、Authorization 或最终 identity；
- 日志和错误不能包含 token、headers、request/response body、prompt、tool argument 或 encrypted continuation；
- source error body 在翻译前分类，但不向下游或日志展开。

这些边界优先于与任何客户端实现做逐字段“兼容”。

## 11. pi legacy 兼容范围

旧凭据映射：

| 历史字段 | 当前语义字段或行为 |
|---|---|
| `refresh` | GitHub access token |
| `access` | Copilot session token |
| `expires` | refresh timing migration |
| `enterpriseUrl` | 规范化并重新校验的 GitHub host |
| `availableModelIds` | legacy model snapshot 输入 |

迁移只发生在 auth parser。旧值仍必须通过当前 host、expiry、token 和 endpoint 校验；legacy 字段不会降低 SSRF、日志或模型路由约束。

不再从 `pi` 同步：

- client/editor identity version；
- 模型 API 选择；
- 请求 body 和 beta；
- stream terminal parser；
- remote model catalog trust；
- OAuth 文件锁、UI、runtime snapshot 或 provider composition。

若 `pi` 出现值得关注的 Copilot 变化，应先在当前 VS Code/CAPI 或 GitHub provider 响应中找到独立证据，再按第 12 节流程评估。

## 12. 更新 VS Code 基线的流程

### 12.1 固定 revision

记录新的：

```text
VS Code tag and commit
extensions/copilot package version
@vscode/copilot-api lockfile version
CLIProxyAPI revision
plugin revision and dirty files
```

不要用浮动 `main` 或 dirty upstream worktree 生成正式结论。

### 12.2 优先复核的 VS Code 区域

| 上游区域 | 本地影响 |
|---|---|
| model metadata / `chatEndpoint.ts` | `models.go` capability 和 endpoint route |
| `chatMLFetcher.ts` / networking | `headers.go` identity、intent、correlation |
| `responsesApi.ts` | Responses defaults、state、compaction、terminal |
| `messagesApi.ts` / anthropic networking | Messages body、thinking、cache、beta |
| CAPI client `_mixinHeaders` | 最终 API version 和 identity override order |
| upstream tests | 本地 contract fixture 与边界条件 |

### 12.3 本地修改顺序

1. 先写能区分旧/新行为的 failing contract test。
2. 只修改拥有该行为的 route、header、normalizer 或 terminal abstraction。
3. 第一轮运行最窄测试；通过后再跑协议组和全量 gate。
4. 对 opaque state、terminal、Authorization 或 endpoint 变更扩大 adversarial 与 integration coverage。
5. 更新本文、上游架构、当前架构、实施记录和 README 的同一事实。

### 12.4 验证 gate

```bash
make test
make vet
go test -race ./...
make integration
test -z "$(gofmt -d src/*.go)"
git diff --check
```

影响 ABI、auth、route、stream 或 raw HTTP 时，`make integration` 不能省略。

## 13. 当前验收状态

2026-08-07 快照：

- VS Code test adapter 与 uncached `go test ./...` 全部通过；
- vet、race、gofmt、diff check 和真实 c-shared integration 通过；
- integration 实际调用注册后的 `Execute`、`ExecuteStream` 和 `HttpRequest`；
- raw duplicate status/error bypass 被拒绝且 secret sentinel 不泄漏；
- GPT-5.6 Terra final review：`APPROVE`，无 Critical/High/Medium finding。

## 14. 文档分工

| 文档 | 维护内容 |
|---|---|
| [README.md](README.md) | 安装、配置、登录、调用和用户可见限制 |
| [CURRENT_PLUGIN_ARCHITECTURE.md](CURRENT_PLUGIN_ARCHITECTURE.md) | 当前可执行架构、security、state 和 terminal contract |
| [VSCODE_COPILOT_1_132_ARCHITECTURE.md](VSCODE_COPILOT_1_132_ARCHITECTURE.md) | pinned upstream 源码事实 |
| [VSCODE_COPILOT_ALIGNMENT_PLAN.md](VSCODE_COPILOT_ALIGNMENT_PLAN.md) | 已完成决策、测试矩阵和验收证据 |
| 本文 | 事实优先级、上游到插件映射、legacy pi 范围和未来升级流程 |