# mcp-qbittorrent

[English](README.md) | 简体中文

[![License: GPL v3](https://img.shields.io/badge/license-GPLv3-blue.svg)](COPYING)

[OpenPlayX](https://github.com/openplayx/openplayx) 的服务器端 BitTorrent 下载服务，提供 MCP 服务和 HTTP API（原名 *bittorrent-server*）。它在锁定版本的 **qbittorrent-nox 5.1.0** 引擎之上提供一套受限的 HTTP API。OpenPlayX App 通过宿主注册的 AI 工具调用它：添加磁力链接 / `.torrent` 下载、查看进度、选择文件、暂停、恢复、限速和移除任务。下载在服务器上进行，App 关闭后也会继续。

## 它做什么（以及不做什么）

- 只接受用户提供的 `magnet:` 链接和上传的 `.torrent` 文件。不接入搜索站、索引器，不接受远程种子地址（`xs`/`as`）或本地路径。
- 每一个嵌套地址（tracker、web seed、peer 提示）都会按网络策略检查，而不只是检查外层协议。不允许的地址（私有地址、回环地址——例如已失效、现在解析到 127.0.0.1 的 tracker 域名——或不支持的协议）会被去掉并在 `removed_addresses` 中报告，磁力链接/种子的其余部分照常使用。`.torrent` 文件会被重写、去掉这些地址（info 字典不变，所以 info hash 不变）。只有无效输入（没有 info hash、`xs`/`as` 远程获取）才会被拒绝。
- 调用方使用逻辑上的 `destination_ref` 和服务任务 ID。引擎凭据、SID、完整磁力链接和绝对路径永远不会返回给调用方。
- 通过 API 添加的每个种子都下载到目标目录下自己的 `opx-*` 目录；移除时默认保留文件，除非另一个单独预览过的计划要求删除（此时引擎只删除该种子自己的文件）。
- **无状态**：服务不保存任何任务信息。每次读取都实时访问引擎；引擎中的每个种子（包括在 WebUI 中添加的）都会被列出且可控制，以引擎的 40 位十六进制 hash 标识。计划和移除确认都是签名 token（密钥按进程生成，所以重启后失效）；上传的 `.torrent` 文件在内存中保留 `input_ttl_seconds`。文件选择/暂停意图记录在引擎标签里（`opx-pending`、`opx-auto`、`opx-preset-video`、`opx-hold-*`）。引擎不可用时，读取请求返回 `503 engine_unavailable`。

## 目录结构

```text
src/cmd/server/           API 入口；另有 `new-token` 和 `engine-password-hash` 子命令
src/internal/api/         鉴权、限流、请求/响应、错误
src/internal/torrents/    无状态服务：输入、签名计划、实时视图、控制、标签自动化
src/internal/qbittorrent  qBittorrent 5.x WebUI API 适配器（锁定版本）
src/internal/files/       授权的文件引用，防符号链接的读取/删除
src/internal/webui/       原生 WebUI 的受保护反向代理
src/api/                  OpenAPI 3.1（由 scripts/gen-openapi.py 生成）和 /api/docs 页面
deploy/                   Compose、API 和引擎镜像、配置模板
scripts/                  构建、部署、健康检查、回滚、端到端测试
src/test/fakeqb/          API 测试使用的进程内模拟引擎
src/test/e2e/             基于真实 qbittorrent-nox 的 Docker 端到端测试
docs/                     运维、App 集成
```

## 快速开始

依赖：宿主机上的 Docker 和 Compose；构建二进制需要 Go 1.26+。

```bash
go run ./src/cmd/server new-token my-client     # 生成 API token 及其 secrets.json 条目
scripts/build.sh linux/amd64                # 镜像用的 dist/bittorrent-server-linux-amd64
```

然后在 `deploy/` 中：由 `env.example` 创建 `.env`，由 `config/config.example.json` 创建 `config/config.json`，由 `config/credentials.example.json` 创建 `config/secrets.json`，再用 `scripts/deploy.sh` 部署。[docs/operations.md](docs/operations.md)（英文）详细介绍了每一步、端口和引擎的安全设置。用 `curl -s http://<host>:<API_PORT>/api/readyz` 检查结果。

如果不只在私有局域网内使用，请通过 HTTPS 提供 API（内置 TLS 或反向代理）。

## 开发与测试

```bash
go test -race ./...          # 单元测试 + 基于模拟引擎的 API 测试
scripts/e2e.sh               # Docker 中的真实 qbittorrent-nox（隔离网络）
scripts/build.sh linux/amd64 # 静态二进制输出到 dist/
```

`scripts/e2e.sh` 会构建两个镜像，生成一次性凭据，启动一个隔离的环境（被测引擎没有外网路由，关闭 DHT/PeX/LSD），用另一个引擎充当做种方和 tracker 来创建 v1/v2/hybrid 测试种子，并逐项检查全部验收项。它只删除自己的 compose 项目和工作目录。

## URL 布局

同一个端口同时提供两部分：

| 路径 | 由谁提供 |
|---|---|
| `/api/v1/…`、`/api/healthz`、`/api/readyz`、`/api/docs`、`/api/openapi.json` | 本服务（Bearer token） |
| 其余所有路径，包括 `/` 和 WebUI 自己的 `/api/v2/…` | 经受保护代理转发的 qBittorrent 原生 WebUI（`webui.enabled`） |

不启用 WebUI 时，`/` 重定向到 `/api/docs`。

## API 概览

| 方法与路径 | 用途 |
|---|---|
| `GET /api/healthz`、`GET /api/readyz` | 存活检查；数据库 + 引擎就绪 |
| `GET /api/v1/capabilities`、`GET /api/v1/destinations` | 引擎版本、限制、下载目标 |
| `POST /api/v1/torrent-inputs` | 上传 `.torrent` → 短期有效的 `input_ref` |
| `POST /api/v1/torrent-plans` | 校验磁力链接/输入 + 目标 → 有时效的计划 |
| `POST /api/v1/torrents` | 按计划添加；新建返回 201，引擎中已存在返回 200 |
| `GET /api/v1/torrents[/{id}]`、`GET /api/v1/torrents/{id}/files` | 引擎中的全部种子（实时）；文件及选择版本 |
| `PATCH /api/v1/torrents/{id}/selection` | 基于 `files_revision` 修改文件选择 |
| `POST /api/v1/torrents/{id}/pause` / `resume` | 以引擎状态确认 |
| `PATCH /api/v1/torrents/{id}/limits` | 单任务速率，不超过管理员上限 |
| `POST /api/v1/torrents/{id}/remove-plan` / `remove` | 先预览，再执行 |
| `GET\|HEAD /api/v1/torrents/{id}/files/{file_id}/content` | 已完成的文件，支持 Range |

完整定义：`src/api/openapi.json`，在 `/api/docs` 渲染。

## MCP

`POST /api/mcp` 提供 `list_destinations`、`create_plan`、`start_download`、`list_downloads`、`get_download`、`pause`、`resume`、`create_remove_plan` 和 `remove`。下载和移除都保持两步（先生成计划，再用 `plan_ref` 执行）；`.torrent` 文件通过 `POST /api/v1/torrent-inputs` 上传，以 `input_ref` 传给 `create_plan`。

支持 Streamable HTTP 的一个子集：通过 `POST` 发送单条 JSON-RPC 2.0 消息，以 `application/json` 应答（不用 SSE，服务器不主动发消息）；无状态（不发 `Mcp-Session-Id`）；支持 `initialize`、`ping`、`tools/list`、`tools/call` 和通知；协议版本 2025-11-25、2025-06-18 和 2025-03-26。每个请求都需要与 HTTP API 相同的 Bearer token；来自外部 `Origin` 的请求会被拒绝。业务失败返回 `isError: true` 的工具结果，错误码与 HTTP API 相同。MCP 层调用的是与 HTTP 处理器相同的服务代码（没有第二份状态，也不回环请求自己）。

## WebUI

用户在浏览器中打开服务器根地址，用引擎的 WebUI 账号（`QBT_USERNAME` / 引擎密码）登录，而不是 API token。代理（`src/internal/webui`）会：

- 按浏览器访问的主机检查 Origin/Referer，再改写 Host/Origin/Referer，使引擎自身的 Host 和 CSRF 检查保持开启；
- 拒绝全局设置（`app/setPreferences`）、关闭程序、搜索插件、RSS 和种子制作——这些按钮在界面上会显示错误；
- 添加时只接受磁力链接和 `.torrent` 文件；保存路径必须在下载根目录内，并且不能在 API 的下载目标目录中；
- 按客户端 IP 限制登录失败次数（引擎自身的封禁已关闭，因为所有浏览器都通过代理的地址访问它）。

WebUI 和 API 操作的是同一个引擎：任何一边的改动都会立即在另一边看到。

## 在 MCP 客户端中使用

```json
{
  "mcpServers": {
    "qbittorrent": {
      "type": "http",
      "url": "https://bt.example.com/api/mcp",
      "headers": { "Authorization": "Bearer <token>" }
    }
  }
}
```

在 OpenPlayX 中，App 的 BitTorrent 设置和 AI 工具直接使用 HTTP API，见 [docs/app-integration.md](docs/app-integration.md)（英文）。

## 参与贡献

欢迎提 issue 和 PR，见 [CONTRIBUTING.md](CONTRIBUTING.md)（英文）。安全问题请按 [SECURITY.md](SECURITY.md)（英文）私下报告。

## 声明

本项目只管理用户自己添加的下载，不搜索、不托管、不分发内容。下载和分享什么由你自己负责（BitTorrent 在下载和做种时会上传数据）：只用于你有权获取的内容，并遵守适用于你的法律。本项目与 qBittorrent 没有关联。

## 许可证

GNU GPL v3，见 [COPYING](COPYING)。引擎的许可信息见 [deploy/LICENSES.md](deploy/LICENSES.md)。
