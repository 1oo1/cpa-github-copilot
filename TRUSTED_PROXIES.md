# trusted-proxies 使用说明

`trusted-proxies` 只影响一件事：**认证失败限流如何识别客户端来源**。它不代理流量、不影响正常请求，只是防暴力破解限流的客户端识别依据。

相关实现：`internal/app/security.go`（`parseTrustedProxies` / `trustedAddr` / `clientKey`）。

## 背景

服务内置防护：同一来源 10 分钟内连续 10 次猜错管理密钥或 API Key，封该来源 15 分钟（`429` + `Retry-After`），期间即使猜中也拒绝；此前认证成功的密钥不受影响，不带密钥的请求不计数。

"来源"默认取 TCP 对端地址（`r.RemoteAddr`）。当服务位于反向代理之后时：

- 所有请求的对端地址都是**代理的地址**，不是真实客户端。
- 不配置 `trusted-proxies` 时，任何攻击者猜错 10 次都会**封掉整个代理地址**——经该代理的所有正常用户一起被封 15 分钟。

## 工作方式

`clientKey` 的判定逻辑：

1. 对端地址**不在** `trusted-proxies` 中 → 直接使用对端地址，完全忽略 `X-Forwarded-For`（防止客户端自行伪造 XFF 绕过封禁）。
2. 对端地址**在** `trusted-proxies` 中 → 从 `X-Forwarded-For` 右端往左逐跳解析，跳过所有可信代理地址，取**第一个不可信的地址**作为真实客户端 IP。
3. IPv6 客户端按 /64 分组（一个 /64 通常属于同一主机）。

因此生效需要两个条件同时满足：

- `trusted-proxies` 填了**代理连到本服务时使用的地址**（即 `RemoteAddr` 里看到的地址）；
- 代理正确传递了 `X-Forwarded-For`。

## 配置格式

YAML 列表，支持单个 IP 或 CIDR，启动时校验，格式错误会直接报错：

```yaml
trusted-proxies:
  - 127.0.0.1        # 单个 IP
  - ::1
  - 10.0.0.0/8       # 网段
  - 172.16.0.0/12
```

## 不同部署方式怎么填

| 部署方式 | 填什么 |
|---|---|
| 客户端直连（`listen` 直接暴露，无反向代理） | **留空/不配置** |
| nginx 与本服务同机，转发到 `127.0.0.1:8317` | `127.0.0.1`、`::1` |
| 本服务在 Docker 中，宿主机 nginx 通过端口映射访问 | **Docker 网桥网关网段**，如 `172.17.0.0/16`（见下节） |
| nginx 与本服务在同一 Compose 网络（连接 `http://copilot-proxy:8317`） | 该 Compose 网络网段，如 `172.18.0.0/16` |
| 公网 LB / CDN 回源 | 回源 IP 段（云厂商文档为准） |

**要填的是代理"出口到本服务"的地址，不是客户端的地址。**

### Docker：为什么填的不是 127.0.0.1

链路为：`客户端 → nginx（宿主机）→ 127.0.0.1:8317 端口映射 → 容器`。

docker 端口映射会对来源做 SNAT，容器看到的 TCP 对端不是 `127.0.0.1`，而是 **Docker 网桥的网关 IP**：

- `docker run -p 127.0.0.1:8317:8317 ...`（默认 bridge 网络）：通常为 `172.17.0.1`，填 `172.17.0.0/16` 即可，整个网段兜住重建容器后的变化。
- Compose 自定义网络：网段通常不同（常见为 `172.18.0.0/16` 起），先确认网关：

```bash
# Compose 网络
docker network inspect <网络名> -f '{{(index .IPAM.Config 0).Gateway}}'

# 或直接看容器默认网关
docker exec <容器名> ip route | awk '/default/{print $3}'
```

把查到的网关 IP（或整个网段）填入 `trusted-proxies`。

安全性：宿主机 nginx 的场景下，该端口映射只绑定 `127.0.0.1`，宿主机以外的客户端连不到这条路径；能从网桥网段过来的只有 nginx 和宿主机本机进程（那已意味着宿主机被控）。因此信任网段不会给外部攻击者伪造 `X-Forwarded-For` 的机会。

## 代理必须转发 X-Forwarded-For

nginx 默认不加该头，需在 `location` 中显式配置：

```nginx
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
```

没有它时，即使配了 `trusted-proxies`，服务也只能看到网关 IP——效果与不配置相同，所有经 nginx 的客户端共享同一个限流计数桶。

## 验证方法

1. 配置改好并重启服务（`trusted-proxies` 属配置文件字段，需重启生效）。
2. 从外部用**错误**的 API Key 请求 10 次 → 应触发 `429` + `Retry-After`。
3. 用**正确的** Key 从另一台机器/另一个 IP 请求：
   - 正常返回 → `trusted-proxies` 生效，只有被封的 IP 受影响；
   - 同样 `429` → 未生效，所有客户端仍共享代理地址（通常是网段填错或 `X-Forwarded-For` 未转发）。

## 常见错误

- **填了客户端地址**：应该填代理侧地址（`RemoteAddr`），不是访客 IP。
- **只配了 trusted-proxies，nginx 没转发 XFF**：两个条件缺一不可。
- **Docker 里照抄 127.0.0.1**：容器看到的是网桥网关，不是回环地址。
- **不填**：直连部署是正确做法；反向代理部署不填则会把所有人绑定到同一个限流桶。
