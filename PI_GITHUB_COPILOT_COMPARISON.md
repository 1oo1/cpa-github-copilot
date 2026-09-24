# VS Code / Copilot Chat 兼容基线

文件名为保留历史链接而沿用。`pi` 不是实现或同步基线，仅保留旧凭据字段迁移。

## 当前依据

- 推理 HTTP wire：VS Code `1.136.1`，Copilot Chat `0.64.1`，`@vscode/copilot-api@0.5.2`，API version `2026-08-01`。
- 套餐及额度：VS Code `1.139.0`，Copilot Chat `0.67.0`，固定 revision `2242ebbb54efeeb0129e08e919e7e8d43033cd83`。
- 模型列表及能力：当前账户的 Copilot `/models`。不能用其他厂商的静态模型目录覆盖。
- 原 CPA 协议转换源码迁自 `c404af96ebacedf8168b3c2bdbf4449a21cd1c1e`；管理面板中的额度格式化语义迁自 `4530da271ba2e89810d4dccebc57f3091afa590a`。不依赖这些仓库参与构建，也不自动同步它们的完整功能。

额度入口：[账户请求](https://github.com/microsoft/vscode/blob/2242ebbb54efeeb0129e08e919e7e8d43033cd83/src/vs/workbench/services/accounts/browser/defaultAccount.ts)、[套餐与额度解析](https://github.com/microsoft/vscode/blob/2242ebbb54efeeb0129e08e919e7e8d43033cd83/src/vs/workbench/services/chat/common/chatEntitlementService.ts)、[官方展示](https://github.com/microsoft/vscode/blob/2242ebbb54efeeb0129e08e919e7e8d43033cd83/src/vs/workbench/contrib/chat/browser/chatStatus/chatStatusDashboard.ts)。推理依据另见 [HTTP wire 架构基线](VSCODE_COPILOT_1_136_ARCHITECTURE.md)。

## 所有权

本服务负责 GitHub Device Flow、Copilot session、账户模型、provider wire、调度、额度和预算保护；客户端负责 conversation history、continuation、compaction 的选择与下一轮重放。VS Code 的 UI、telemetry、agent loop、WebSocket 和 runtime identity 不移植。

所有外发 HTTP 由共享 transport 提供，Copilot 模块仍通过 hostClient 使用它。不能伪造 session/machine/device ID，不能放宽 token、origin、路径、header、终态和 opaque state 校验。

## 升级流程

1. 固定 VS Code 稳定 tag/commit、内置 Copilot Chat 版本及 CAPI lockfile，不直接依赖浮动 main。
2. 对比账户 entitlement、token exchange、模型 metadata、endpoint、networking、Messages 和 Responses 实现及相邻测试。
3. 先增加区分旧/新行为的 fixture，再修改行为所属模块；额度基线升级不等于推理请求头自动升级。
4. 跑 test、vet、race、HTTP/SSE 集成及前端测试；检查私密数据脱敏、取消、EOF、流式错误和凭据替换。
5. 更新实际变更涉及的基线和 README。只发布新的独立二进制，保留用户数据目录。

即使只使用 Copilot，也必须继续处理依赖安全更新和客户端公共 API 的兼容问题。
