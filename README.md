# Private Alma Relay

独立实现的 Alma 加密中继。运行时不依赖 Alma 官方中继、账号服务或官方服务端源码；没有复制官方服务端。Go 写的单个静态二进制，只依赖 `gorilla/websocket`，不需要数据库，也不需要 Node 运行时（Node 只用来跑对照安装包的兼容测试）。

**生产服务是纯 Go，仓库包含可选的 JavaScript 兼容测试。** GitHub 的语言占比会统计 `test/compat.test.mjs`；部署和运行中继不需要 Node/npm，Docker 最终镜像也只包含 Go 二进制。

本版本对照 Windows Alma **0.4.211** 的客户端协议实现。默认只允许端到端加密隧道，不支持关闭加密后的旧版 HTTP 代理，也不提供网页聊天界面。手机使用原有 Alma App。

## 已实现

- 桌面注册、注册密钥验证、每 15 秒心跳、60 秒无 pong 后断线清理、同配对桌面接管（关闭码 4000）。
- `/agent?th=…` 与 `/c/<routing-key>/__alma_e2e__` 双向 WebSocket 转发。
- 握手文本和加密二进制帧原样转发，多手机会话隔离；HTTP、流式输出、文件和 WebSocket 由两端在加密隧道内承载。
- 注册/打开隧道超时、连接数限制、缓冲限制、消息大小限制、升级请求限速、浏览器 Origin 检查。
- 手机超限只关它自己那条隧道（1009），不会连累桌面和其他手机；桌面发不动时手机上传会被暂停（背压），而不是把桌面断开。
- 桌面这一段支持 permessage-deflate：桌面帧是 JSON 里装 base64 的密文，压缩后线上字节约等于原始密文字节，基本消除 base64 约 33% 的体积膨胀，相比未压缩传输减少约 25% 的字节（具体取决于消息大小和内容）。手机那段本来就是密文，不压缩。Alma 桌面默认就会提议压缩，不用改设置。
- 不落盘聊天数据，不输出配对链接、消息、注册密钥或请求路径。

## 边界

- **没有 iPhone 后台 APNs 推送**：官方 App 的推送需要其开发者签名凭据。服务会明确回复 `unsupported`，不会假报成功，也不会转交官方服务。手机前台聊天与后台通知是两件事。
- **不是 AI API 中继**：不会改变模型供应商速度。只替换手机/远程端与桌面的通信路径。
- **单进程、单实例**：注册映射只存在内存，重启后桌面重连即可恢复；不要直接启动多个副本并随机负载均衡。
- 限制：256 条连接、每桌面 16 条隧道、单 WebSocket 消息 16 MiB、每连接发送队列 32 MiB。手机发往桌面的消息要包成 base64，所以手机单条消息上限是 12,582,864 字节（约 12 MiB），超过时只关闭这条手机隧道；桌面发往手机的二进制解码后自然不超过这个数。大文件是否可传取决于客户端是否分块，不承诺无限大小。
- 每 TCP 来源每分钟最多 120 次 WebSocket 升级；反代后所有用户可能共用此额度，适合私人使用，不适合直接当公共服务。
- 聊天隧道保留客户端自己的加密，服务仍可见连接元数据和路由标识。桌面单独发送的 APNs 通知帧不属于加密隧道，会收到但被丢弃、不存储。
- 路由标识和配对链接都应当保密，关闭反代访问日志；有人掌握路由标识仍可能占用连接配额。注册密钥不是配对码：前者防止陌生桌面注册，后者用于客户端的端到端认证。

## 部署：独立 VPS + Docker Compose

适用于服务器的 **80/443 未被其他服务占用**。若已有 nginx/Caddy/宝塔，不要直接启动本文件里的 Caddy，更不要停掉现有服务；只部署 relay，然后接入已有反代。

1. 准备域名，A/AAAA 指向服务器。没有 IPv6 就别配 AAAA。为了避开原来的线路，建议先 DNS-only，不套 Cloudflare 代理。
2. 上传本目录到服务器。安装 Docker Engine 和 Compose 插件，开放 TCP 80/443。
3. 生成 `.env`，以下命令不会把密钥打印到终端，域名请换成自己的：

```bash
umask 077
printf 'RELAY_DOMAIN=relay.example.com\nRELAY_REGISTRATION_KEY=' > .env
openssl rand -hex 32 >> .env
chmod 600 .env
docker compose config --quiet
docker compose up -d --build
```

4. 检查 `docker compose ps`：relay 应为 healthy。请求 `https://你的域名/healthz` 应返回 `{"ok":true}`。这只证明 HTTP/TLS 通了，还要完成手机实际配对验证。
5. 桌面 Alma 的手机连接设置里选择「使用自建中继」，填 `https://你的域名`（不要加 `/agent` 或 `/c/`）。注册令牌填写 `.env` 的 `RELAY_REGISTRATION_KEY` 值，不沿用官方账号令牌。保持端到端加密开启，应用后重新扫码配对手机。
6. 手机先用移动网络发一条消息，确认流式返回、切后台再回前台能重连，再测一次附件。桌面必须保持运行且联网。

Caddy 会自动申请和续期证书，relay 的 8787 端口仅在 Compose 内部开放。不要把 Docker API、Alma 本地 API 或 8787 直接暴露到公网。

更新：保留 `.env`，替换源文件后 `docker compose up -d --build`。重启会中断正在进行的请求；不要在长任务过程中发布。恢复官方中继可在 Alma 设置里切回，再重新配对。

## 已有反代 / 直接运行二进制

自行编译需要 **Go 1.24 或更高版本**（以 `go.mod` 为准）；Docker 构建阶段使用 Go 1.27，不要求宿主机安装 Go。以下命令使用 Bash / Git Bash 语法，为 Linux x86-64 服务器构建：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o alma-relay .
```

ARM64 服务器将 `GOARCH=amd64` 改为 `GOARCH=arm64`。生产构建关闭 CGO，不需要 C 编译器，目标服务器也不需要安装 Go 或 Node。

把 `alma-relay` 放到服务器（例如 `/opt/alma-relay/alma-relay`），用 `deploy/alma-relay.service` 常驻，配置放 `/etc/alma-relay.env`。默认监听 `127.0.0.1:8787`，反代同一台机器的这个地址。`alma-relay -healthcheck` 请求本机 `/healthz`，成功退出码为 0。需要设置环境变量时：

| 变量 | 含义 |
| --- | --- |
| `RELAY_REGISTRATION_KEY` | 必填，至少 24 字符；建议 32 字节随机十六进制 |
| `RELAY_REGISTRATION_KEY_FILE` | 可选，从受限文件读取密钥，优先于环境变量 |
| `HOST` | 默认 `127.0.0.1`；容器里为 `0.0.0.0` |
| `PORT` | 默认 `8787` |
| `PUBLIC_ORIGIN` | 可选，例如 `https://relay.example.com`；允许同 Origin 浏览器连接，不设置则拒绝带 Origin 的连接，原生客户端不受影响 |

已有 Caddy 增加站点：

```caddyfile
relay.example.com {
    reverse_proxy 127.0.0.1:8787
}
```

已有 nginx 的站点需要正常 TLS、WebSocket Upgrade、关闭代理缓冲、长读超时。若 nginx 在容器里，`127.0.0.1` 指它自己的容器，应该用共享网络中的 relay 服务名；不要照抄成宿主机地址。

```nginx
location / {
    proxy_pass http://127.0.0.1:8787;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_buffering off;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
    access_log off;
}
```

## 测试和验证范围

普通单元测试只需要 Go，不需要 Node 或安装 Alma：

```bash
CGO_ENABLED=0 go test ./...
```

并发竞态检查使用以下命令，需要当前平台支持 Go race detector、启用 CGO，并在 PATH 中提供可用的 C 编译器（如 GCC/Clang；Windows 使用兼容的 MinGW-w64 工具链）：

```bash
CGO_ENABLED=1 go test -race ./...
```

这是测试工具的要求，不代表生产服务依赖 CGO。

普通测试覆盖：协议、隔离、鉴权、资源限制、心跳、超大消息只关一条隧道、慢桌面背压、桌面段压缩率、解压后大小上限。

可选兼容测试会编译并启动 Go 版中继，直接在内存读取自己安装包的协议类（不复制到发布文件，不启动整套 Alma，不读取用户配置），连接隔离的假 API 服务，并确认安装包的桌面类协商了压缩：

需要 **Node.js 22+、npm、Go**，并确保 `go` 在 PATH 中。`ALMA_ASAR` 请替换为自己安装的 Alma `resources/app.asar` 路径；测试依赖版本相关的安装包标记，Alma 升级后可能需要适配。以下示例使用 Bash / Git Bash：

```bash
npm ci --ignore-scripts
ALMA_ASAR='/path/to/Alma/resources/app.asar' npm test
```

真实类测试覆盖加密握手、HTTP 请求、1 MiB 上传/下载逐字节核对、分段 SSE、内部 WebSocket 文本/二进制、桌面重新连接、错误加密码拒绝。用的是安装包里的桌面端、加密 responder 和远程 initiator 类，**不是手机原生 App 的真机测试**。特殊 handoff 路由处理器在测试中未加载，普通 HTTP/WS 代理和加密类使用原实现。

验证记录：Go 单元测试及竞态检查、对照 Alma 0.4.211 的兼容测试，以及经正式域名的 1 MiB 加密全流程和真实桌面隧道验证已通过。这些是已有测试结果，不代表当前服务状态，也不保证后续 Alma 版本兼容。手机真机配对、iOS 后台行为、长期稳定性和 Docker 镜像运行仍未验证。


## 文件

- `relay.go`：中继实现；`main.go`：环境变量、监听、信号和健康检查。
- `relay_test.go`：协议、隔离、鉴权、资源限制测试。
- `test/compat.test.mjs`：可选的已安装 Alma 协议兼容测试（Node）。
- `Dockerfile` / `compose.yaml` / `Caddyfile`：部署配置。
- `.env.example`：配置示例，刻意不包含有效密钥。

无管理后台、账号系统、遥测、持久数据库和自动更新。先把私人转发这件事做好。
