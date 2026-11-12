// 浏览器接收页：对方不用装 Baozi，打开这个地址就能收文件。
//
// 为什么放在这儿：WebRTC 的 DataChannel 浏览器原生支持，Baozi 是发送方（它有文件、
// 也只有它能读磁盘），浏览器当接收方即可。于是"跨网传文件"对接收方是**零安装**的，
// 手机上也不用装 App——这正是局域网那条路一直在用的形态。
//
// 传输协议跟 internal/p2p/transfer.go 一模一样，改两边必须一起改：
//   1. 文本 {"type":"manifest","items":[{"path":"相对路径","size":字节}]}
//   2. 二进制 [4 字节小端文件序号][≤32KB 数据]
//   3. 文本 {"type":"done","total":字节}
//
// 信令也是同一套：连 /ws?code=<6 位>&role=receiver，等 sender 的 offer，回 answer。
// 两边都不做 trickle——把候选收全了再塞进 SDP 一次性发过去。

export const RECEIVER_HTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Baozi 跨网接收</title>
<style>
  :root { --fg:#1a1a1a; --muted:#666; --line:#e3e6ec; --bg:#f5f6f8; --accent:#2563eb; --ok:#16a34a; --bad:#dc2626; }
  * { box-sizing:border-box; }
  body { margin:0; padding:20px; background:var(--bg); color:var(--fg);
         font:14px/1.5 "Microsoft YaHei UI","Microsoft YaHei",system-ui,-apple-system,sans-serif; }
  .wrap { max-width:640px; margin:0 auto; }
  .card { background:#fff; border:1px solid var(--line); border-radius:14px; padding:20px; }
  h1 { font-size:18px; margin:0 0 4px; }
  .sub { color:var(--muted); font-size:12px; margin-bottom:18px; }
  .code { font:600 34px/1 ui-monospace,Consolas,monospace; letter-spacing:6px; color:var(--accent); }
  .state { display:flex; align-items:center; gap:8px; margin:16px 0; font-size:13px; }
  .dot { width:8px; height:8px; border-radius:50%; background:var(--muted); flex:none; }
  .dot.ok { background:var(--ok); } .dot.bad { background:var(--bad); } .dot.wait { background:var(--accent); }
  .bar { height:8px; background:#e8ebf0; border-radius:6px; overflow:hidden; margin:8px 0; }
  .fill { height:100%; width:0; background:var(--accent); transition:width .15s linear; }
  .list { margin-top:6px; }
  .row { display:flex; justify-content:space-between; gap:12px; padding:8px 0; border-top:1px solid var(--line); font-size:13px; }
  .row span:first-child { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .row span:last-child { color:var(--muted); flex:none; }
  .bytes { color:var(--muted); font-size:12px; }
  .hint { color:var(--muted); font-size:12px; margin-top:14px; }
  .err { color:var(--bad); font-size:13px; margin-top:10px; white-space:pre-wrap; }
  .savebox { margin-top:16px; }
  .savebox button { width:100%; padding:12px; font-size:15px; }
  .codebox { display:flex; gap:8px; margin-top:12px; }
  input.codein { flex:1; min-width:0; padding:10px 12px; border:1px solid var(--line); border-radius:10px;
                 font:16px/1 ui-monospace,Consolas,monospace; letter-spacing:3px; text-transform:lowercase; }
  button { border:0; border-radius:10px; padding:10px 16px; background:var(--accent); color:#fff;
           font-size:14px; cursor:pointer; }
  button[disabled] { opacity:.5; cursor:default; }
</style>
</head>
<body>
<div class="wrap">
  <div class="card">
    <h1>Baozi 跨网接收</h1>
    <div class="sub">浏览器直接收，不装任何东西。文件走 WebRTC 直连，不经过这个网站。</div>

    <div id="codeArea">
      <div class="code" id="codeText">------</div>
      <div class="codebox" id="codeForm" hidden>
        <input class="codein" id="codeInput" placeholder="对方给的 6 位码" maxlength="6" autocomplete="off">
        <button id="codeGo">开始接收</button>
      </div>
    </div>

    <div class="state"><span class="dot" id="dot"></span><span id="stateText">等待连接…</span></div>

    <div class="bar" id="barBox" hidden><div class="fill" id="fill"></div></div>
    <div class="bytes" id="bytes"></div>

    <div class="list" id="list"></div>

    <div class="savebox" id="savebox" hidden>
      <button id="saveBtn">保存文件</button>
    </div>

    <div class="hint" id="hint">
      保持这个页面开着，关掉就断了。收完会自动存到浏览器的下载目录；
      要是没看到文件，点上面的按钮再存一次。
    </div>
    <div class="err" id="err" hidden></div>
  </div>
</div>
<script>
(function () {
  "use strict";
  var CHUNK_LIMIT = 512 * 1024 * 1024; // 单文件超过这个量就别攒内存了
  var ICE_TIMEOUT_MS = 6000;           // 跟 Go 端的 gatherBudget（8s）留点余量
  var OPEN_TIMEOUT_MS = 60000;

  var el = function (id) { return document.getElementById(id); };
  var dot = el("dot"), stateText = el("stateText");
  var barBox = el("barBox"), fill = el("fill"), bytes = el("bytes");
  var list = el("list"), err = el("err"), hint = el("hint");
  var savebox = el("savebox"), saveBtn = el("saveBtn");

  var code = (new URL(location.href)).searchParams.get("code") || "";
  code = code.toLowerCase().replace(/[^0-9a-z]/g, "").slice(0, 6);

  function setState(text, kind) {
    stateText.textContent = text;
    dot.className = "dot" + (kind ? " " + kind : "");
  }
  function fail(text) {
    setState("连接失败", "bad");
    err.hidden = false;
    err.textContent = text;
  }
  function fmt(n) {
    if (n < 1024) return n + " B";
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
    if (n < 1024 * 1024 * 1024) return (n / 1048576).toFixed(1) + " MB";
    return (n / 1073741824).toFixed(2) + " GB";
  }

  // 没带短码就让人手输，省得非要抄一条长地址。
  if (!code || code.length !== 6) {
    el("codeText").textContent = "------";
    el("codeForm").hidden = false;
    setState("输入对方给你的 6 位码", "wait");
    el("codeGo").onclick = function () {
      var v = (el("codeInput").value || "").toLowerCase().replace(/[^0-9a-z]/g, "");
      if (v.length !== 6) { fail("短码是 6 位（数字和小写字母）"); return; }
      location.href = "/r?code=" + v;
    };
    el("codeInput").focus();
    return;
  }

  el("codeText").textContent = code;
  // 带了短码就直接干活，那个手输框不必留在 DOM 里。
  el("codeForm").remove();
  setState("正在连信令服务器…", "wait");

  var ws = new WebSocket(location.origin.replace(/^http/, "ws") + "/ws?code=" + code + "&role=receiver");
  var pc = null, dc = null, manifest = null, parts = [], received = 0, total = 0;
  var opened = false;
  // finished 是"已经收到 done"的标志。此后连接断掉是正常的（对方发完就退出了），
  // 不能再把 connectionState 的 failed 当成错误盖到"已完成"上面。
  var finished = false;

  ws.onopen = function () { setState("已连上信令，等对方进入房间…", "wait"); };
  ws.onerror = function () { fail("连不上信令服务器（" + location.host + "）"); };
  ws.onclose = function (ev) {
    if (!opened && !dc) { setState("信令连接已断开", "bad"); }
  };
  ws.onmessage = function (ev) {
    var m;
    try { m = JSON.parse(ev.data); } catch (_) { return; }
    if (m.type === "peer_joined") {
      setState("对方已进入房间，正在打洞…", "wait");
      return;
    }
    if (m.type === "peer_left") {
      if (!opened) setState("对方离开了房间", "bad");
      return;
    }
    if (m.type === "offer") { answer(m.sdp); return; }
  };

  function answer(sdp) {
    setState("收到对方的连接请求，正在回应…", "wait");
    pc = new RTCPeerConnection({ iceServers: [{ urls: "stun:stun.cloudflare.com:3478" }] });

    pc.ondatachannel = function (e) {
      dc = e.channel;
      dc.binaryType = "arraybuffer";
      dc.onopen = function () {
        opened = true;
        setState("已直连，开始接收", "ok");
      };
      dc.onmessage = onData;
      dc.onclose = function () {
        if (!manifest && !finished) setState("连接被对方关闭", "bad");
      };
    };
    pc.onconnectionstatechange = function () {
      if (finished) return; // 传完了断开是正常的，别再报错
      if (pc.connectionState === "failed") fail("两端网络打不通（对称 NAT 或防火墙拦了）");
      if (pc.connectionState === "connected" && !opened) setState("已连上，等数据通道打开…", "ok");
    };

    pc.setRemoteDescription({ type: "offer", sdp: sdp })
      .then(function () { return pc.createAnswer(); })
      .then(function (a) { return pc.setLocalDescription(a); })
      .then(function () { return waitIce(pc); })
      .then(function () {
        // 候选收全了再发：Go 端不做 trickle，它等的是一条完整的 answer。
        ws.send(JSON.stringify({ type: "answer", sdp: pc.localDescription.sdp }));
        setState("已回复，等待打洞完成…", "wait");
        setTimeout(function () {
          if (!opened) fail("60 秒没连上。多半是两端都在对称 NAT 后面，需要中继（还没做）");
        }, OPEN_TIMEOUT_MS);
      })
      .catch(function (e) { fail("协商失败：" + (e && e.message ? e.message : e)); });
  }

  function waitIce(pc) {
    return new Promise(function (resolve) {
      if (pc.iceGatheringState === "complete") return resolve();
      var finished = false;
      var done = function () { if (!finished) { finished = true; resolve(); } };
      pc.addEventListener("icegatheringstatechange", function () {
        if (pc.iceGatheringState === "complete") done();
      });
      setTimeout(done, ICE_TIMEOUT_MS);
    });
  }

  function onData(ev) {
    var d = ev.data;
    if (typeof d === "string") {
      var m;
      try { m = JSON.parse(d); } catch (_) { return; }
      if (m.type === "manifest") {
        manifest = m.items || [];
        parts = manifest.map(function () { return []; });
        total = manifest.reduce(function (a, it) { return a + (it.size || 0); }, 0);
        barBox.hidden = false;
        renderList();
        setState("接收中…", "ok");
      } else if (m.type === "done") {
        finished = true;
        received = total > 0 ? total : received;
        finish();
      }
      return;
    }
    // 二进制块：[4 字节小端序号][数据]
    var view = new DataView(d);
    if (d.byteLength < 4) return;
    var idx = view.getUint32(0, true);
    var chunk = d.slice(4);
    if (!parts[idx]) parts[idx] = [];
    parts[idx].push(chunk);
    received += chunk.byteLength;
    if (total > 0) {
      fill.style.width = Math.min(100, (received / total) * 100) + "%";
      bytes.textContent = fmt(received) + " / " + fmt(total);
    }
  }

  function renderList() {
    list.innerHTML = "";
    manifest.forEach(function (it) {
      var row = document.createElement("div");
      row.className = "row";
      var name = document.createElement("span");
      name.textContent = it.path;
      var size = document.createElement("span");
      size.textContent = fmt(it.size || 0);
      row.appendChild(name); row.appendChild(size);
      list.appendChild(row);
    });
  }

  function finish() {
    if (!manifest || !manifest.length) { fail("对方没发清单，什么也没收到"); return; }
    setState("接收完成，等保存", "ok");
    fill.style.width = "100%";
    bytes.textContent = fmt(received) + "（" + manifest.length + " 个文件）";
    var big = manifest.some(function (it) { return (it.size || 0) > CHUNK_LIMIT; });
    if (big) {
      hint.textContent = "文件较大，浏览器是先把它攒在内存里再存盘的。这个页面适合中小文件，" +
        "大文件建议用装了 Baozi 的那台机器接收（那边是边收边写，不吃内存）。";
    }
    saveBtn.textContent = manifest.length > 1 ? "保存 " + manifest.length + " 个文件" : "保存文件";
    savebox.hidden = false;
    // 先自动存一次：桌面浏览器都接（实测 Chrome 会自动落到下载目录）。
    // 有些环境（手机浏览器、公司策略）会拦掉无手势的下载，那就由用户点按钮，
    // 所以按钮一直留着，点了再存一遍。
    doSave();
  }

  // 逐个触发下载。多文件时 Chrome 会问一次"是否允许下载多个文件"，允许后全部落盘。
  function doSave() {
    if (!manifest) return;
    manifest.forEach(function (it, i) {
      var blob = new Blob(parts[i] || [], { type: "application/octet-stream" });
      var a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = baseName(it.path);
      document.body.appendChild(a);
      a.click();
      setTimeout(function () { URL.revokeObjectURL(a.href); a.remove(); }, 1000);
    });
    setState("已保存到浏览器的下载目录", "ok");
  }

  saveBtn.onclick = function () {
    saveBtn.textContent = "再存一次 · 共 " + fmt(received);
    doSave();
  };

  // 传输用的 path 可能带一层文件夹，下载时只取最后一段文件名。
  function baseName(p) {
    var s = String(p || "file").replace(/\\\\/g, "/");
    var seg = s.split("/");
    var last = seg[seg.length - 1] || "file";
    return last.replace(/[:*?"<>|]/g, "_");
  }
})();
</script>
</body>
</html>`;
