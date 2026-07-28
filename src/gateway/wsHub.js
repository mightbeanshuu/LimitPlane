// wsHub — a WebSocket server from scratch (RFC 6455), zero dependencies.
//
// SSE pushes one way over plain HTTP. A WebSocket is a REAL two-way socket
// that starts life as an HTTP request and then "upgrades": the client sends
// a random key, the server answers with SHA1(key + a magic GUID) in base64,
// and from that moment the TCP socket stops speaking HTTP and starts
// speaking framed WebSocket messages. That handshake dance and the frame
// format below are the entire protocol — worth seeing raw once, because
// after this, socket.io and friends are just conveniences on top.
//
// We only need server -> client pushes (dashboards listen, they don't chat),
// so we implement: handshake, sending text frames, and just enough frame
// PARSING to answer pings and notice closes. That's a complete, spec-legal
// one-way hub in ~100 lines.

import { createHash } from "node:crypto";

const MAGIC_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"; // fixed by the RFC, not a secret

export function computeAccept(secWebSocketKey) {
  return createHash("sha1").update(secWebSocketKey + MAGIC_GUID).digest("base64");
}

// Wrap a JSON-able message in a WebSocket text frame (server frames are
// unmasked). The only fiddly part is the length: 7 bits if it fits, else an
// escape value (126/127) followed by the real length in 16 or 64 bits.
export function encodeTextFrame(str) {
  const payload = Buffer.from(str);
  const len = payload.length;
  let header;
  if (len < 126) {
    header = Buffer.from([0x81, len]); // 0x81 = FIN + text opcode
  } else if (len < 65536) {
    header = Buffer.alloc(4);
    header[0] = 0x81;
    header[1] = 126;
    header.writeUInt16BE(len, 2);
  } else {
    header = Buffer.alloc(10);
    header[0] = 0x81;
    header[1] = 127;
    header.writeBigUInt64BE(BigInt(len), 2);
  }
  return Buffer.concat([header, payload]);
}

export function createWsHub({ authorize } = {}) {
  const sockets = new Set();

  // Wire this to the http server: server.on("upgrade", hub.handleUpgrade)
  function handleUpgrade(req, socket) {
    const url = new URL(req.url, "http://x");
    if (authorize && !authorize(req, url)) {
      socket.write("HTTP/1.1 401 Unauthorized\r\n\r\n"); // no token, no socket
      return socket.destroy();
    }
    const key = req.headers["sec-websocket-key"];
    if (!key) return socket.destroy();

    // The handshake: prove we speak WebSocket by hashing their key.
    socket.write(
      "HTTP/1.1 101 Switching Protocols\r\n" +
        "Upgrade: websocket\r\n" +
        "Connection: Upgrade\r\n" +
        `Sec-WebSocket-Accept: ${computeAccept(key)}\r\n\r\n`
    );

    sockets.add(socket);
    const drop = () => {
      sockets.delete(socket);
      socket.destroy();
    };
    socket.on("close", drop);
    socket.on("error", drop);

    // Minimal inbound parsing: answer pings, honor closes, ignore the rest.
    socket.on("data", (buf) => {
      const opcode = buf[0] & 0x0f;
      if (opcode === 0x8) drop(); // close frame
      if (opcode === 0x9) socket.write(Buffer.from([0x8a, 0x00])); // ping -> pong
    });
  }

  // Push one typed message to every connected dashboard.
  function broadcast(type, data) {
    if (sockets.size === 0) return;
    const frame = encodeTextFrame(JSON.stringify({ type, data }));
    for (const s of sockets) s.write(frame);
  }

  return { handleUpgrade, broadcast, count: () => sockets.size };
}
