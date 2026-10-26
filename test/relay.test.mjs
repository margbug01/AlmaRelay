import test from 'node:test';
import assert from 'node:assert/strict';
import { once } from 'node:events';
import { createHash, randomBytes } from 'node:crypto';
import { WebSocket } from 'ws';
import { createRelay } from '../server.mjs';

const secret = randomBytes(32).toString('hex');
const routingKey = randomBytes(16).toString('base64url');
const tokenHash = createHash('sha256').update(routingKey).digest('hex');
const nextFrame = async ws => JSON.parse((await once(ws, 'message'))[0].toString());
const sendFrame = (ws, value) => ws.send(JSON.stringify(value));

async function setup(context, options = {}) {
  const relay = createRelay({ registrationKey: secret, ...options });
  relay.server.listen(0, '127.0.0.1');
  await once(relay.server, 'listening');
  const base = `ws://127.0.0.1:${relay.server.address().port}`;
  context.after(() => relay.stop());
  return { ...relay, base };
}
async function register(base, key = secret, hash = tokenHash) {
  const ws = new WebSocket(`${base}/agent?th=${hash}`);
  await once(ws, 'open');
  const reply = nextFrame(ws);
  sendFrame(ws, { t: 'register', tokenHash: hash, key, name: 'Test desktop' });
  return { ws, reply: await reply };
}
async function rejected(url, expectedStatus, options = {}) {
  const ws = new WebSocket(url, options);
  ws.on('error', () => {});
  const [, response] = await once(ws, 'unexpected-response');
  assert.equal(response.statusCode, expectedStatus);
  response.resume();
  ws.terminate();
}

const settings = { timeout: 10000 };
test('requires a strong registration key', settings, () => {
  assert.throws(() => createRelay({ registrationKey: 'short' }));
});
test('health check does not leak pairings', settings, async context => {
  const { base } = await setup(context);
  const response = await fetch(`${base.replace('ws:', 'http:')}/healthz`);
  assert.deepEqual(await response.json(), { ok: true });
});
test('wrong registration key is rejected', settings, async context => {
  const { base, stats } = await setup(context);
  const { ws, reply } = await register(base, 'wrong');
  assert.equal(reply.t, 'error');
  assert.equal((await once(ws, 'close'))[0], 1008);
  assert.equal(stats().agents, 0);
});
test('registration query cannot disagree with frame', settings, async context => {
  const { base } = await setup(context);
  const ws = new WebSocket(`${base}/agent?th=${tokenHash}`);
  await once(ws, 'open');
  const closed = once(ws, 'close');
  sendFrame(ws, { t: 'register', tokenHash: 'a'.repeat(64), key: secret });
  assert.equal((await closed)[0], 1008);
});
test('rejects unknown routes, plaintext and foreign browser origins', settings, async context => {
  const { base } = await setup(context, { publicOrigin: 'https://relay.example.com' });
  await rejected(`${base}/c/${routingKey}/__alma_e2e__`, 503);
  await rejected(`${base}/c/${routingKey}/api/settings`, 404);
  await rejected(`${base}/agent?th=${tokenHash}`, 403, { origin: 'https://other.example.com' });
  await rejected(`${base}/agent?th=bad`, 400);
});
test('registration timeout and malformed frames do not leave agents', settings, async context => {
  const { base, stats } = await setup(context, { registrationTimeout: 30 });
  const idle = new WebSocket(`${base}/agent?th=${tokenHash}`);
  await once(idle, 'open');
  assert.equal((await once(idle, 'close'))[0], 1008);
  const invalid = new WebSocket(`${base}/agent?th=${tokenHash}`);
  await once(invalid, 'open');
  const closed = once(invalid, 'close');
  invalid.send('null');
  assert.equal((await closed)[0], 1008);
  assert.equal(stats().agents, 0);
});
test('forwards text and binary, queues early handshake, isolates phones', settings, async context => {
  const { base, stats } = await setup(context);
  const { ws: desktop, reply } = await register(base);
  assert.equal(reply.t, 'registered');
  const opening = nextFrame(desktop);
  const phone = new WebSocket(`${base}/c/${routingKey}/__alma_e2e__`);
  await once(phone, 'open');
  const request = await opening;
  assert.equal(request.path, '/__alma_e2e__');
  phone.send('early handshake');
  const pending = nextFrame(desktop);
  sendFrame(desktop, { t: 'wopened', id: request.id });
  assert.equal((await pending).s, 'early handshake');
  const incoming = once(phone, 'message');
  sendFrame(desktop, { t: 'wmsg', id: request.id, s: 'handshake response' });
  const [text, isBinary] = await incoming;
  assert.equal(text.toString(), 'handshake response');
  assert.equal(isBinary, false);
  const nextOpen = nextFrame(desktop);
  const second = new WebSocket(`${base}/c/${routingKey}/__alma_e2e__`);
  await once(second, 'open');
  const secondRequest = await nextOpen;
  assert.notEqual(secondRequest.id, request.id);
  sendFrame(desktop, { t: 'wopened', id: secondRequest.id });
  const payload = randomBytes(4096);
  const upstream = nextFrame(desktop);
  second.send(payload);
  const binary = await upstream;
  assert.equal(binary.id, secondRequest.id);
  assert.deepEqual(Buffer.from(binary.b, 'base64'), payload);
  const downstream = once(second, 'message');
  sendFrame(desktop, { t: 'wmsg', id: secondRequest.id, b: binary.b });
  const [returned, binaryFlag] = await downstream;
  assert.equal(binaryFlag, true);
  assert.deepEqual(returned, payload);
  const closeFrame = nextFrame(desktop);
  second.close();
  assert.equal((await closeFrame).t, 'wclose');
  assert.equal(stats().tunnels, 1);
});
test('replacement closes old desktop and tunnels without deleting new desktop', settings, async context => {
  const { base, stats } = await setup(context);
  const first = await register(base);
  const opening = nextFrame(first.ws);
  const phone = new WebSocket(`${base}/c/${routingKey}/__alma_e2e__`);
  await once(phone, 'open');
  const request = await opening;
  sendFrame(first.ws, { t: 'wopened', id: request.id });
  const oldClosed = once(first.ws, 'close');
  const phoneClosed = once(phone, 'close');
  const replacement = await register(base);
  assert.equal(replacement.reply.t, 'registered');
  assert.equal((await oldClosed)[0], 4000);
  await phoneClosed;
  assert.equal(stats().agents, 1);
  assert.equal(stats().tunnels, 0);
});
test('desktop loss closes phones', settings, async context => {
  const { base, stats } = await setup(context);
  const { ws } = await register(base);
  const opening = nextFrame(ws);
  const phone = new WebSocket(`${base}/c/${routingKey}/__alma_e2e__`);
  await once(phone, 'open');
  await opening;
  const closed = once(phone, 'close');
  ws.terminate();
  await closed;
  assert.equal(stats().agents, 0);
  assert.equal(stats().tunnels, 0);
});
test('limits phones and cancels unacknowledged tunnel opens', settings, async context => {
  const { base, stats } = await setup(context, { maxTunnels: 1, openTimeout: 100 });
  const { ws } = await register(base);
  const opening = nextFrame(ws);
  const phone = new WebSocket(`${base}/c/${routingKey}/__alma_e2e__`);
  await once(phone, 'open');
  await opening;
  const closed = once(phone, 'close');
  await rejected(`${base}/c/${routingKey}/__alma_e2e__`, 429);
  await closed;
  assert.equal(stats().tunnels, 0);
});
test('heartbeat tolerates delayed pongs but removes silent connections', settings, async context => {
  const { base, stats } = await setup(context, { heartbeatInterval: 20, heartbeatTimeout: 200 });
  const ws = new WebSocket(`${base}/agent?th=${tokenHash}`, { autoPong: false });
  let respond = true;
  ws.on('ping', data => {
    if (respond) setTimeout(() => { if (ws.readyState === WebSocket.OPEN) ws.pong(data); }, 60);
  });
  await once(ws, 'open');
  const reply = nextFrame(ws);
  sendFrame(ws, { t: 'register', tokenHash, key: secret });
  await reply;
  await new Promise(resolve => setTimeout(resolve, 350));
  assert.equal(ws.readyState, WebSocket.OPEN);
  respond = false;
  const closed = once(ws, 'close');
  await closed;
  const cleanupDeadline = Date.now() + 500;
  while (stats().agents !== 0 && Date.now() < cleanupDeadline) {
    await new Promise(resolve => setTimeout(resolve, 5));
  }
  assert.equal(stats().agents, 0);
});
test('APNs is explicitly unsupported, not falsely acknowledged', settings, async context => {
  const { base } = await setup(context);
  const { ws } = await register(base);
  const response = nextFrame(ws);
  sendFrame(ws, { t: 'push', id: 'test-push', notifications: [] });
  assert.deepEqual(await response, { t: 'pushed', id: 'test-push', unsupported: true, results: [] });
});
test('oversized messages are rejected', settings, async context => {
  const { base } = await setup(context, { maxPayload: 1024 });
  const { ws } = await register(base);
  const closed = once(ws, 'close');
  ws.send('x'.repeat(2048));
  const [code] = await closed;
  assert.ok(code === 1009 || code === 1006);
});
