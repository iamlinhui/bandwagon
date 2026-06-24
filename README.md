# VPS看板 (Bandwagon Dashboard)

一个基于搬瓦工 **KiwiVM API** 的多机器监控看板,用 **Go 单二进制** 实现,前端是自包含的卡片网格(无需任何前端构建、无需 CDN)。

监控多台 BandwagonHost VPS 的运行状态、IP、系统、套餐、**月度流量**(带进度条)、运行时长等,并支持在看板上直接 **启动 / 停止 / 重启**。

## 功能

- 🗂 **多实例** —— 在一份配置里管理任意数量的 VPS
- 🟢 **状态总览** —— 在线 / 离线 / 错误 计数 + 全局流量汇总
- 📊 **卡片网格** —— 每台机器一张卡片,高亮流量用量(接近上限自动变黄/红)
- 🔍 **搜索 / 分组** —— 按名称、IP、节点、机房、分组过滤
- ⚡ **电源操作** —— 启动 / 停止 / 重启(危险操作二次确认)
- 🔐 **可选密码登录** —— 部署到公网时给看板加一把锁
- 🔄 **后台轮询** —— 默认每 60 秒采集一次,前端自动刷新
- 📦 **单二进制** —— HTML 嵌入二进制,拷贝一个文件即可部署

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
  "instances": [
    { "id": "tokyo-1", "name": "东京节点", "group": "亚洲", "veid": "1234567", "api_key": "private_xxx" },
    { "id": "la-1",    "name": "洛杉矶",   "group": "美国", "veid": "7654321", "api_key": "private_yyy" }
  ]
}
```

| 字段 | 说明 |
| --- | --- |
| `listen` | 监听地址,默认 `:8080` |
| `title` | 页面标题 |
| `password` | 访问密码。留空 = 开放访问;填值 = 需登录 |
| `poll_seconds` | 轮询间隔(秒),默认 60 |
| `instances[].id` | 可选,实例唯一标识,默认用 veid |
| `instances[].name` | 卡片显示名 |
| `instances[].group` | 可选,分组标签 |
| `instances[].veid` | KiwiVM VEID |
| `instances[].api_key` | KiwiVM API Key |

## 构建与运行

需要 Go 1.22+。

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

## API

看板前端调用的 JSON 接口(密码启用时需登录态 cookie 或 `Authorization: Bearer <session>`):

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET`  | `/api/auth/status` | 鉴权状态(无需登录) |
| `POST` | `/api/login` | `{password}` 登录 |
| `POST` | `/api/logout` | 登出 |
| `GET`  | `/api/instances` | 全部机器快照 |
| `GET`  | `/api/instances/{id}` | 单台机器详情 |
| `POST` | `/api/instances/{id}/action` | `{action: start|stop|restart}` |

## 说明与限制

- KiwiVM API 的查询类接口主要提供 **套餐规格 + 月度流量 + 运行状态**,并不直接给出实时 CPU / 内存 **使用率百分比**。本看板如实展示 API 能提供的数据(系统、套餐容量、流量用量、运行时长、负载)。若需要实时 CPU/内存曲线,需要额外通过 SSH 采集或在机器上部署 agent。
- 后台轮询对多台机器串行调用并加入间隔,以避免触发 KiwiVM 限流。`getLiveServiceInfo` 为尽力而为(用于补全 uptime / load),失败不影响主信息展示。
- 会话 token 仅保存在内存中,进程重启后需要重新登录。

## 项目结构

```
bandwagon/
├── main.go                     # 入口
├── config.example.json         # 配置示例
├── web/
│   ├── embed.go                # //go:embed index.html
│   └── index.html              # 卡片网格前端(HTML+CSS+JS)
└── internal/
    ├── kiwivm/client.go        # KiwiVM API 客户端
    └── dashboard/
        ├── config.go           # 配置加载与校验
        ├── store.go            # 内存快照 + 后台轮询
        └── server.go           # HTTP 路由 + 鉴权 + 电源操作
```
