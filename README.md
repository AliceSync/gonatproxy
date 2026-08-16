# DeepSeek AI Tunnel (Go)

A Go TLS tunnel for AI backends and NAT traversal. It has **three** components:

- **server** (`cmd/server`) — the control plane and relay hub. It decides
  *everything*: which local ports the client listens on, where each port
  forwards to (a server-side TCP target **or** a NAT client's service), and
  who may register as a NAT client. Certificates are auto-reloaded (ACME).
- **client** (`cmd/client`, the "entry" endpoint) — runs alongside AI tooling.
  It only knows the server address, dials out over TLS, gets its port list from
  the server, and blindly tunnels inbound local connections.
- **natclient** (`cmd/natclient`) — runs on a host behind NAT. It dials out to
  the server, registers its identity + the local services it exposes, and
  serves inbound streams from the server. Multiple services on multiple ports
  are supported.

```
AI tool --(5051)--> client ═TLS═> server ──> deepseek backend (127.0.0.1:8000)
AI tool --(5052)--> client ═TLS═> server ──> lmstudio       (127.0.0.1:1234)
HTTP    --(8080)--> client ═TLS═> server ──> NAT client "nat-home" ═> local http service
HTTPS   --(8081)--> client ═TLS═> server ──> NAT client "nat-home" ═> local tcp service
```

Both `client` and `natclient` dial **out** to the server, so no inbound firewall
rules are needed on their side — this is what makes NAT traversal possible. The
server is the only publicly reachable endpoint. Supported OS: **Linux**.

## Roles in one picture

| component | connects | trusts | decides |
|-----------|----------|--------|---------|
| server    | listens (TLS) | — | all routing, ports, NAT ids |
| client    | dials server | server cert | nothing (only server addr) |
| natclient | dials server | server cert | its own local services (its config) |

## Directory layout

```
deepseekaiworker/
├── cmd/
│   ├── server/main.go         # TLS hub: entry/nat roles, routing, ACME reload
│   ├── server/certmanager.go  # auto-reloads cert/key when files change (ACME)
│   ├── client/main.go         # entry tunnel endpoint (dynamic local ports)
│   └── natclient/main.go      # exposes local NAT services via the server
├── internal/
│   ├── mux/                   # length-framed multiplexed stream protocol
│   ├── tlscfg/                # shared client TLS trust (CA/fingerprint/skip)
│   └── ...
├── configs/                   # server.json, client.json, natclient.json
├── systemd/                   # deepseek-server/.service, deepseek-client@, deepseek-natclient@
├── scripts/                   # build.sh, gen_certs.sh, install.sh
├── go.mod
└── README.md
```

## TLS trust (client & natclient)

The client verifies the server's certificate. Pick one mode via config:

| field                    | meaning |
|--------------------------|---------|
| `ca_file` *(default)*    | verify against a trusted CA. **Recommended.** |
| `server_fingerprint`     | verify the server's leaf cert equals this hex(sha256). Use when CA rotates (e.g. together with ACME) and you cannot pin the CA. |
| `insecure_skip_verify`   | disable verification (only for initial bring-up / lab). |

If none is set, startup fails with *"nothing to trust"*. Precedence:
`server_fingerprint` > `insecure_skip_verify` > `ca_file`.

> Note on ACME + fingerprint: if your CA rotates certs frequently, the
> fingerprint must be updated to match. Using `ca_file` *(default)* avoids that
> — point it at your ACME CA and let verification stay stable across renewals.

## Server config (`configs/server.json`)

- `cert_file` / `key_file` — ACME-managed paths are fine; the server **polls
  every `cert_check_seconds` and reloads automatically** when files change.
  A failed reload keeps the previous cert and retries later.
- `clients` — map of `nat client id → secret`. Only NAT clients with a
  matching secret may register.
- `routes` — each route has a `listen_port` (the local port the entry client
  binds) and **exactly one** of:
  - `target` — a server-side `host:port` to dial, or
  - `nat_client` + `service` — a NAT client's registered service name.

```json
"routes": [
  { "name": "deepseek", "listen_port": 5051, "target": "127.0.0.1:8000" },
  { "name": "home-dash", "listen_port": 8080, "nat_client": "nat-home", "service": "dashboard" }
]
```

## NAT client config (`configs/natclient.json`)

- `client_id` + `secret` must match an entry in the server's `clients` map.
- `services` — the local services to expose; each has `name`, a local `port`,
  and optional `addr` (defaults to `127.0.0.1:<port>`). Add as many as you
  need — each is reachable through its own route.

```json
"services": [
  { "name": "dashboard", "port": 8080, "addr": "127.0.0.1:8080" },
  { "name": "api",       "port": 8081, "addr": "127.0.0.1:8081" }
]
```

Changing a NAT client's config only on the NAT side is enough: on reconnect it
re-registers with the server, which updates its view.

## Build

```bash
./scripts/build.sh
# produces bin/deepseek-server, bin/deepseek-client, bin/deepseek-natclient
```

## Server CLI

```text
deepseek-server                  run in the foreground
deepseek-server run              run in the foreground (explicit)
deepseek-server start            run in the background (daemonized)
deepseek-server version          print version and exit
Flags: -config <path>   (default config.json; works before or after the subcommand)
```

- **foreground** is the default; `start` re-launches itself detached in the
  background (logs to `log_file` or `<config dir>/server.log`) and returns.
- **No config file**: the server starts in **initialization mode** — it runs
  only the web console (over **HTTPS** with an in-memory self-signed cert) so
  you can create an account, edit and generate a config, then start normally.

## Logging (optional, size-capped)

- **No `log_file` configured?** Completely optional:
  - running foreground (or under systemd) → logs go to the process's
    stdout/stderr, i.e. the **systemd journal** under systemd;
  - running daemonized via `start` → an implicit `<config dir>/server.log`
    is used so the background process still has a place to log.
- **`log_max_bytes`** (default 100 MiB) caps how large the log grows; once
  exceeded it **rotates** to `server.log.1 → .2 → …`, keeping
  **`log_max_files`** (default 5) backups. Set `log_max_bytes: 0` to disable
  rotation. Both are editable in the 配置 page.

## Web console

The server embeds a management console (login = **account + password**):

- First run with no admin account → **initialization mode** on
  `https://localhost:8443` that walks you through creating the admin account.
- Default console address is the relay port **`:8443`**, **shared** on the same
  TLS port: the server demuxes HTTP(console) vs tunnel(relay) per connection,
  so your browser and the AI clients use the same 8443/TLS.
- You can also give the console an **independent** port via `admin.listen`
  (e.g. `":8445"`); leave it empty to share `:8443`.
- If `:8443` is busy at startup in init mode, the server picks an ephemeral
  free port and tells you.
- With no cert files configured, an **ephemeral self-signed certificate** is
  generated in memory (console is still HTTPS); set `cert_file`/`key_file`
  (ACME) for persistent certs.
- After login you can:
  - edit every configurable item (relay listen, cert paths, cert-check interval,
    dial timeout, routes, NAT clients + secrets/services, admin listen/TLS);
  - **export** ready-to-use `client.json` and `natclient.json` configs;
  - save → config is persisted + hot-reloaded (no restart). The console
    manages NAT client secrets/services for clean export.
- Failed login attempts are rate-limited per IP.

### Management pages

- **文件 (Files)** — a full file manager: browse, jump to a path, create/delete
  files & dirs, upload, download, and edit text files in-browser. Restricted by
  `FileRoot` when set (unset = whole filesystem).
- **配置 (Config)** — every path field (`cert_file`, `key_file`, `log_file`,
  and the admin console TLS cert/key) has a **选择** button that opens the file
  manager in *select mode*: pick a server-side file and it is filled into the
  field. This is how you choose certificate files instead of typing a path.
- **服务 (Services)** — systemd unit management:
  - **生成 / 更新 unit 文件** writes `deepseek-server.service` from the *current*
    executable and config paths (so the service reads the **same config** the
    console manages) and runs `daemon-reload`.
  - **enable（自启）** and **start（立即）** are separated — enable does NOT start.
  - **平滑交接**: if the server was launched via `start` (daemonized) or in the
    foreground and is *not* yet a systemd service, clicking **start** first closes
    the listeners gracefully (freeing the port), lets systemd take over, then the
    old process exits cleanly. So systemd owns the port and the correct config.
- **系统 (System)** — binary updates + restart:
  - Upload a new `deepseek-server`; choose whether to **apply immediately** (mv +
    fork start) or just stage it. On the system page you can then **apply and
    restart** the staged update, or **restart now**.
  - Restart is either delegated to `systemctl restart` (systemd) or a graceful
    fork (non-systemd), so the service mode keeps working correctly.

### systemd as a `service` CLI

Beyond the web console, the same unit generation is available as a command:

```bash
deepseek-server service install   # generate unit + enable (separate from start)
deepseek-server service start     # start now (graceful handoff)
deepseek-server service status
deepseek-server service show      # print the unit that would be installed
```

> **进程模式判定**：页面上"前台进程 / start 守护进程 / systemd 管理"按真实
> 状态判定——"systemd 管理"仅当 `deepseek-server.service` 已安装、活跃且
> 其 `MainPID` 等于当前进程 pid 时为真（不再仅凭环境变量 `INVOCATION_ID`
> 猜测），所以即使用 `go run` 启动也正确显示为"前台进程"。

## Install as systemd services

`scripts/install.sh` installs binaries, a (per-role) unit, and config. The
server unit starts `deepseek-server run` in the foreground under systemd.

```bash
sudo ./scripts/install.sh server                    # server host + web console
sudo ./scripts/install.sh client                    # entry host
sudo ./scripts/install.sh natclient my-nat-id       # NAT host
```


## Test run

```bash
# 1. certs (on the server host)
./scripts/gen_certs.sh <SERVER_IP_OR_HOST>...
# 2. edit configs, then:
./bin/deepseek-server    -config configs/server.json
./bin/deepseek-natclient -config configs/natclient.json   # NAT host
./bin/deepseek-client    -config configs/client.json      # entry host
# connect:
curl http://127.0.0.1:5051/...    # -> deepseek target
curl http://127.0.0.1:8080/...    # -> NAT client's local service
```

## Install as systemd services

`scripts/install.sh` is provided:

```bash
sudo ./scripts/install.sh server                    # server host
sudo ./scripts/install.sh client                    # entry host (binds local ports)
sudo ./scripts/install.sh natclient my-nat-id       # NAT host: deepseek-natclient@my-nat-id
```

Services:
- `deepseek-server.service`
- `deepseek-client@.service` (per-user template)
- `deepseek-natclient@.service` (per-id template, reads `/etc/deepseek/natclient-<id>.json`)

All restart automatically; the client and natclient also reconnect on their
own. If you need local entry ports **<1024**, run as root or add
`CAP_NET_BIND_SERVICE`.

## Protocol (short)

One persistent TLS connection per component pair, multiplexed by a 4-byte
connection id. Frame: `[type 1B][conn id 4B][len 4B][payload N B]`.

| type | meaning |
|------|---------|
| 0x01 OPEN     | client→server: open stream, payload = 2-byte listen port. server→natclient: open stream, payload = 2-byte service port |
| 0x02 DATA     | either direction, raw tunnel bytes |
| 0x03 CLOSE    | half-close: no more DATA from sender |
| 0x04 CONFIG   | server→entry client: JSON ports list |
| 0x05 REGISTER | client→server (first frame): role + id/secret/services |

Handshake: client dials TLS → sends REGISTER → server routes by role
(`entry` gets a CONFIG; `nat` is registered and receives OPENs for its
services). Reliability features: auto-reconnect with backoff, certificate
auto-reload, mutex-protected framing, and per-connection relay goroutines that
survive transient load (verified race-free under load).

## Performance / reliability notes

- Raw byte relay with 32 KiB buffers; no protocol parsing on the hot path.
- All frame writes per connection are serialized by one mutex; streams don't
  block one another except by the shared socket write lock (standard for
  multiplexed tunnels).
- Verified under concurrent load and with the Go race detector (0 races).
