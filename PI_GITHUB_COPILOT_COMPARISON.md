# VS Code 对齐基线与 pi 历史边界

<!-- compatibility-baseline
verified_at: 2026-08-07T10:34:19Z
vscode_tag: 1.132.0
vscode_revision: df53daabb18cd157bdb08c7f01c34df936cf12f4
copilot_chat_version: 0.60.0
copilot_api_package: 0.4.3
cliproxyapi_revision: 9e9230a19efc555375416d49577cdc9bcd2cc9a6
-->

> 文件名为兼容旧链接而保留。`pi` 不再是实现或同步基线。

## 事实来源

发生冲突时按以下顺序判断：

1. 本仓库源码与测试：插件实际发送、接受、拒绝和记录的内容；
2. pinned VS Code/Copilot Chat：官方客户端的 HTTP wire、能力和状态语义；
3. pinned `@vscode/copilot-api`：endpoint 映射及最终 identity/API version；
4. 相邻 CLIProxyAPI：ABI、host callback 和 translator contract；
5. 当前账户的 Copilot `/models`：运行时模型、endpoint 与 capability；
6. `pi`：仅作 legacy 凭据迁移和非权威交叉检查。

不能用低优先级静态目录覆盖账户 `/models`，也不能用 caller header 覆盖
Authorization、API origin 或 pinned identity。上游依据与源码锚点见
[VSCODE_COPILOT_1_132_ARCHITECTURE.md](VSCODE_COPILOT_1_132_ARCHITECTURE.md)。

## 产品边界

VS Code 是持有会话状态的完整客户端；本项目是 stateless provider plugin：

| 责任 | 所有者 |
|---|---|
| GitHub Device Flow、Copilot token exchange、账户模型与 provider wire | 本插件 |
| 公共 API、凭据文件、刷新调度、transport、translator | CLIProxyAPI |
| conversation history、Responses marker/compaction 选择与下一轮重放 | 调用客户端 |
| VS Code UI、telemetry、agent loop、WebSocket 和 runtime identity | 不移植 |

插件只保证原生 Responses opaque state 无损；无法证明可逆的跨格式 continuation 必须
fail closed。所有上游流量必须经过 `hostClient`，且不能向日志或错误暴露 token、
header、请求/响应正文或 encrypted continuation。

## pi legacy 范围

`auth.parse` 仍识别旧凭据字段 `refresh`、`access`、`expires`、
`enterpriseUrl` 和 `availableModelIds`。迁移后的值仍须通过当前 host、expiry、
token 和 endpoint 校验。

不再从 `pi` 同步模型目录、identity、payload builder、SSE parser、UI、文件锁或
provider composition。若 `pi` 暴露新的 Copilot 行为，必须先在当前 VS Code/CAPI
源码或 GitHub provider 响应中找到独立证据。

## 更新基线

升级 VS Code/Copilot Chat 时：

1. 固定 VS Code tag/commit、Copilot Chat version、CAPI lockfile version、
   CLIProxyAPI revision 和插件 revision；不要以浮动分支或 dirty upstream 下结论。
2. 优先检查 model metadata/endpoint、公共 networking、Responses、Messages、CAPI
   header mixin 及相邻 upstream tests。
3. 先增加能区分旧/新行为的 contract test，再修改行为所属的 route、header、
   normalizer 或 terminal validator。
4. 对 opaque state、terminal、Authorization、endpoint 或 streaming 变更补充
   adversarial 与 integration coverage。
5. 更新本文的 baseline block 和上游架构文档；本插件的当前行为以源码与测试为准，
   不维护完成清单或验收快照。

最低验证 gate：

```bash
make test
make vet
go test -race ./...
make integration
test -z "$(gofmt -d src/*.go)"
git diff --check
```
