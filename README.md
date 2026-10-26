# Private Alma Relay

独立实现的 Alma 加密中继。运行时不依赖 Alma 官方中继、账号服务或官方服务端源码；没有复制官方服务端。只依赖 Node.js 和 `ws`，不需要数据库。

本版本对照 Windows Alma **0.4.211** 的客户端协议实现。默认只允许端到端加密隧道，不支持关闭加密后的旧版 HTTP 代理，也不提供网页聊天界面。手机使用原有 Alma App。

## 已实现

- 桌面注册、注册密钥验证、每 15 秒心跳、60 秒无 pong 后断线清理、同配对桌面接管（关闭码 4000）。
- `/agent?th=…` 与 `/c/<routing-key>/__alma_e2e__` 双向 WebSocket 转发。
- 握手文本和加密二进制帧原样转发，多手机会话隔离；HTTP、流式输出、文件和 WebSocket 由两端在加密隧道内承载。
- 注册/打开隧道超时、连接数限制、缓冲限制、消息大小限制、升级请求限速、浏览器 Origin 检查。
- 不落盘聊天数据，不输出配对链接、消息、注册密钥或请求路径。

## 边界

- **没有 iPhone 后台 APNs 推送**：官方 App 的推送需要其开发者签名凭据。服务会明确回复 `unsupported`，不会假报成功，也不会转交官方服务。手机前台聊天与后台通知是两件事。
- **不是 AI API 中继**：不会改变模型供应商速度。只替换手机/远程端与桌面的通信路径。
- **单进程、单实例**：注册映射只存在内存，重启后桌面重连即可恢复；不要直接启动多个副本并随机负载均衡。
- 限制：256 条连接、每桌面 16 条隧道、单 WebSocket 消息 16 MiB、每连接发送队列 32 MiB；桌面帧封装含 base64 膨胀，因此单个手机二进制消息的实际上限低于 16 MiB（约 12 MiB）。大文件是否可传取决于客户端是否分块，不承诺无限大小。超限断开而不是无限缓存。
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

## 已有反代 / 直接 Node

```bash
npm ci --omit=dev --ignore-scripts
node --env-file=.env server.mjs
```

默认监听 `127.0.0.1:8787`，使用 systemd 等进程管理器常驻，反代同一台机器的这个地址。需要设置环境变量时：

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

```bash
npm ci --ignore-scripts
npm test
npm audit --omit=dev --registry=https://registry.npmjs.org
```

普通测试不依赖 Alma。可选兼容测试直接在内存读取自己安装包的协议类（不复制到发布文件，不启动整套 Alma，不读取用户配置），连接隔离的假 API 服务：

```bash
ALMA_ASAR='/path/to/Alma/resources/app.asar' npm test
```

真实类测试覆盖加密握手、HTTP 请求、1 MiB 上传/下载逐字节核对、分段 SSE、内部 WebSocket 文本/二进制、桌面重新连接、错误加密码拒绝。用的是安装包里的桌面端、加密 responder 和远程 initiator 类，**不是手机原生 App 的真机测试**。特殊 handoff 路由处理器在测试中未加载，普通 HTTP/WS 代理和加密类使用原实现。


## 文件

- `server.mjs`：独立中继实现。
- `test/relay.test.mjs`：协议、隔离、鉴权、资源限制测试。
- `test/compat.test.mjs`：可选的已安装 Alma 协议兼容测试。
- `Dockerfile` / `compose.yaml` / `Caddyfile`：部署配置。
- `.env.example`：配置示例，刻意不包含有效密钥。

无管理后台、账号系统、遥测、持久数据库和自动更新。先把私人转发这件事做好。
