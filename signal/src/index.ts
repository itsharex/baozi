// Baozi 跨网互传的信令服务：一个 Cloudflare Worker + 一个 Durable Object 房间。
//
// 它只干一件事：把两端各自的连接信息（SDP + ICE 候选地址）互相转发一次。
// 之后文件数据走 WebRTC 直连，永远不经过这里——它连一个文件字节都看不到。
//
// 为什么非要 Durable Object：WebSocket 必须让两端落在**同一个实例**里才能互相转发。
// 普通 Worker 是无状态的、每次请求可能被派到不同实例；DO 按短码 idFromName 取，
// 同一个短码一定落在同一个实例，两端才连得上。
//
// 免费额度够用：DO 每天 10 万请求、13,000 GB-s；一次传文件只用几个请求、实例活几秒。

interface Env {
  ROOM: DurableObjectNamespace;
}

import { RECEIVER_HTML } from "./receiver";

const CODE_RE = /^[0-9a-z]{6}$/;
// 房间建好后没人来，超过这个时间就作废，免得短码被占着不放。
const ROOM_TTL_MS = 10 * 60 * 1000;

export default {
  async fetch(req: Request, env: Env): Promise<Response> {
    const url = new URL(req.url);

    // 给 Baozi 做连通性探测用：填完 URL 点"测试"，就是打这个。
    if (url.pathname === "/health") {
      return Response.json({ ok: true, service: "baozi-signal" });
    }

    // 浏览器接收页：接收方不用装 Baozi，打开 /r?code=xxxxxx 就能收。
    // 页面里的 JS 直接连 /ws 走同一套信令，所以这里只负责发一个静态页面。
    if (url.pathname === "/r") {
      return new Response(RECEIVER_HTML, {
        headers: {
          "content-type": "text/html; charset=utf-8",
          // 改版之后要立刻生效，不让人拿到旧页面。
          "cache-control": "no-cache",
        },
      });
    }

    if (url.pathname === "/ws") {
      const code = (url.searchParams.get("code") ?? "").toLowerCase();
      const role = url.searchParams.get("role") ?? "";
      if (!CODE_RE.test(code)) return new Response("bad code", { status: 400 });
      if (role !== "sender" && role !== "receiver") {
        return new Response("bad role", { status: 400 });
      }
      return env.ROOM.get(env.ROOM.idFromName(code)).fetch(req);
    }

    return new Response("baozi-signal\n", { status: 200 });
  },
};

// Room 是一个短码对应的一次配对。两端各自挂一条 WebSocket，
// 收到的任何消息都原样转给对端；控制消息（谁进来/谁走了）也是 JSON。
export class Room {
  private sockets = new Map<string, WebSocket>();

  constructor(private state: DurableObjectState) {}

  async fetch(req: Request): Promise<Response> {
    if (req.headers.get("Upgrade") !== "websocket") {
      return new Response("expected websocket", { status: 426 });
    }

    const url = new URL(req.url);
    const role = url.searchParams.get("role")!;

    // 一个角色只能占一条连接：防止有人拿同一个码反复连进来把房间搅乱。
    if (this.sockets.has(role)) return new Response("role taken", { status: 409 });
    if (this.sockets.size >= 2) return new Response("room full", { status: 409 });

    const pair = new WebSocketPair();
    const client = pair[0];
    const server = pair[1];
    server.accept();
    this.sockets.set(role, server);

    // 纯转发：不解析、不落盘、不改内容。
    server.addEventListener("message", (ev) => {
      for (const [other, sock] of this.sockets) {
        if (other === role) continue;
        try {
          sock.send(ev.data);
        } catch {
          // 对端已经断了，等它自己的 close 事件去清理。
        }
      }
    });

    const leave = () => {
      this.sockets.delete(role);
      for (const sock of this.sockets.values()) {
        try {
          sock.send(JSON.stringify({ type: "peer_left" }));
        } catch {}
      }
    };
    server.addEventListener("close", leave);
    server.addEventListener("error", leave);

    // 队列里已经有对端在等了，通知它一声，省得它干等到超时。
    for (const [other, sock] of this.sockets) {
      if (other === role) continue;
      try {
        sock.send(JSON.stringify({ type: "peer_joined" }));
      } catch {}
    }

    return new Response(null, { status: 101, webSocket: client });
  }
}
