# DeepSeek AI Tunnel（Go 版）

一个用 Go 编写的 TLS 隧道程序，用于把「AI 工具 → 本地入口」的数据安全地转发到
DeepSeek 后端、以及穿透 NAT 暴露内网服务。它包含**三个可执行程序**，只支持 Linux：

- **server**（`cmd/server`，控制面 + 中继枢纽）：它决定一切——入口客户端要绑定哪些
  本地端口、每个端口转发到哪里（服务器本地的 TCP 目标，**或**某个 NAT 客户端的
  服务）、以及允许哪些 NAT 客户端注册。证书支持自动加载（ACM 场景）。
- **client**（`cmd/client`，入口端点）：运行在 AI 工具所在的机器上。它只知道服务器
  地址，向外拨号走 TLS，从服务器拿到端口列表，然后在本地监听这些端口，把每一个本地
  入站连接原样隧穿给服务器。
- **natclient**（`cmd/natclient`）：运行在 NAT 后面的主机上。它向外拨号到服务器
  （因此能穿越 NAT），注册自己的身份和暴露的本地服务，并服务服务器转发过来的入站流。
  支持在多个端口暴露多个服务。

下图是三种程序的分工关系：

```
AI 工具 --(5051)--> client ══TLS═▶ server ──▶ DeepSeek 后端 (127.0.0.1:8000)
AI 工具 --(5052)--> client ══TLS═▶ server ──▶ lmstudio       (127.0.0.1:1234)
浏览器 --(8080)--> client ══TLS═▶ server ──▶ NAT 客户端 "nat-home" ═▶ 本地 http 服务
HTTPS  --(8081)--> client ══TLS═▶ server ──▶ NAT 客户端 "nat-home" ═▶ 本地 tcp 服务
```

`client` 和 `natclient` 都是**向外拨号**到服务器，因此它们那边不需要任何入站防火墙
规则——这正是能做 NAT 穿透的原因。服务器才是唯一对外开放的端点。

## 三种程序一句话理解

| 程序 | 连接方向 | 信任 | 决定 |
|------|---------|------|------|
| server | 监听（TLS） | — | 全部路由、端口、NAT 身份 |
| client | 拨号到 server | server 证书 | 无（只知道 server 地址） |
| natclient | 拨号到 server | server 证书 | 自己要暴露的本地服务（自己的配置） |

## 目录结构

```
deepseekaiworker/
├── cmd/
│   ├── server/main.go        # TLS 枢纽：入口/NAT 角色、路由、证书热加载
│   ├── server/certmanager.go # 证书文件变化时自动重载（ACME）
│   ├── client/main.go        # 入口隧道端点（动态本地端口）
│   └── natclient/main.go     # 通过 server 暴露本地 NAT 服务
├── internal/
│   ├── mux/                  # 长度分帧的多路复用流协议
│   ├── tlscfg/               # 客户端连接的 TLS 信任（CA/指纹/跳过校验）
│   ├── config/               # server 配置的读取/保存/校验
│   └── admin/                # 内嵌 Web 控制台（登录、配置、文件、服务、系统）
├── configs/                  # server.json, client.json, natclient.json 示例
├── systemd/                  # server/client/natclient 的 systemd 单元文件
├── scripts/                  # build.sh, gen_certs.sh, install.sh
├── go.mod
└── README.md
```

## 客户端 TLS 信任（client & natclient）

客户端要校验服务器证书，三种模式在配置里选一个即可：

| 字段 | 含义 |
|------|------|
| `ca_file` *(默认)* | 用受信任的 CA 校验。**推荐。** |
| `server_fingerprint` | 把服务器的叶子证书与这个 hex(sha256) 比对。CA 轮换（如 ACME）无法固定 CA 时可用。 |
| `insecure_skip_verify` | 完全跳过校验（仅用于首次搭建/内网实验）。 |

三者都不设则启动报错 *"nothing to trust"*。优先级：`server_fingerprint` >
`insecure_skip_verify` > `ca_file`。

## server 配置（`configs/server.json`）

- `cert_file` / `key_file` —— 可以是 ACME 管理的路径；服务器每 `cert_check_seconds`
  秒轮询一次，文件变化**自动重载**。重载失败会保留旧证书并稍后重试。
- `clients` —— `NAT 客户端 id → secret` 的映射。只有 secret 匹配的 NAT 客户端才能注册。
- `routes` —— 每条路由有一个 `listen_port`（入口 client 本地要绑定的端口）和恰好一个目标：
  - `target` —— 服务器本地的 `host:port` 目标；或
  - `nat_client` + `service` —— 某个 NAT 客户端注册的服务名。
- **`routes` 可以为空**：此时服务器只运行 Web 控制台（中继关闭），方便你先初始化账号、
  之后再补路由，不会启动报错。

```json
"routes": [
  { "name": "deepseek", "listen_port": 5051, "target": "127.0.0.1:8000" },
  { "name": "home-dash", "listen_port": 8080, "nat_client": "nat-home", "service": "dashboard" }
]
```

## NAT 客户端配置（`configs/natclient.json`）

- `client_id` + `secret` 必须与 server 的 `clients` 映射里的一项一致。
- `services` —— 要暴露的本地服务，每个有 `name`、本地 `port`，`addr` 可选（默认
  `127.0.0.1:<port>`）。可加多个，每个通过自己的路由可达。

```json
"services": [
  { "name": "dashboard", "port": 8080, "addr": "127.0.0.1:8080" },
  { "name": "api",       "port": 8081, "addr": "127.0.0.1:8081" }
]
```

修改 NAT 客户端的配置只需改 NAT 那侧：重连后它会重新注册，服务器更新视图。

## 构建

```bash
./scripts/build.sh
# 生成 bin/deepseek-server, bin/deepseek-client, bin/deepseek-natclient
```

## server 命令行

```text
deepseek-server                  # 前台运行
deepseek-server run              # 前台运行（显式）
deepseek-server start            # 后台守护运行（daemonized）
deepseek-server service <子命令>  # 管理 systemd 服务单元
deepseek-server version          # 打印版本
Flags: -config <path>   (默认为 config.json；可在子命令前后)
```

- **前台是默认**；`start` 会用 `Setsid` 把自己重新拉起到后台（各角色日志写入 `log_file`
  或 `<配置目录>/server.log`）并返回。
- **没有配置文件**：服务器进入**初始化模式**，只跑 Web 控制台，方便你建号、生成配置后再正常启动。

## 日志（可选，且带大小上限）

- **`log_file` 可以不配置**：
  - 前台或 systemd 下运行 → 日志直接进进程的 stdout/stderr，即 systemd 的 **journal**；
  - 仅用 `start` 后台守护时 → 隐式使用 `<配置目录>/server.log`，保证后台进程仍有去处。
- **`log_max_bytes`**（默认 100 MiB）限制日志大小，超过后**轮转**成 `server.log.1
  → .2 → …`，保留 **`log_max_files`**（默认 5）份备份。设为 `0` 可关闭轮转。
  这些都能在配置页编辑。

## Web 控制台

服务器内嵌一个管理控制台（登录 = **账号 + 密码**）：

- 首次运行没有管理员账号 → **初始化模式**，在 Web 上引导创建管理员账号。
- 默认控制台地址是中继端口 **`:8443`**，**同端口共享**：服务器按连接区分 HTTP（控制台）
  与隧道（中继），所以浏览器和 AI 客户端共用同一个 8443/TLS。
- 也可通过 `admin.listen` 给控制台独立端口（如 `":8445"`）；留空则共享 `:8443`。
- 若 `:8443` 启动时被占用，初始化模式会退到一个可用临时端口并提示你。
- 没有配置证书时在内存里生成**临时自签名证书**（控制台仍是 HTTPS）；配置
  `cert_file`/`key_file` 用持久证书。
- 登录后可：
  - 编辑所有可配置项（中继监听、证书路径、证书检查间隔、拨号超时、路由、NAT 客户端
    + secret/服务、控制台监听/TLS）；
  - **导出**现成的 `client.json` 和 `natclient.json`；
  - 保存 → 配置持久化并**热重载无需重启**。
- 登录失败按 IP 限速。

### 页面

- **概览**：运行模式（前台 / start 守护 / systemd 管理）、监听、路由、NAT 客户端汇总。
- **配置**：所有路径字段（`cert_file`、`key_file`、`log_file`、控制台证书/私钥）都有
  **选择**按钮，会打开文件管理器选择模式，选中的服务器端文件会回填到字段里——
  证书等再也不用手打路径。另可填日志轮转上限/份数；删除路由/客户端/服务有确认对话框。
- **文件**：文件管理器——浏览、跳转目录、新建/删除文件与目录、上传、下载、在线编辑文本。
- **服务**：systemd 单元管理：
  - **生成/更新 unit 文件**：按服务器**当前可执行文件与配置路径**生成
    `deepseek-server.service` 并 `daemon-reload`，保证服务读取的配置与控制台完全一致；
  - **enable（自启）与 start（立即）分开**——enable 不启动；
  - **平滑交接**：若当前进程不是 systemd 管理（前台或 `start` 守护），点 **start** 时
    先平滑关闭监听、释放端口，让 systemd 接管启动，随后旧进程干净退出。
  - 进程模式判定严格按真实状态：仅当 unit 已安装、活跃且其 `MainPID`==当前 pid 才显示
    "systemd 管理"，`go run` 启动会正确显示为"前台进程"。
- **系统**：更新程序 + 重启。上传新的 `deepseek-server`，选择是否上传后立即替换重启
  （mv 替换 + fork 启动），或先暂存稍后应用；另有单独的重启按钮。所有操作有明确的确认对话框。
- 所有对话框（确认/输入/提示）都是自定义美观样式，不再用原生 `alert/confirm/prompt`。

## 作为 systemd `service` 命令

```bash
deepseek-server service install     # 生成 unit + enable（与 start 分开）
deepseek-server service enable      # 仅设置开机自启（不启动）
deepseek-server service start       # 立即启动（带平滑交接）
deepseek-server service stop|restart
deepseek-server service status
deepseek-server service show        # 打印要安装的 unit 内容（不写入）
```

### client / natclient 同样支持 systemd（无图形界面，全部命令行）

`client` 和 `natclient` 这两个程序没有 Web 控制台，因此直接内置 `service` 子命令
来生成并托管各自的 systemd 单元，用法与 server 一致（enable 与 start 分开）：

```bash
# 入口客户端
sudo deepseek-client    -config /etc/deepseek/client.json    service install   # 生成 unit + enable
sudo deepseek-client    -config /etc/deepseek/client.json    service start     # 启动
# NAT 客户端
sudo deepseek-natclient -config /etc/deepseek/natclient-<id>.json service install
sudo deepseek-natclient -config /etc/deepseek/natclient-<id>.json service start
# 通用：service enable|start|stop|restart|status|show
```

- 单元按**当前可执行文件与 `-config` 配置路径**生成：`/etc/systemd/system/deepseek-client.service`、
  `/etc/systemd/system/deepseek-natclient.service`（`User`/`Group`=当前用户，可用环境变量
  `DEEPSEEK_SERVICE_USER` 覆盖，测试可用 `DEEPSEEK_SYSTEMD_DIR` 指定单元目录）。
- 两个程序的**配置本就极简**（client 只需 server 地址 + 信任方式；natclient 只需
  server + id/secret/services），并且由 **server 控制台直接导出**（「配置」页的
  「导出 client / 导出 natclient 配置」）自动生成，不需手写 secret/服务。

> 进程模式提示：client / natclient 的前台实例没有 server 那样的自动交接。执行
> `service start` 前请先停止正在前台运行的旧实例（Ctrl-C），否则本地端口可能冲突。

### 部署助手（server 控制台「部署」页）

想真正"一条命令装好"？在 server 控制台 **「部署」页**，对**入口客户端**和**每个 NAT
客户端**各生成**一段可直接复制、以 root 在目标主机执行的安装命令**，它会自动：

1. 从 server 下载对应二进制（`/api/deploy/deepseek-client|natclient`）与现成配置
   （`/api/deploy/cfg/client.json`、`/api/deploy/cfg/natclient/<id>.json`，secret/服务
   直接从服务器配置生成）；
2. 写入 `/usr/local/bin/<二进制>` 和 `/etc/deepseek/<配置>`；
3. 运行 `deepseek-<role> service install` + `service start`，启用并启动 systemd 单元。

命令内置一个 **15 分钟有效期**的部署令牌（`Authorization: Bearer <token>`），以便目标
主机无需浏览器登录即可拉取文件；也可在页面上选择「暂用 insecure_skip_verify」批量改配置。
二进制目录由配置 `deploy_dir` 指定（默认取 server 可执行文件同目录；留空自动探测）。

> 部署助手依赖 `deploy_dir` 指向已经 `scripts/build.sh` 编译好的 deepseek-server/
> client/natclient 三个二进制。

## 安装为 systemd 服务

`scripts/install.sh` 安装二进制、单元文件、配置：

```bash
sudo ./scripts/install.sh server          # 服务器主机 + Web 控制台
sudo ./scripts/install.sh client          # 入口主机（绑定本地端口）
sudo ./scripts/install.sh natclient 我的id # NAT 主机：deepseek-natclient@我的id
```

server 单元以 `deepseek-server run` 前台方式在 systemd 下运行，日志进 journal。

## 运行测试（手动）

```bash
# 1. 在 server 主机生成证书
./scripts/gen_certs.sh <IP_或_域名>...
# 2. 编辑配置，然后分别启动
./bin/deepseek-server    -config configs/server.json
./bin/deepseek-natclient -config configs/natclient.json   # NAT 主机
./bin/deepseek-client    -config configs/client.json      # 入口主机
# 3. 通过入口客户端本地端口访问目标：
curl http://127.0.0.1:5051/...   # -> DeepSeek 后端
curl http://127.0.0.1:8080/...   # -> NAT 客户端的本地服务
```

> 已用 client + server + natclient 三者联调验证：入口本地端口 → NAT 路由 → 本地后端、
> 以及直接目标路由，均能拿到真实响应数据。

## 协议（简）

每对组件之间一条持久 TLS 连接，用 4 字节连接 id 多路复用。
帧格式：`[type 1B][conn id 4B][len 4B][payload N B]`

| type | 含义 |
|------|------|
| 0x01 OPEN     | client→server：open stream，payload=2 字节监听端口；server→natclient：payload=2 字节服务端口 |
| 0x02 DATA     | 双向原始隧道字节 |
| 0x03 CLOSE    | 半关闭：发送方不再有 DATA |
| 0x04 CONFIG   | server→入口 client：JSON 端口列表 |
| 0x05 REGISTER | client→server（首帧）：角色 + id/secret/services |

握手：client 拨 TLS → 发 REGISTER → server 按角色分发（`entry` 收到 CONFIG；
`nat` 注册并可收到其服务的 OPEN）。可靠性：自动重连退避、证书自动重载、互斥保护的
帧、多连接中继 goroutine 结构（并发与竞态检测下无竞态）。

## 性能/可靠性

- 原始字节中继，32 KiB 缓冲，热路径不做协议解析。
- 每个连接的所有帧写由一把互斥锁串行化；不同流互不阻塞（共享 socket 写锁为标准做法）。
- 并发负载与 Go 竞态检测下验证无竞态。
