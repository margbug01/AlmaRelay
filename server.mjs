import http from 'node:http';
import { createHash, randomUUID, timingSafeEqual } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';
import { WebSocketServer, WebSocket } from 'ws';

const hash = value => createHash('sha256').update(value).digest();
const routeHash = value => hash(value).toString('hex');
const validHash = value => typeof value === 'string' && /^[a-f0-9]{64}$/.test(value);

export function createRelay({ registrationKey, publicOrigin, maxPayload = 16 * 1024 * 1024,
  maxBuffered = 32 * 1024 * 1024, maxConnections = 256, maxTunnels = 16,
  registrationTimeout = 5000, openTimeout = 10000, heartbeatInterval = 15000, heartbeatTimeout = 60000 } = {}) {
  if (typeof registrationKey !== 'string' || registrationKey.length < 24) {
    throw new Error('RELAY_REGISTRATION_KEY must contain at least 24 characters');
  }
  const expectedKey = hash(registrationKey);
  const origin = publicOrigin ? new URL(publicOrigin).origin : null;
  const agents = new Map();
  const peers = new Set();
  const attempts = new Map();
  const wss = new WebSocketServer({ noServer: true, maxPayload, perMessageDeflate: false });
  let stopping = false;

  const server = http.createServer((request, response) => {
    response.setHeader('Cache-Control', 'no-store');
    response.setHeader('X-Content-Type-Options', 'nosniff');
    response.setHeader('Referrer-Policy', 'no-referrer');
    response.setHeader('Content-Security-Policy', "default-src 'none'; frame-ancestors 'none'");
    if (request.method === 'GET' && request.url === '/healthz') {
      response.writeHead(stopping ? 503 : 200, { 'Content-Type': 'application/json' });
      return response.end(JSON.stringify({ ok: !stopping }));
    }
    response.writeHead(request.url === '/' ? 200 : 404, { 'Content-Type': 'text/plain; charset=utf-8' });
    response.end(request.url === '/' ? 'Private Alma relay. Use the Alma mobile app with end-to-end encryption enabled.\n' : 'Not found. This relay only supports encrypted WebSocket tunnels.\n');
  });
  server.requestTimeout = 10000;
  server.headersTimeout = 10000;
  server.keepAliveTimeout = 5000;
  server.on('clientError', (_error, socket) => socket.destroy());

  function send(socket, data, binary = false) {
    if (socket.readyState !== WebSocket.OPEN) return false;
    if (Buffer.byteLength(data) > maxPayload || socket.bufferedAmount + Buffer.byteLength(data) > maxBuffered) {
      socket.terminate();
      return false;
    }
    socket.send(data, { binary }, error => { if (error) socket.terminate(); });
    return true;
  }
  const frame = (socket, value) => send(socket, JSON.stringify(value));
  function close(socket, code, reason) {
    if (socket.readyState === WebSocket.OPEN) socket.close(code, reason);
    const timer = setTimeout(() => socket.terminate(), 1000);
    timer.unref();
    socket.once('close', () => clearTimeout(timer));
  }
  function removeTunnel(slot, tunnel, notify = true, code = 1012) {
    if (!slot.tunnels.delete(tunnel.id)) return;
    clearTimeout(tunnel.timer);
    tunnel.queue.length = 0;
    if (notify) frame(slot.ws, { t: 'wclose', id: tunnel.id });
    close(tunnel.ws, code, 'Tunnel closed');
  }
  function removeAgent(slot) {
    if (agents.get(slot.hash) === slot) agents.delete(slot.hash);
    for (const tunnel of [...slot.tunnels.values()]) removeTunnel(slot, tunnel, false);
  }
  function reject(socket, status) {
    socket.end(`HTTP/1.1 ${status}\r\nConnection: close\r\nContent-Length: 0\r\n\r\n`);
  }

  server.on('upgrade', (request, socket, head) => {
    if (stopping || peers.size >= maxConnections) return reject(socket, '503 Service Unavailable');
    const address = request.socket.remoteAddress;
    const now = Date.now();
    let rate = attempts.get(address);
    if (!rate || now - rate.start > 60000) {
      rate = { start: now, count: 0 };
      attempts.set(address, rate);
    }
    if (++rate.count > 120) return reject(socket, '429 Too Many Requests');
    if (request.headers.origin && (!origin || request.headers.origin !== origin)) {
      return reject(socket, '403 Forbidden');
    }
    let url;
    try { url = new URL(request.url, 'http://relay.local'); }
    catch { return reject(socket, '400 Bad Request'); }
    const isAgent = url.pathname === '/agent';
    const route = /^\/c\/([A-Za-z0-9_-]{22})\/__alma_e2e__$/.exec(url.pathname);
    if (!isAgent && !route) return reject(socket, '404 Not Found');
    const tokenHash = isAgent ? url.searchParams.get('th') : routeHash(route[1]);
    if (!validHash(tokenHash)) return reject(socket, '400 Bad Request');
    const slot = isAgent ? null : agents.get(tokenHash);
    if (!isAgent && (!slot || slot.ws.readyState !== WebSocket.OPEN)) return reject(socket, '503 Service Unavailable');
    if (slot && slot.tunnels.size >= maxTunnels) return reject(socket, '429 Too Many Requests');
    wss.handleUpgrade(request, socket, head, ws => {
      peers.add(ws);
      ws.lastPong = Date.now();
      ws.on('pong', () => { ws.lastPong = Date.now(); });
      ws.on('error', error => {
        console.warn('Relay socket error', error.code || 'unknown');
        ws.terminate();
      });
      ws.once('close', code => {
        peers.delete(ws);
        console.info('Relay connection closed', isAgent ? 'desktop' : 'phone', code);
      });
      if (isAgent) attachAgent(ws, tokenHash);
      else attachPhone(ws, slot);
    });
  });

  function attachAgent(ws, tokenHash) {
    let slot;
    const timer = setTimeout(() => close(ws, 1008, 'Registration timeout'), registrationTimeout);
    timer.unref();
    ws.once('close', () => {
      clearTimeout(timer);
      if (slot) removeAgent(slot);
    });
    ws.on('message', (data, binary) => {
      let message;
      try { if (binary) throw new Error(); message = JSON.parse(data.toString()); }
      catch { return close(ws, 1008, 'Invalid protocol frame'); }
      if (!message || typeof message !== 'object' || Array.isArray(message)) return close(ws, 1008, 'Invalid protocol frame');
      if (!slot) {
        if (message.t !== 'register' || message.tokenHash !== tokenHash || typeof message.key !== 'string' ||
            message.key.length > 4096 || !timingSafeEqual(hash(message.key), expectedKey)) {
          frame(ws, { t: 'error', message: 'Registration denied' });
          return close(ws, 1008, 'Registration denied');
        }
        clearTimeout(timer);
        const previous = agents.get(tokenHash);
        if (previous) {
          removeAgent(previous);
          close(previous.ws, 4000, 'Another desktop took over this pairing');
        }
        slot = { ws, hash: tokenHash, tunnels: new Map() };
        agents.set(tokenHash, slot);
        frame(ws, { t: 'registered' });
        return;
      }
      if (agents.get(tokenHash) !== slot) return;
      if (message.t === 'push') {
        frame(ws, { t: 'pushed', id: typeof message.id === 'string' ? message.id.slice(0, 128) : '', unsupported: true, results: [] });
        return;
      }
      const tunnel = slot.tunnels.get(message.id);
      if (!tunnel) return;
      switch (message.t) {
        case 'wopened':
          if (tunnel.opened) return;
          tunnel.opened = true;
          clearTimeout(tunnel.timer);
          for (const queued of tunnel.queue) {
            if (!frame(ws, queued)) break;
          }
          tunnel.queue.length = 0;
          tunnel.queuedBytes = 0;
          break;
        case 'wmsg':
          if (!tunnel.opened) return removeTunnel(slot, tunnel);
          if (typeof message.s === 'string' && message.b === undefined) send(tunnel.ws, message.s);
          else if (typeof message.b === 'string' && message.s === undefined && message.b.length % 4 === 0 && /^[A-Za-z0-9+/]*={0,2}$/.test(message.b)) {
            send(tunnel.ws, Buffer.from(message.b, 'base64'), true);
          } else removeTunnel(slot, tunnel);
          break;
        case 'wclose':
          removeTunnel(slot, tunnel, false, 1000);
          break;
        case 'werr':
          removeTunnel(slot, tunnel, false, 1011);
          break;
      }
    });
  }

  function attachPhone(ws, slot) {
    const tunnel = { id: randomUUID(), ws, opened: false, queue: [], queuedBytes: 0 };
    slot.tunnels.set(tunnel.id, tunnel);
    tunnel.timer = setTimeout(() => removeTunnel(slot, tunnel), openTimeout);
    tunnel.timer.unref();
    ws.once('close', () => removeTunnel(slot, tunnel));
    ws.on('message', (data, binary) => {
      if (!slot.tunnels.has(tunnel.id)) return;
      const message = { t: 'wmsg', id: tunnel.id, ...(binary ? { b: data.toString('base64') } : { s: data.toString() }) };
      if (tunnel.opened) {
        if (!frame(slot.ws, message)) removeTunnel(slot, tunnel);
      } else {
        tunnel.queuedBytes += data.length;
        if (tunnel.queuedBytes > 65536 || tunnel.queue.length >= 8) return removeTunnel(slot, tunnel, true, 1009);
        tunnel.queue.push(message);
      }
    });
    if (!frame(slot.ws, { t: 'wopen', id: tunnel.id, path: '/__alma_e2e__' })) removeTunnel(slot, tunnel, false);
  }

  const heartbeat = setInterval(() => {
    for (const ws of peers) {
      if (Date.now() - ws.lastPong > heartbeatTimeout) {
        console.warn('Relay heartbeat timed out');
        ws.terminate();
        continue;
      }
      if (ws.readyState === WebSocket.OPEN) ws.ping();
    }
    const cutoff = Date.now() - 60000;
    for (const [address, rate] of attempts) if (rate.start < cutoff) attempts.delete(address);
  }, heartbeatInterval);
  heartbeat.unref();

  return {
    server,
    stats: () => ({ agents: agents.size, connections: peers.size, tunnels: [...agents.values()].reduce((total, slot) => total + slot.tunnels.size, 0) }),
    async stop() {
      stopping = true;
      clearInterval(heartbeat);
      for (const ws of peers) ws.terminate();
      await new Promise(resolve => wss.close(resolve));
      await new Promise(resolve => server.close(resolve));
    }
  };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    const key = process.env.RELAY_REGISTRATION_KEY_FILE
      ? readFileSync(process.env.RELAY_REGISTRATION_KEY_FILE, 'utf8').trim()
      : process.env.RELAY_REGISTRATION_KEY;
    const relay = createRelay({ registrationKey: key, publicOrigin: process.env.PUBLIC_ORIGIN });
    const port = Number(process.env.PORT || 8787);
    if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error('Invalid PORT');
    relay.server.listen(port, process.env.HOST || '127.0.0.1', () => console.log(`Private Alma relay listening on port ${port}`));
    relay.server.on('error', () => { console.error('Relay could not listen'); process.exitCode = 1; });
    for (const signal of ['SIGINT', 'SIGTERM']) process.once(signal, async () => { await relay.stop(); });
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
