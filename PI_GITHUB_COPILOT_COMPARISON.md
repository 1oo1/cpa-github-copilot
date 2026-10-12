# VS Code / Copilot Chat 兼容基线

文件名为保留历史链接而沿用。`pi` 不是实现或同步基线，仅保留旧凭据字段迁移。

## 当前依据

- 推理 HTTP wire：VS Code `1.141.0`，Copilot Chat `0.69.0`，`@vscode/copilot-api@0.5.2`，API version `2026-08-01`。
- 套餐及额度：VS Code `1.141.0`，Copilot Chat `0.69.0`，固定 revision `2a59476c9bfcb90b3ddc372c36762471b7dfad1c`。
- 模型列表及能力：当前账户的 Copilot `/models`。不能用其他厂商的静态模型目录覆盖。
- 原 CPA 协议转换源码迁自 `c404af96ebacedf8168b3c2bdbf4449a21cd1c1e`；管理面板中的额度格式化语义迁自 `4530da271ba2e89810d4dccebc57f3091afa590a`。不依赖这些仓库参与构建，也不自动同步它们的完整功能。

额度入口：[账户请求](https://github.com/microsoft/vscode/blob/2a59476c9bfcb90b3ddc372c36762471b7dfad1c/src/vs/workbench/services/accounts/browser/defaultAccount.ts)、[套餐与额度解析](https://github.com/microsoft/vscode/blob/2a59476c9bfcb90b3ddc372c36762471b7dfad1c/src/vs/workbench/services/chat/common/chatEntitlementService.ts)、[官方展示](https://github.com/microsoft/vscode/blob/2a59476c9bfcb90b3ddc372c36762471b7dfad1c/src/vs/workbench/contrib/chat/browser/chatStatus/chatStatusDashboard.ts)。推理依据另见 [HTTP wire 架构基线](VSCODE_COPILOT_1_136_ARCHITECTURE.md)。

## 1.141.0 / 0.69.0 采用范围

- 固定 [VS Code 1.141.0](https://github.com/microsoft/vscode/releases/tag/1.141.0)，
  revision `2a59476c9bfcb90b3ddc372c36762471b7dfad1c`。
  [extension package](https://github.com/microsoft/vscode/blob/2a59476c9bfcb90b3ddc372c36762471b7dfad1c/extensions/copilot/package.json)
  为 `0.69.0`，[lockfile](https://github.com/microsoft/vscode/blob/2a59476c9bfcb90b3ddc372c36762471b7dfad1c/extensions/copilot/package-lock.json)
  仍锁定 `@vscode/copilot-api@0.5.2`。同步 editor/plugin/User-Agent identity，HTTP API
  version 仍为 `2026-08-01`，token broker 仍为 `2025-04-01`。
- 保留响应 `X-Copilot-Service-Request-Id`，并新增保留 Copilot Chat 0.69 读取的
  `X-GitHub-Copilot-Request-Te`；两者仅限响应白名单并按原值返回，不接受调用方伪造上游身份。
- 原生 Responses 已支持不带 `rs` 前缀的 opaque reasoning ID；保持回放和跨协议拒绝回归，
  不移植上游 prompt/history 层内部 `originApi` 字段，也不放宽签名/opaque state 校验。
- 模型能力仍按账户 `/models` metadata 发现及路由，不照搬客户端模型名能力表；本仓库额外执行
  项目兼容门槛：Claude ID 仅接受 5.1 及以上且版本分隔符使用 `-`
  （例如 `claude-fable-5-5`），GPT ID 仅接受 6 及以上。门槛只决定是否暴露/路由模型，
  不凭名字补齐 context、prompt/output limits、tool search、reasoning 或 vision 能力。
- 0.69 将 Responses explicit prompt-cache breakpoint 的客户端默认值改为关闭；代理只透传调用方
  实际构造的 body，不擅自注入 breakpoint。上游新增的 telemetry、WebSocket、agent/history
  管理仍留在客户端。
- 与旧推理基线相比，token exchange、model metadata fetcher、CAPI client、Messages 请求构造、
  Anthropic beta gate 及 Responses compaction 默认值不变。
- 与旧额度基线相比，entitlement 解析与 quota dashboard 数值语义未变；1.141 的
  `defaultAccount.ts`/dashboard 变化主要是 GitHub Enterprise session URI 解析与 URL 打开失败处理，
  不改变预算锁和凭据替换规则。

## 所有权

本服务负责 GitHub Device Flow、Copilot session、账户模型、provider wire、调度、额度和预算保护；客户端负责 conversation history、continuation、compaction 的选择与下一轮重放。VS Code 的 UI、telemetry、agent loop、WebSocket 和 runtime identity 不移植。

所有外发 HTTP 由共享 transport 提供，Copilot 模块仍通过 hostClient 使用它。不能伪造 session/machine/device ID，不能放宽 token、origin、路径、header、终态和 opaque state 校验。

## 升级流程

1. 固定 VS Code 稳定 tag/commit、内置 Copilot Chat 版本及 CAPI lockfile，不直接依赖浮动 main。
2. 用浅克隆取得完整源码，不再通过浏览器逐页读取 raw 文件或只下载解压归档。目标目录位于已忽略的
   `.local/upstream/`，保留 `.git`，便于后续同步时 fetch 新 tag、复用对象并核对 commit：

   ```bash
   git clone --depth 1 --branch <vscode-stable-tag> \
     https://github.com/microsoft/vscode.git .local/upstream/<vscode-stable-tag>
   git -C .local/upstream/<vscode-stable-tag> rev-parse HEAD
   ```

   同一 tag 已存在且为有效 Git checkout 时直接复用；目录缺失 `.git` 或 tag/commit 不符时，
   先检查是否包含本地研究产物，再用新的临时目录完成克隆并替换，不能把无 `.git` 的导出目录当作
   可升级源码。后续同步新版本时，优先对现有 checkout `git fetch --depth 1 origin tag <new-tag>`，
   无法获取时才按上述命令另建版本目录。
3. 对比账户 entitlement、token exchange、模型 metadata、endpoint、networking、Messages 和 Responses 实现及相邻测试。
4. 先增加区分旧/新行为的 fixture，再修改行为所属模块；额度基线升级不等于推理请求头自动升级。
5. 跑 test、vet、race、HTTP/SSE 集成及前端测试；检查私密数据脱敏、取消、EOF、流式错误和凭据替换。
6. 更新实际变更涉及的基线和 README。只发布新的独立二进制，保留用户数据目录。

即使只使用 Copilot，也必须继续处理依赖安全更新和客户端公共 API 的兼容问题。
