import test from 'node:test';
import assert from 'node:assert/strict';
import { openSync, readSync, closeSync } from 'node:fs';
import { createHash, randomBytes, webcrypto } from 'node:crypto';
import { runInNewContext } from 'node:vm';
import { once } from 'node:events';
import http from 'node:http';
import { WebSocket, WebSocketServer } from 'ws';
import { createRelay } from '../server.mjs';

function loadInstalledClients(archive) {
  const descriptor = openSync(archive, 'r');
  let source;
  try {
    const header = Buffer.alloc(16);
    readSync(descriptor, header, 0, 16, 0);
    const json = Buffer.alloc(header.readUInt32LE(12));
    readSync(descriptor, json, 0, json.length, 16);
    const entry = JSON.parse(json).files.out.files.main.files['index.js'];
    const content = Buffer.alloc(entry.size);
    readSync(descriptor, content, 0, content.length, 8 + header.readUInt32LE(4) + Number(entry.offset));
    source = content.toString();
  } finally { closeSync(descriptor); }
  function section(start, end) {
    const first = source.indexOf(start);
    const last = source.indexOf(end, first);
    assert.ok(first >= 0 && last > first, `Installed bundle markers changed: ${start}`);
    return source.slice(first, last);
  }
  const crypto = 'const ' + section('pg="P-256"', 'const Og=');
  const desktop = section('class py{', 'function wy()');
  const client = section('const OK=new TextEncoder;class RK{', 'function DK(');
  const context = {
    crypto: webcrypto, Buffer, TextEncoder, TextDecoder, Uint8Array, ArrayBuffer, DataView,
    btoa, atob, setTimeout, clearTimeout, setInterval, clearInterval, fetch, Headers, Response,
    ReadableStream, AbortController, DOMException, URL, console,
    rn: WebSocket, St: createHash, va: () => undefined,
    Vg: () => undefined, hy: () => undefined,
    Rg: '/ws/handoff/tool-approvals', Qg: '/ws/handoff/questions'
  };
  return runInNewContext(`${crypto}\n${desktop}\n${client}\n({ Desktop: yy, Client: RK, routingKey: Tg })`, context, { timeout: 3000 });
}

const archive = process.env.ALMA_ASAR;
test('installed Alma 0.4.211: real E2E handshake, HTTP, streams, upload, WS and reconnect', {
  skip: !archive && 'Set ALMA_ASAR to the local installed app.asar', timeout: process.env.ALMA_TEST_RELAY_URL ? 120000 : 30000
}, async context => {
  const { Desktop, Client, routingKey } = loadInstalledClients(archive);
  const key = process.env.ALMA_TEST_RELAY_URL ? process.env.ALMA_TEST_RELAY_KEY : randomBytes(32).toString('hex');
  assert.ok(key && key.length >= 24, 'External relay test requires its registration key');
  const code = randomBytes(32).toString('base64url');
  const payloadSize = Number(process.env.ALMA_TEST_PAYLOAD_BYTES || 1024 * 1024);
  assert.ok(Number.isInteger(payloadSize) && payloadSize >= 256 && payloadSize <= 4 * 1024 * 1024);
  const payload = randomBytes(payloadSize);
  const local = http.createServer(async (request, response) => {
    if (request.url === '/api/test') {
      response.writeHead(200, { 'Content-Type': 'application/json' });
      response.end(JSON.stringify({ actualLocalFixture: true }));
    } else if (request.url === '/api/upload') {
      const chunks = [];
      for await (const chunk of request) chunks.push(chunk);
      response.end(createHash('sha256').update(Buffer.concat(chunks)).digest('hex'));
    } else if (request.url === '/api/download') {
      response.end(payload);
    } else if (request.url === '/api/stream') {
      response.writeHead(200, { 'Content-Type': 'text/event-stream' });
      response.write('data: first\n\n');
      setTimeout(() => response.end('data: second\n\n'), 30);
    } else { response.writeHead(404); response.end(); }
  });
  const localSockets = new WebSocketServer({ server: local });
  localSockets.on('connection', socket => socket.on('message', (data, binary) => socket.send(data, { binary })));
  local.listen(0, '127.0.0.1');
  await once(local, 'listening');
  const relay = process.env.ALMA_TEST_RELAY_URL ? null : createRelay({ registrationKey: key });
  if (relay) {
    relay.server.listen(0, '127.0.0.1');
    await once(relay.server, 'listening');
  }
  const base = process.env.ALMA_TEST_RELAY_URL || `http://127.0.0.1:${relay.server.address().port}`;
  const desktop = new Desktop({ serverUrl: base, connectCode: code, registrationKey: key,
    e2eEncryption: true, localPort: local.address().port, deviceName: 'Isolated compatibility test' });
  const clients = [];
  context.after(async () => {
    for (const client of clients) client.close();
    desktop.stop();
    for (const socket of localSockets.clients) socket.terminate();
    await relay?.stop();
    await new Promise(resolve => localSockets.close(resolve));
    local.closeAllConnections();
    await new Promise(resolve => local.close(resolve));
  });
  async function connectedDesktop() {
    const deadline = Date.now() + 5000;
    while (desktop.getStatus().state !== 'connected') {
      assert.ok(Date.now() < deadline, 'Desktop did not register');
      await new Promise(resolve => setTimeout(resolve, 10));
    }
  }
  desktop.start();
  await connectedDesktop();
  context.diagnostic('Desktop registered');
  const client = new Client({ serverUrl: base, code, WebSocketImpl: WebSocket });
  clients.push(client);
  await client.connect();
  assert.equal(client.getState(), 'ready');
  context.diagnostic('E2E handshake ready');
  assert.deepEqual(await (await client.fetch('/api/test')).json(), { actualLocalFixture: true });
  assert.equal(await (await client.fetch('/api/upload', { method: 'POST', body: payload })).text(), createHash('sha256').update(payload).digest('hex'));
  assert.deepEqual(Buffer.from(await (await client.fetch('/api/download')).arrayBuffer()), payload);
  context.diagnostic(`HTTP and ${payloadSize} byte upload/download verified`);
  const stream = (await client.fetch('/api/stream')).body.getReader();
  const first = await stream.read();
  assert.equal(new TextDecoder().decode(first.value), 'data: first\n\n');
  const second = await stream.read();
  assert.equal(new TextDecoder().decode(second.value), 'data: second\n\n');
  assert.equal((await stream.read()).done, true);
  const socket = client.openWebSocket('/ws/test');
  await new Promise((resolve, reject) => { socket.onopen = resolve; socket.onerror = reject; });
  const text = new Promise(resolve => { socket.onmessage = event => resolve(event.data); });
  socket.send('encrypted hello');
  assert.equal(await text, 'encrypted hello');
  const binary = new Promise(resolve => { socket.onmessage = event => resolve(event.data); });
  socket.send(payload.subarray(0, 256));
  assert.deepEqual(Buffer.from(await binary), payload.subarray(0, 256));
  socket.close();
  client.close();
  desktop.reconnect();
  await connectedDesktop();
  const reconnected = new Client({ serverUrl: base, code, WebSocketImpl: WebSocket });
  clients.push(reconnected);
  await reconnected.connect();
  assert.equal((await reconnected.fetch('/api/test')).status, 200);
  const knownRoute = await routingKey(code);
  class ForcedRouteSocket extends WebSocket {
    constructor(url) { super(url.replace(/\/c\/[^/]+\//, `/c/${knownRoute}/`)); }
  }
  const wrongCode = new Client({ serverUrl: base, code: randomBytes(32).toString('base64url'), WebSocketImpl: ForcedRouteSocket });
  clients.push(wrongCode);
  await assert.rejects(wrongCode.connect(), /Handshake authentication failed/);
  assert.equal((await reconnected.fetch('/api/test')).status, 200);
  context.diagnostic('Real installed desktop/responder/initiator classes passed; isolated local fixture only, no real settings or conversation data accessed.');
});
