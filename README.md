# VPS看板 (Bandwagon Dashboard)

一个基于搬瓦工 **KiwiVM API** 的多机器监控看板,用 **Go 单二进制** 实现,前端是自包含的卡片网格(无需任何前端构建、无需 CDN —— 终端库 xterm.js 也一并内嵌)。

监控多台 BandwagonHost VPS 的运行状态、IP、系统、套餐、**月度流量**(带进度条)、运行时长等,支持在看板上直接 **启动 / 停止 / 重启**,并提供两种浏览器终端:**SSH 真终端(PTY)** 与 **KiwiVM Basic Shell**。

## 功能

- 🗂 **多实例** —— 在一份配置里管理任意数量的 VPS
- 🟢 **状态总览** —— 在线 / 离线 / 错误 计数 + 全局流量汇总
- 📊 **卡片网格** —— 每台机器一张卡片,高亮流量用量(接近上限自动变黄/红)
- 🔍 **搜索 / 分组** —— 按名称、IP、节点、机房、分组过滤
- ⚡ **电源操作** —— 启动 / 停止 / 重启(危险操作二次确认)
- 🖥 **SSH 真终端** —— 浏览器里的完整 PTY(`top`/`vim`/tab 补全/颜色/后台任务都能用),凭据只留在服务器
- ⌨️ **Basic Shell** —— 不想配 SSH 时的兜底终端,走 KiwiVM API 执行单条命令
- 🔐 **可选密码登录** —— 部署到公网时给看板加一把锁
- 🔄 **后台轮询** —— 默认每 60 秒采集一次,前端自动刷新
- 📦 **单二进制** —— HTML、CSS、JS 与 xterm.js 全部嵌入,拷贝一个文件即可部署

## 获取 VEID 与 API Key

每台搬瓦工 VPS 都有一对凭证:

1. 登录搬瓦工客户中心 → `Services` → `My Services`
2. 点击目标 VPS 的 **KiwiVM Control Panel**
3. 左侧菜单 `KiwiVM Extras` → `API`,即可看到 **VEID**;点击 `Show API Key` 显示密钥(以 `private_` 开头)

> ⚠️ API Key 权限很高(可重启 / 重装 / 执行命令),切勿上传到公开仓库。本程序仅使用 POST 请求调用 API,不会把密钥写入 URL。

## 配置

复制示例并填入你的机器信息:

```bash
cp config.example.json config.json
```

```json
{
  "listen": ":8080",
  "title": "VPS看板",
  "password": "",
  "poll_seconds": 60,
  "shell": { "enabled": true, "timeout_seconds": 60 },
  "instances": [
    {
      "id": "tokyo-1", "name": "东京节点", "group": "亚洲",
      "veid": "1234567", "api_key": "private_xxx",
      "ssh": { "user": "root", "password": "该机的 root 密码" }
    },
    { "id": "la-1", "name": "洛杉矶", "group": "美国", "veid": "7654321", "api_key": "private_yyy" }
  ]
}
```

| 字段 | 说明 |
| --- | --- |
| `listen` | 监听地址,默认 `:8080` |
| `title` | 页面标题 |
| `password` | 访问密码。留空 = 开放访问;填值 = 需登录 |
| `poll_seconds` | 轮询间隔(秒),默认 60 |
| `shell.enabled` | 是否开启 Basic Shell,默认 `false`(命令以 root 身份在 VPS 内执行,故默认关闭) |
| `shell.timeout_seconds` | 单条命令超时(秒),默认 60,范围 5–600 |
| `allowed_origins` | 可选,SSH 终端 WebSocket 握手允许的浏览器 Origin,如 `["demo.example.com", "*.example.com"]`;留空 = 同源校验,`["*"]` = 不校验。只在反向代理改写了 Host 时才需要 |
| `instances[].id` | 可选,实例唯一标识,默认用 veid |
| `instances[].name` | 卡片显示名 |
| `instances[].group` | 可选,分组标签 |
| `instances[].veid` | KiwiVM VEID |
| `instances[].api_key` | KiwiVM API Key |
| `instances[].ssh.user` | SSH 用户名,默认 `root` |
| `instances[].ssh.password` | SSH 密码(账号密码登录只填这个即可) |
| `instances[].ssh.private_key` | 可选,内联 PEM 私钥(与 `password` 至少填一个;两个都填时先用密钥) |
| `instances[].ssh.private_key_file` | 可选,私钥文件路径;相对路径按 config.json 所在目录解析,启动时就读入并校验 |
| `instances[].ssh.passphrase` | 可选,加密私钥的口令 |
| `instances[].ssh.host` | 可选,默认取 KiwiVM 返回的第一个 IPv4 |
| `instances[].ssh.port` | 可选,默认取 KiwiVM 的 `ssh_port`(搬瓦工端口是随机的),再退到 22 |
| `instances[].ssh.host_key_fingerprint` | 可选,固定主机密钥指纹(`SHA256:…`);留空则首次连接时在终端里显示指纹供核对 |

> `ssh` 块是**每台机器各自配置**的:填了就出现 `>_ SSH 终端` 按钮,不填就只有 Basic Shell。

## 网页 SSH 终端(真 PTY)

给实例配上 `ssh` 凭据后,详情弹窗里会多出 **`>_ SSH 终端`**:浏览器里是一个完整的伪终端(xterm.js),服务端用 `golang.org/x/crypto/ssh` 连上 VPS,两边通过 WebSocket 对接。

- **真 PTY** —— `top`、`vim`、`htop`、`less`、tab 补全、颜色、`Ctrl+C`、后台任务、环境变量全都正常;窗口大小会自动同步到远端(`window-change`)。
- **凭据不出服务器** —— 浏览器只收发按键与屏幕字节,密码/私钥不会被下发到前端,也不会出现在 URL 或日志里。
- **地址与端口自动来** —— 默认用 KiwiVM 返回的 IPv4 与 `ssh_port`(搬瓦工的 SSH 端口是每台随机的),所以通常只要填 `user` + `password`;需要时可用 `host`/`port` 覆盖。
- **主机密钥校验** —— 配了 `host_key_fingerprint` 就严格校验,不匹配直接拒绝连接;没配则接受并在终端里打印指纹(首次使用请核对,之后写进配置固定下来)。
- **连接管理** —— 每台机器最多 3 个并发终端;30 分钟无输入无输出自动断开;每 30 秒一次 SSH keepalive + WebSocket ping 防 NAT 掉线;关掉窗口即断开远端 shell。
- **失败原因看得见** —— 认证失败、端口不通、没配凭据、终端数满 etc. 都会在终端里以红字给出中文说明(WebSocket 客户端读不到 HTTP 错误体,所以错误是走帧协议送的)。

> ⚠️ 这等于把 root 权限挂在看板后面。请务必设置 `password`,并且只在可信网络或带鉴权的反向代理后暴露;HTTPS 部署时浏览器会自动改用 `wss://`。

## Basic Shell(网页 Shell)

机器详情弹窗左下角的 `>_ Basic Shell` 会打开一个轻量终端:命令经 KiwiVM 的 `basicShell/exec` 在 VPS 内同步执行,**不需要开放 SSH 端口,也不需要在配置里存 SSH 密码或私钥**。适合没配 SSH 凭据的机器,或只想跑几条查询命令的场合。

- **默认关闭** —— 需显式设置 `shell.enabled: true`。开启后,能访问看板的人就等于拥有这些机器的 root 权限,所以请同时设置 `password`,并只在内网或带鉴权的反向代理后面暴露。未设密码而开启时,启动日志会打印警告。
- **不是 PTY** —— 每条命令都是一次独立的 API 调用:`top`/`vim`/`less` 等全屏交互程序用不了(`top -bn1 | head -15` 可以),环境变量、`export`、后台任务也不会带到下一条命令。需要这些就用上面的 SSH 终端。
- **`cd` 由看板代管** —— 单独的 `cd xxx` 走 `basicShell/cd`(失败自动回落到 `cd xxx && pwd` 让真 shell 解析),解析出的目录记在浏览器会话里,之后的命令自动加 `cd '<目录>' || exit 1;` 前缀;`cd a && ls` 这类复合命令照常执行,只是目录不会保留。
- **串行 + 限流** —— 同一台机器同时只执行一条命令,每分钟最多 60 条,避免打爆 KiwiVM 的 API 配额(可用 `getRateLimitStatus` 查看余量)。
- **退出码 vs API 错误** —— KiwiVM 把两者都塞在 `error` 字段里;本看板按「>255 视为 API 错误,≤255 视为命令退出码」区分(实测认证失败是 `700005`)。
- **输出上限 512 KB**,超出截断并提示;ANSI 颜色码与 `\r\n` 会被清理干净。
- 前端支持 ↑↓ 历史、`Ctrl+C` 中止请求(命令可能仍在 VPS 上跑完)、`Ctrl+L` 或 `clear` 清屏,以及一排常用命令快捷键。

## 构建与运行

需要 Go 1.23+(依赖 `golang.org/x/crypto/ssh` 与 `github.com/coder/websocket`,首次构建需要能访问模块代理;产物仍是单个静态二进制,无 cgo)。

```bash
# 构建
go build -o bandwagon .

# 运行
./bandwagon -config config.json
```

浏览器打开 `http://localhost:8080`。

### 交叉编译(例如在本地编译 Linux 版本部署到服务器)

```bash
GOOS=linux GOARCH=amd64 go build -o bandwagon .
```

把 `bandwagon` 和 `config.json` 上传到服务器,`./bandwagon -config config.json` 即可。

> 用 systemd 部署时见 `deploy/bandwagon.service`。注意服务以 `User=nobody` 运行:如果用 `ssh.private_key_file`,私钥文件必须让该用户可读(`chown root:nobody` + `chmod 640`),否则启动就会报读取失败。

### 反向代理 / HTTPS(SSH 终端必读)

SSH 终端走 WebSocket,反向代理必须透传 `Upgrade`/`Connection` 头,并且**不要用默认的 60 秒读超时**掐断长连接:

```nginx
location / {
    proxy_pass         http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header   Host              $host;      # 保留 Host,同源校验才过得去
    proxy_set_header   Upgrade           $http_upgrade;
    proxy_set_header   Connection        "upgrade";
    proxy_set_header   X-Real-IP         $remote_addr;
    proxy_read_timeout 3600s;                        # 终端是长连接
    proxy_send_timeout 3600s;
    proxy_buffering    off;                          # 终端输出别被缓冲
}
```

如果代理确实改写了 `Host`(或你从别的域名访问),在配置里加白名单即可,不必关掉校验:

```json
{ "allowed_origins": ["demo.holmes.cn", "*.holmes.cn"] }
```

浏览器 → 看板这一段请尽量用 HTTPS(页面是 https 时前端会自动改用 `wss://`);否则按键是明文过网的。看板 → VPS 那一段始终是加密的 SSH。

## API

看板前端调用的 JSON 接口(密码启用时需登录态 cookie 或 `Authorization: Bearer <session>`):

| 方法 | 路径 | 说明                               |
| --- | --- |----------------------------------|
| `GET`  | `/api/auth/status` | 鉴权状态(无需登录)                       |
| `POST` | `/api/login` | `{password}` 登录                  |
| `POST` | `/api/logout` | 登出                               |
| `GET`  | `/api/instances` | 全部机器快照                           |
| `GET`  | `/api/instances/{id}` | 单台机器详情                           |
| `POST` | `/api/instances/{id}/action` | `{action: start / stop / restart}` |
| `POST` | `/api/instances/{id}/shell` | `{command, cwd}` 执行一条命令,返回 `{output, exit, cwd, truncated, elapsed_ms}`;`command` 为空表示握手(解析初始目录) |
| `GET`  | `/api/instances/{id}/ssh` | **WebSocket** 升级为 SSH 真终端。客户端发文本帧 `{"t":"in","d":"…"}` / `{"t":"resize","cols":N,"rows":M}`,服务端回二进制帧(终端原始字节)+ 文本控制帧 `{"t":"status|ready|err|exit"}` |
| `GET`  | `/vendor/*` | 内嵌的 xterm.js 等前端库(同源,非 CDN) |

WebSocket 帧协议(子协议名 `bw-ssh.v1`):

| 方向 | 帧类型 | 内容 |
| --- | --- | --- |
| 浏览器 → 服务器 | text | `{"t":"in","d":"ls -l\r"}` 按键;`{"t":"resize","cols":120,"rows":40}` 窗口变化 |
| 服务器 → 浏览器 | binary | 终端输出原始字节(不经 UTF-8 重编码,`cat` 二进制文件也不会坏) |
| 服务器 → 浏览器 | text | `{"t":"status","msg":"正在连接…"}` / `{"t":"ready","host","user","host_key","pinned"}` / `{"t":"err","msg"}` / `{"t":"exit","code":0}` |

## 说明与限制

- KiwiVM API 的查询类接口主要提供 **套餐规格 + 月度流量 + 运行状态**,并不直接给出实时 CPU / 内存 **使用率百分比**。本看板如实展示 API 能提供的数据(系统、套餐容量、流量用量、运行时长、负载)。想临时看一眼实时占用,用 **SSH 终端** 跑 `top`/`htop`,或用 Basic Shell 跑 `top -bn1 | head -15`;若要长期曲线,仍需在机器上部署 agent。
- 后台轮询对多台机器串行调用并加入间隔,以避免触发 KiwiVM 限流。`getLiveServiceInfo` 为尽力而为(用于补全 uptime / load / **ssh_port**),失败不影响主信息展示。SSH 终端的地址依赖这次采集,所以机器离线或首轮采集未完成时,可以在配置里写死 `ssh.host` / `ssh.port`。
- 会话 token 仅保存在内存中,进程重启后需要重新登录。
- 依赖:后端 `golang.org/x/crypto/ssh` + `github.com/coder/websocket`,前端 xterm.js(已 vendor 进仓库并内嵌)。仍是**纯 Go、无 cgo**,`GOOS=linux go build` 交叉编译照旧,产物仍是单个二进制(比原来大约 1–2 MB)。
- 浏览器 → 看板这一段如果是 HTTP(非 HTTPS),按键是明文传输的;公网部署请套 TLS 反向代理。看板 → VPS 这一段始终是加密的 SSH。

## 项目结构

```
bandwagon/
├── main.go                     # 入口
├── config.example.json         # 配置示例
├── web/
│   ├── embed.go                # //go:embed index.html + vendor/
│   ├── index.html              # 卡片网格 + 两种终端的前端(HTML+CSS+JS)
│   └── vendor/                 # xterm.js 及其 addon(MIT,已内嵌,见其中 README)
└── internal/
    ├── kiwivm/client.go        # KiwiVM API 客户端(含 basicShell)
    ├── webssh/                 # SSH ↔ WebSocket 桥
    │   ├── webssh.go           #   拨号、PTY、resize、主机密钥校验
    │   └── ws.go               #   帧协议、输出分块、空闲超时、keepalive
    └── dashboard/
        ├── config.go           # 配置加载与校验(含 ssh 块、私钥文件解析)
        ├── store.go            # 内存快照 + 后台轮询 + SSH 地址解析
        ├── shell.go            # Basic Shell(basicShell/exec + cd 代管 + 限流)
        ├── ssh.go              # SSH 终端路由 + 每机并发上限
        └── server.go           # HTTP 路由 + 鉴权 + 电源操作 + 静态资源
```

测试:`go test ./...`。SSH 相关测试用**进程内的假 sshd**(`internal/webssh/fake_sshd_test.go`)跑真实协议,不需要真机器;想手动/浏览器联调可以:

```bash
BW_FAKE_SSHD=1 BW_FAKE_SSHD_PORT=2222 \
  go test ./internal/webssh -run TestFakeSSHDStandalone -timeout 0 -count=1 -v
# 然后在 config.json 里把某台机器的 ssh.host/port 指向 127.0.0.1:2222,user=root,password=test1234
```
