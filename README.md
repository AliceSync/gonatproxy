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
  only the web console so you can create an account, edit and generate a config
  from the UI, then start the service normally.

## Web console

The server embeds a management console (login = **captcha + account/password**):
- First run with no admin account → **initialization mode** on
  `http://localhost:8444` that walks you through creating the admin account.
- After login, you can:
  - edit every configurable item (relay listen, cert paths, cert-check interval,
    dial timeout, routes, NAT clients + their secrets/services, admin listen, TLS);
  - **export** ready-to-use `client.json` and `natclient.json` configs with one
    click;
  - save → the config is persisted and hot-reloaded (no restart needed).
- Default console listen is in `admin.listen` (loopback). To expose it beyond
  the host, set `admin.listen` to e.g. `0.0.0.0:8444` and enable `admin.tls`
  (cert_file/key_file) — otherwise it is plain HTTP on loopback only.
- Config editing UI: dashboard → 配置. Routes and NAT clients are fully editable
  with add/remove rows, incl. nested services per NAT client.
- Login is protected with an image CAPTCHA + rate limiting on failures.

Server config uses the new schema: `clients` is an object of
`{"secret": ..., "services": [...]}` (see `configs/server.json`).

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
