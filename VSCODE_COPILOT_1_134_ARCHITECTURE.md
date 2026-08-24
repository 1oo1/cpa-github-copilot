# VS Code 1.134.0 GitHub Copilot HTTP 基线

<!-- upstream-baseline
verified_at: 2026-08-24T10:18:30Z
repository: https://github.com/microsoft/vscode
tag: 1.134.0
revision: 110a328ea54b42367b803ec53ee0bf52ef26b419
extension: extensions/copilot
package_version: 0.62.0
copilot_api_package: 0.5.2
-->

本文只记录本仓库无法直接给出的 pinned 上游依据：模型 API 选择、最终 HTTP identity、
三协议 wire 规则及客户端状态所有权。BYOK、UI、telemetry 和 agent 工具实现不在范围内。

## 源码锚点

| 关注点 | VS Code 1.134.0 源码 |
|---|---|
| 模型 metadata 与 endpoint | [modelMetadataFetcher.ts]、[chatEndpoint.ts] |
| 公共请求与 transport | [chatMLFetcher.ts]、[networking.ts] |
| CAPI header/endpoint mixin | [capiClient.ts]、lockfile 中的 `@vscode/copilot-api@0.5.2` |
| Responses body、state、compaction、events | [responsesApi.ts]、[responsesApi.spec.ts] |
| Messages body、thinking、cache、events | [messagesApi.ts]、[anthropic.ts]、[messagesApi.spec.ts] |

这些链接固定到上方 revision，避免浮动分支改变既有结论。

## 模型与 API 选择

`ChatEndpoint` 从 Copilot `/models` 获取 ID/family、prompt/output limits、tools、
vision、streaming、reasoning、thinking、context editing 和 `supported_endpoints`。
选择顺序是 Responses、实验允许的 Messages、Chat Completions；CAPI SDK 再把请求类型
映射到 `/responses`、`/v1/messages` 或 `/chat/completions`。

能力来自账户 catalog，不能用模型名称或外部静态目录补齐。endpoint 内部扩展字段也不能
误写成 `/models` 公共 schema 的保证。

## 最终 HTTP identity

请求头先由 Copilot Chat networking 生成 request/interaction metadata，再由 CAPI SDK
追加或覆盖 editor identity 与最终 API version。HTTP 出口可固定证明的值是：

```http
User-Agent: GitHubCopilotChat/0.62.0
Editor-Version: vscode/1.134.0
Editor-Plugin-Version: copilot-chat/0.62.0
X-GitHub-Api-Version: 2026-08-01
```

`Copilot-Integration-Id` 的 production 值通常为 `vscode-chat`。session/machine/device
ID、fetcher library version 和部分 build identity 依赖正式构建或运行环境，代理不能伪造。
Responses WebSocket 使用另一 API version；本项目只对齐 HTTP。

Copilot token broker 是独立版本面：GitHub token 请求使用
`Authorization: token ...` 和 `X-GitHub-Api-Version: 2025-04-01`。`/models` 与
`/models/{id}/policy` 则经 CAPI header mixin 使用上述 `2026-08-01`。

动态字段包括 `X-Request-Id`、`X-Agent-Task-Id`、`X-Interaction-Id`、
`X-Interaction-Type`、`OpenAI-Intent` 和 `X-Initiator`。其中 initiator 来自 VS Code
调用上下文，并非上游通过最后一条 message role 推断。

## 三协议 wire 契约

### Chat Completions

- body 基于 `messages` 和 `model`，其余字段取决于调用 fixture 与模型 capability；
- unsupported tools/streaming 会被关闭；
- reasoning effort 仅在模型声明的范围内发送；
- family-specific 转换存在，因此不能维护一份脱离 capability 的固定 golden body；
- stream 与 non-stream 分别由 CAPI SSE/JSON processor 处理。

### Responses

基础请求始终包含 `model`、`input`、`stream:true`、`store:false` 和 encrypted
reasoning include；truncation、tools、reasoning、verbosity、cache 与 context management
由配置和 capability 决定。

0.62.0 中 Responses context management 的配置默认为关闭；显式启用后，compaction
阈值仍为 prompt window 的 90%，window 无效时回退 50000。Grok route 不生成
`reasoning.summary` 或 `text.verbosity`，常规工具被构造为 function tool，仅
tool-search 能力走专用 `tool_search` 形状。

完成响应的 `response.id` 可成为下一轮 `previous_response_id`，此时 input 只包含
marker 之后的增量历史。mode、summary 或连接状态变化可能使 marker 失效并触发完整重放。

已知排除 family 即使在开关启用时也不注入 compaction。客户端负责从 output item
或 terminal output 选择最新 compaction item、
切分 history、去重并在下一轮重放。provider endpoint 只运输 opaque state。

processor 将 `response.completed`、`response.incomplete`、`response.failed` 和顶层
`error` 视为不同终态，同时处理 text/reasoning/tool/compaction deltas。普通 EOF 不是
Responses 成功的源码证据。

### Anthropic Messages

body 中 `system`、tools、`max_tokens`、thinking、effort、cache 和 context management
均由输入、capability 与配置共同决定。tool schema/ID、assistant thinking/signature、
trailing assistant 和 cache breakpoint 还会被规范化。

pinned 源码可生成四个 beta：

- `interleaved-thinking-2025-05-14`;
- `advanced-tool-use-2025-11-20`;
- `context-management-2025-06-27`;
- `extended-cache-ttl-2025-04-11`.

各 beta 的上游 gate 不相同，应以 [chatEndpoint.ts] 与 [messagesApi.ts] 的实现和测试为准，
不能把任意 caller beta 视为 VS Code 行为。

## 可移植边界

代理可对齐模型能力、HTTP header/body、opaque Responses state、SSE terminal/tool/usage
语义及 Anthropic body/header 联动。以下职责属于完整客户端，不能从 wire 层推导：

- conversation/history store 与 latest compaction selection；
- Prompt rendering、agent tool loop、UI 和 telemetry；
- WebSocket connection lifecycle；
- runtime machine/session/device identity。

本插件的采用范围、安全收紧和 legacy 边界见
[PI_GITHUB_COPILOT_COMPARISON.md](PI_GITHUB_COPILOT_COMPARISON.md)；当前可执行行为始终以
[src](src/) 及其测试为准。

[modelMetadataFetcher.ts]: https://github.com/microsoft/vscode/blob/110a328ea54b42367b803ec53ee0bf52ef26b419/extensions/copilot/src/platform/endpoint/node/modelMetadataFetcher.ts
[chatEndpoint.ts]: https://github.com/microsoft/vscode/blob/110a328ea54b42367b803ec53ee0bf52ef26b419/extensions/copilot/src/platform/endpoint/node/chatEndpoint.ts
[chatMLFetcher.ts]: https://github.com/microsoft/vscode/blob/110a328ea54b42367b803ec53ee0bf52ef26b419/extensions/copilot/src/extension/prompt/node/chatMLFetcher.ts
[networking.ts]: https://github.com/microsoft/vscode/blob/110a328ea54b42367b803ec53ee0bf52ef26b419/extensions/copilot/src/platform/networking/common/networking.ts
[capiClient.ts]: https://github.com/microsoft/vscode/blob/110a328ea54b42367b803ec53ee0bf52ef26b419/extensions/copilot/src/platform/endpoint/common/capiClient.ts
[responsesApi.ts]: https://github.com/microsoft/vscode/blob/110a328ea54b42367b803ec53ee0bf52ef26b419/extensions/copilot/src/platform/endpoint/node/responsesApi.ts
[responsesApi.spec.ts]: https://github.com/microsoft/vscode/blob/110a328ea54b42367b803ec53ee0bf52ef26b419/extensions/copilot/src/platform/endpoint/node/test/responsesApi.spec.ts
[messagesApi.ts]: https://github.com/microsoft/vscode/blob/110a328ea54b42367b803ec53ee0bf52ef26b419/extensions/copilot/src/platform/endpoint/node/messagesApi.ts
[anthropic.ts]: https://github.com/microsoft/vscode/blob/110a328ea54b42367b803ec53ee0bf52ef26b419/extensions/copilot/src/platform/networking/common/anthropic.ts
[messagesApi.spec.ts]: https://github.com/microsoft/vscode/blob/110a328ea54b42367b803ec53ee0bf52ef26b419/extensions/copilot/src/platform/endpoint/test/node/messagesApi.spec.ts
