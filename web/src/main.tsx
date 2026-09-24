import React, { useEffect, useState, useRef } from "react";
import { createRoot } from "react-dom/client";
import { buckets, usedPercent, resetLabel, type Quota } from "./quota";
import "./style.css";

type Account = {
  id: string;
  name: string;
  account: string;
  disabled: boolean;
  priority: number;
  models: string[];
  budget_guard: { blocked: boolean; reason?: string; reset_at?: string };
  quota: Quota;
  cooldown_until?: string;
};
type Usage = {
  requests: number;
  errors: number;
  input_tokens: number;
  output_tokens: number;
};
type Event = {
  time: string;
  level: string;
  event: string;
  account?: string;
  model?: string;
  status?: number;
  duration_ms?: number;
  input_tokens?: number;
  output_tokens?: number;
};
type Settings = {
  proxy_redacted?: boolean;
  listen: string;
  proxy_url: string;
  routing: string;
  session_affinity: boolean;
};
type APIKey = { id: string; preview: string };
const navigation = [
  "概览",
  "账号与额度",
  "用量统计",
  "运行日志",
  "API Keys",
  "设置",
];
const initialUsage = {
  requests: 0,
  errors: 0,
  input_tokens: 0,
  output_tokens: 0,
};
function App() {
  const [key, setKey] = useState("");
  const sessionKey = useRef(key);
  sessionKey.current = key;
  const [inputKey, setInputKey] = useState("");
  const [page, setPage] = useState("概览");
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [usage, setUsage] = useState(initialUsage);
  const [events, setEvents] = useState<Event[]>([]);
  const [settings, setSettings] = useState<Settings | null>(null);
  const [keys, setKeys] = useState<APIKey[]>([]);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [login, setLogin] = useState<{
    url: string;
    state: string;
    user_code: string;
    interval: number;
  } | null>(null);
  const [newKey, setNewKey] = useState("");
  const [models, setModels] = useState<string[] | null>(null);
  async function api<T>(
    path: string,
    method = "GET",
    body?: unknown,
    signal?: AbortSignal,
  ): Promise<T> {
    const response = await fetch(`/v0/management/${path}`, {
      method,
      headers: {
        Authorization: `Bearer ${key}`,
        "Content-Type": "application/json",
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
    });
    const result = await response.json().catch(() => ({}));
    if (sessionKey.current !== key) throw new Error("会话已结束");
    if (!response.ok)
      throw new Error(result.error?.message ?? `HTTP ${response.status}`);
    return result;
  }
  async function refresh() {
    const [a, u, e, c, k] = await Promise.all([
      api<Account[]>("accounts"),
      api<Usage>("usage"),
      api<Event[]>("logs"),
      api<Settings>("config"),
      api<APIKey[]>("api-keys"),
    ]);
    setAccounts(a ?? []);
    setUsage(u);
    setEvents(e ?? []);
    setSettings(c);
    setKeys(k ?? []);
  }
  async function action(fn: () => Promise<void>) {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      await fn();
      await refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : "操作失败");
    } finally {
      setBusy(false);
    }
  }
  useEffect(() => {
    if (!key) return;
    const controller = new AbortController();
    let alive = true;
    const load = async () => {
      try {
        const [a, u, e, c, k] = await Promise.all([
          api<Account[]>("accounts", "GET", undefined, controller.signal),
          api<Usage>("usage", "GET", undefined, controller.signal),
          api<Event[]>("logs", "GET", undefined, controller.signal),
          api<Settings>("config", "GET", undefined, controller.signal),
          api<APIKey[]>("api-keys", "GET", undefined, controller.signal),
        ]);
        if (alive) {
          setAccounts(a ?? []);
          setUsage(u);
          setEvents(e ?? []);
          setSettings(c);
          setKeys(k ?? []);
        }
      } catch (e) {
        if (alive) setError(e instanceof Error ? e.message : "加载失败");
      }
    };
    void load();
    const timer = setInterval(() => void load(), 15000);
    return () => {
      alive = false;
      controller.abort();
      clearInterval(timer);
    };
  }, [key]);
  useEffect(() => {
    if (!login || !key) return;
    let alive = true;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      try {
        const result = await api<{ status: string }>(
          `login/status?state=${encodeURIComponent(login.state)}`,
        );
        if (!alive) return;
        if (result.status === "success") {
          setLogin(null);
          setNotice("GitHub Copilot 账号已添加");
          await refresh();
        } else if (result.status === "pending") {
          timer = setTimeout(poll, Math.max(1, login.interval) * 1000);
        } else {
          setLogin(null);
          setError("GitHub 授权未完成");
        }
      } catch (e) {
        if (alive) {
          setLogin(null);
          setError(e instanceof Error ? e.message : "授权失败");
        }
      }
    };
    timer = setTimeout(poll, Math.max(1, login.interval) * 1000);
    return () => {
      alive = false;
      clearTimeout(timer);
    };
  }, [login, key]);
  const logout = () => {
    sessionKey.current = "";
    setKey("");
    setInputKey("");
    setLogin(null);
    setAccounts([]);
    setKeys([]);
    setEvents([]);
    setSettings(null);
    setNewKey("");
    setError("");
  };
  if (!key)
    return (
      <main className="login">
        <div className="brand-mark">C</div>
        <span className="eyebrow">ONE SERVICE. YOUR COPILOT.</span>
        <h1>Copilot Proxy</h1>
        <p>专注 GitHub Copilot 的独立 API 服务</p>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            void (async () => {
              setBusy(true);
              setError("");
              try {
                const response = await fetch("/v0/management/usage", {
                  headers: { Authorization: `Bearer ${inputKey}` },
                });
                if (!response.ok) throw new Error();
                setKey(inputKey);
                setInputKey("");
              } catch {
                setError("管理密钥无效或服务不可用");
              } finally {
                setBusy(false);
              }
            })();
          }}
        >
          <label>
            管理密钥
            <input
              autoComplete="current-password"
              aria-label="管理密钥"
              type="password"
              value={inputKey}
              onChange={(e) => setInputKey(e.target.value)}
              required
            />
          </label>
          <button className="primary" disabled={busy}>
            进入控制台 →
          </button>
        </form>
        {error && <p role="alert">{error}</p>}
        <small>
          首次启动后，从私有的 data/config.yaml 获取
          management-key。密钥不会存入浏览器持久存储。
        </small>
      </main>
    );
  const active = accounts.filter(
    (a) => !a.disabled && !a.budget_guard.blocked,
  ).length;
  return (
    <div className="shell">
      <aside>
        <div className="brand">
          <b className="brand-mark">C</b>
          <div>
            Copilot Proxy<small>STANDALONE</small>
          </div>
        </div>
        <nav>
          {navigation.map((item, i) => (
            <button
              className={page === item ? "selected" : ""}
              key={item}
              onClick={() => setPage(item)}
            >
              <span>{["◈", "◎", "▥", "≡", "⌘", "⚙"][i]}</span>
              {item}
            </button>
          ))}
        </nav>
        <div className="sidebar-foot">
          <i /> GitHub Copilot only<small>单二进制 · 内置管理面板</small>
          <button disabled={busy} onClick={logout}>
            退出登录
          </button>
        </div>
      </aside>
      <main>
        <header>
          <div>
            <span className="eyebrow">COPILOT WORKSPACE</span>
            <h1>{page}</h1>
          </div>
          <button onClick={() => void action(async () => {})} disabled={busy}>
            ↻ 刷新
          </button>
        </header>
        {error && (
          <div className="banner error" role="alert">
            {error}
            <button disabled={busy} onClick={logout}>
              重新登录
            </button>
          </div>
        )}
        {notice && (
          <div className="banner" role="status">
            {notice}
          </div>
        )}
        {(page === "概览" || page === "用量统计") && (
          <>
            <section className="metrics">
              {[
                ["可用账号", `${active} / ${accounts.length}`],
                ["累计请求", usage.requests.toLocaleString()],
                ["输入 Tokens", usage.input_tokens.toLocaleString()],
                ["输出 Tokens", usage.output_tokens.toLocaleString()],
              ].map(([label, value]) => (
                <article key={label}>
                  <span>{label}</span>
                  <strong>{value}</strong>
                </article>
              ))}
            </section>
            <section className="panel intro">
              <div>
                <span className="eyebrow">ONE UPSTREAM, THREE PROTOCOLS</span>
                <h2>一个 Copilot 订阅，连接你的工具。</h2>
                <p>
                  兼容 Chat Completions、Responses 和 Anthropic
                  Messages。账号级预算保护在后台持续检查，不依赖此页面。
                </p>
                <div className="tags">
                  <code>/v1/chat/completions</code>
                  <code>/v1/responses</code>
                  <code>/v1/messages</code>
                </div>
              </div>
              <div className="orb">✳</div>
            </section>
            <div className="section-head">
              <h2>最近请求</h2>
              <span>错误 {usage.errors} · 额度以 GitHub 返回为准</span>
            </div>
            <EventTable
              events={events
                .filter((e) => e.event === "request.completed")
                .slice(-20)}
            />
          </>
        )}
        {page === "账号与额度" && (
          <>
            <div className="toolbar">
              <p>每 60 秒后台检查额度；超过 99% 额外预算后暂停整个账号。</p>
              <div>
                <label className="button">
                  导入认证文件
                  <input
                    type="file"
                    accept="application/json,.json"
                    hidden
                    disabled={busy}
                    onChange={(e) => {
                      const file = e.target.files?.[0];
                      if (!file) return;
                      void action(async () => {
                        if (file.size > 16 * 1024 * 1024)
                          throw new Error("文件过大");
                        const credential = JSON.parse(await file.text());
                        await api("accounts", "POST", {
                          name: file.name,
                          credential,
                        });
                        setNotice("Copilot 认证文件已导入");
                      });
                      e.target.value = "";
                    }}
                  />
                </label>
                <button
                  className="primary"
                  disabled={busy}
                  onClick={() =>
                    void action(async () => {
                      setLogin(await api("login", "POST"));
                    })
                  }
                >
                  ＋ GitHub 登录
                </button>
              </div>
            </div>
            {login && (
              <section className="panel login-flow">
                <h2>完成 GitHub 授权</h2>
                <p>
                  输入验证码 <code>{login.user_code}</code>{" "}
                  后，此处会自动添加账号。
                </p>
                <a
                  className="button primary"
                  href={login.url}
                  target="_blank"
                  rel="noopener noreferrer"
                >
                  打开 GitHub 授权页 ↗
                </a>
                <button onClick={() => setLogin(null)}>关闭</button>
              </section>
            )}
            <section className="account-grid">
              {accounts.map((a) => (
                <article className="account-card" key={a.id}>
                  <div className="account-heading">
                    <span className="avatar">GH</span>
                    <div>
                      <h2>{a.account || a.name}</h2>
                      <small>{a.name}</small>
                    </div>
                    <span
                      className={`badge ${a.budget_guard.blocked ? "warn" : ""}`}
                    >
                      {a.disabled
                        ? "已禁用"
                        : a.budget_guard.blocked
                          ? "预算暂停"
                          : "已启用"}
                    </span>
                  </div>
                  <QuotaPanel account={a} />
                  <div className="account-meta">
                    <label>
                      优先级{" "}
                      <input
                        type="number"
                        defaultValue={a.priority}
                        key={a.priority}
                        min={-1000000}
                        max={1000000}
                        onBlur={(e) => {
                          const value = Number(e.target.value);
                          if (value !== a.priority)
                            void action(async () => {
                              await api("accounts", "PATCH", {
                                id: a.id,
                                priority: value,
                              });
                            });
                        }}
                      />
                    </label>
                    <button onClick={() => setModels(a.models ?? [])}>
                      {a.models?.length ?? 0} 个模型
                    </button>
                  </div>
                  <footer>
                    <button
                      disabled={busy}
                      onClick={() =>
                        void action(async () => {
                          await api("quota/fetch", "POST", { id: a.id });
                          setNotice("额度已查询（最多复用 60 秒缓存）");
                        })
                      }
                    >
                      刷新额度
                    </button>
                    <button
                      disabled={busy}
                      onClick={() =>
                        void action(async () => {
                          await api("accounts/refresh", "POST", { id: a.id });
                        })
                      }
                    >
                      刷新凭据
                    </button>
                    <button
                      disabled={busy}
                      onClick={() =>
                        void action(async () => {
                          await api("accounts", "PATCH", {
                            id: a.id,
                            disabled: !a.disabled,
                          });
                        })
                      }
                    >
                      {a.disabled ? "启用" : "禁用"}
                    </button>
                    <button
                      className="danger"
                      disabled={busy}
                      onClick={() => {
                        if (confirm(`删除 ${a.name}？`))
                          void action(async () => {
                            await api(
                              `accounts?id=${encodeURIComponent(a.id)}`,
                              "DELETE",
                            );
                          });
                      }}
                    >
                      删除
                    </button>
                  </footer>
                </article>
              ))}
            </section>
            {accounts.length === 0 && (
              <Empty
                title="添加你的第一个 Copilot 账号"
                text="使用 GitHub Device Flow 登录，或导入现有 CPA 的 Copilot 认证 JSON。其他提供商的凭据不会被接受。"
              />
            )}
          </>
        )}
        {page === "运行日志" && (
          <>
            <p className="muted">
              仅记录结构化运行事件与请求统计，不记录请求正文、授权头或 Copilot
              token。最近 500 条事件保留在内存中。
            </p>
            <EventTable events={events} />
          </>
        )}
        {page === "API Keys" && (
          <>
            <div className="toolbar">
              <p>用于客户端调用 /v1/*，与管理密钥相互独立。</p>
              <button
                className="primary"
                disabled={busy}
                onClick={() =>
                  void action(async () => {
                    const result = await api<{ key: string }>(
                      "api-keys",
                      "POST",
                    );
                    setNewKey(result.key);
                  })
                }
              >
                生成 API Key
              </button>
            </div>
            {newKey && (
              <div className="panel">
                <strong>请立即保存，只显示一次</strong>
                <pre>{newKey}</pre>
                <button
                  onClick={() => void navigator.clipboard.writeText(newKey)}
                >
                  复制
                </button>
                <button onClick={() => setNewKey("")}>已保存</button>
              </div>
            )}
            <section className="panel">
              {keys.map((k) => (
                <div className="key-row" key={k.id}>
                  <code>{k.preview}</code>
                  <span>API 访问凭据</span>
                  <button
                    className="danger"
                    disabled={busy || keys.length < 2}
                    onClick={() => {
                      if (confirm("撤销此 API Key？"))
                        void action(async () => {
                          await api(`api-keys?id=${k.id}`, "DELETE");
                        });
                    }}
                  >
                    撤销
                  </button>
                </div>
              ))}
            </section>
          </>
        )}
        {page === "设置" && settings && (
          <section className="panel settings">
            <h2>代理与账号调度</h2>
            <p className="muted">
              修改后立即生效。监听地址、模型别名、Copilot
              协议选项和管理密钥通过配置文件维护，修改这些项后重启服务。
            </p>
            <form
              key={`${settings.routing}-${settings.proxy_url}-${settings.session_affinity}`}
              onSubmit={(e) => {
                e.preventDefault();
                const data = new FormData(e.currentTarget);
                void action(async () => {
                  await api("config", "PATCH", {
                    ...(settings.proxy_redacted && !String(data.get("proxy"))
                      ? {}
                      : { proxy_url: String(data.get("proxy")) }),
                    routing: String(data.get("routing")),
                    session_affinity: data.get("affinity") === "on",
                  });
                  setNotice("设置已保存");
                });
              }}
            >
              <label>
                上游代理
                <input
                  name="proxy"
                  defaultValue={settings.proxy_url}
                  placeholder="http://127.0.0.1:7890"
                />
              </label>
              <small>
                含用户名或密码的代理不会在页面回显；如已配置，请通过配置文件修改。
              </small>
              <label>
                调度策略
                <select name="routing" defaultValue={settings.routing}>
                  <option value="round-robin">同优先级轮询</option>
                  <option value="fill-first">优先使用首个可用账号</option>
                </select>
              </label>
              <label className="check">
                <input
                  name="affinity"
                  type="checkbox"
                  defaultChecked={settings.session_affinity}
                />
                按 X-Session-Id 保持会话账号
              </label>
              <button className="primary" disabled={busy}>
                保存设置
              </button>
            </form>
            <hr />
            <h3>预算保护</h3>
            <p>
              Additional Budget 严格超过 99%
              时暂停。到达重置时间并确认新周期额度后才恢复；手动禁用不会被自动解除。并发请求与上游计量延迟仍可能导致超支。
            </p>
          </section>
        )}
      </main>
      {models && (
        <div className="modal-backdrop">
          <section className="panel modal">
            <div className="section-head">
              <h2>当前账号模型</h2>
              <button onClick={() => setModels(null)}>关闭</button>
            </div>
            <pre>{models.join("\n") || "尚无模型，请刷新凭据。"}</pre>
          </section>
        </div>
      )}
    </div>
  );
}
function QuotaPanel({ account: a }: { account: Account }) {
  const q = a.quota ?? {};
  return (
    <section className="quota">
      <strong>
        {q.subscription?.tierName ?? q.subscription?.plan ?? "GitHub Copilot"}
      </strong>
      {a.budget_guard.blocked && (
        <div className="budget-notice" role="status">
          预算保护暂停 · 等待重置校验
          <br />
          {resetLabel(a.budget_guard.reset_at)}
        </div>
      )}
      {q.stale && (
        <small className="muted">
          额度等待更新；旧数据不用于解除预算保护。
        </small>
      )}
      {buckets(q).map((b, i) => (
        <div className="quota-row" key={`${b.window}-${i}`}>
          <div>
            <span>{b.description ?? b.window}</span>
            <b>{usedPercent(b.remainingFraction)}% used</b>
          </div>
          <div className="meter">
            <i
              className={b.remainingFraction < 0.1 ? "low" : ""}
              style={{
                width: `${Math.max(0, Math.min(1, b.remainingFraction)) * 100}%`,
              }}
            />
          </div>
          <small>{resetLabel(b.resetTime)}</small>
        </div>
      ))}
      {q.summary
        ?.filter((m) => m.key === "credits_used")
        .map((m) => (
          <p key={m.key}>Credits used: {m.value.toLocaleString()}</p>
        ))}
      {!q.groups?.length && !q.summary?.length && (
        <p className="muted">后台正在查询订阅额度。</p>
      )}
    </section>
  );
}
function EventTable({ events }: { events: Event[] }) {
  return events.length ? (
    <div className="panel table-wrap">
      <table>
        <thead>
          <tr>
            <th>时间</th>
            <th>事件 / 模型</th>
            <th>账号</th>
            <th>状态</th>
            <th>耗时</th>
          </tr>
        </thead>
        <tbody>
          {[...events].reverse().map((e, i) => (
            <tr key={`${e.time}-${i}`}>
              <td>{new Date(e.time).toLocaleTimeString()}</td>
              <td>{e.model || e.event}</td>
              <td>{e.account || "—"}</td>
              <td>
                <span
                  className={`badge ${(e.status ?? 0) >= 400 ? "warn" : ""}`}
                >
                  {e.status ?? e.level}
                </span>
              </td>
              <td>
                {e.duration_ms === undefined ? "—" : `${e.duration_ms} ms`}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  ) : (
    <Empty title="暂无记录" text="开始使用 API 后，请求统计会显示在这里。" />
  );
}
function Empty({ title, text }: { title: string; text: string }) {
  return (
    <section className="empty">
      <span>◎</span>
      <h2>{title}</h2>
      <p>{text}</p>
    </section>
  );
}
createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
