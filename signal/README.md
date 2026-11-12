# Baozi 跨网互传的信令服务

一个 Cloudflare Worker + Durable Object。它只做一件事：把两端的连接信息
（`sdp` + `ice candidates`）互相转发一次，之后文件数据走 WebRTC 直连，
**不经过这个服务**。

## 为什么需要它

两台机器都躲在 NAT 后面，彼此不知道对方在公网的地址，也没法主动敲门。
所以需要一个双方都够得着的公共点，交换一次"名片"。这就是信令。

## 免费额度够不够

- Durable Objects：免费计划可用（仅 SQLite 后端），每天 10 万请求、13,000 GB-s
- 一次传文件：`/health` 1 次 + 两条 WebSocket（存活几秒）+ 中转几条 JSON
- 结论：日常使用连 1% 都用不到

STUN 用的是 Cloudflare 的 `stun.cloudflare.com:3478`，免费且不限量；
打不通时的 TURN 兜底走 Cloudflare Realtime TURN（每月前 1,000 GB 免费），
需要在账号里生成凭证后才启用。

## 部署

```powershell
cd D:\work\baozi\signal
npm install
npx wrangler login      # 打开浏览器，用你的 Cloudflare 账号授权
npx wrangler deploy
```

部署成功后会打印一个地址，形如：

```
https://baozi-signal.<你的子域>.workers.dev
```

把这个地址填进 Baozi 的互传设置（跨网那一栏），或者写进 `config.json`：

```json
{ "share": { "signal": "https://baozi-signal.xxx.workers.dev" } }
```

## 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/health` | 连通性探测，返回 `{"ok":true}` |
| WS | `/ws?code=<6位>&role=sender\|receiver` | 进房间；同码的 sender 与 receiver 配成一对 |

消息是纯转发：Baozi 发了什么，对端就收到什么。服务自己只发两种控制消息：

```json
{"type":"peer_joined"}
{"type":"peer_left"}
```

## 安全边界

- 短码是 6 位 `0-9a-z`（约 22 亿种），房间 10 分钟内没人来就作废
- 服务不存储任何内容，也不看 SDP 里写了什么
- 真正的身份校验在 WebRTC 的 DTLS 指纹里：SDP 里带的证书指纹对不上，握手就失败
