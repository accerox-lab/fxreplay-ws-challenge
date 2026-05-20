// Smoke test: confirms cross-pod broadcast works through the ingress.
// Opens N WebSocket connections (the Service spreads them across pods via
// kube-proxy round-robin), sends one message from client #0, asserts every
// other client received the broadcast within WAIT_MS.
//
// Run after `make up`:
//   npm install ws    # one-time, in this directory
//   node hack/broadcast-test.js

const WebSocket = require('ws');

const N = Number(process.env.N || 5);
const URL = process.env.URL || 'ws://127.0.0.1/ws';
const HOST_HEADER = process.env.HOST_HEADER || 'ws.local.test';
const WAIT_MS = Number(process.env.WAIT_MS || 1500);

(async () => {
  const clients = [];
  for (let i = 0; i < N; i++) {
    const ws = new WebSocket(URL, { headers: { Host: HOST_HEADER } });
    ws.received = [];
    ws.on('message', (data) => ws.received.push(data.toString()));
    await new Promise((res, rej) => { ws.once('open', res); ws.once('error', rej); });
    clients.push(ws);
  }
  console.log(`opened ${N} clients on ${URL} (Host: ${HOST_HEADER})`);

  const tag = `hello-${Date.now()}`;
  clients[0].send(tag);
  console.log(`sent "${tag}" from client[0]`);
  await new Promise(r => setTimeout(r, WAIT_MS));

  let pass = 0, fail = 0;
  for (let i = 0; i < N; i++) {
    const hit = clients[i].received.some(m => m.includes(tag));
    if (hit) { pass++; console.log(`  client[${i}]: GOT broadcast`); }
    else     { fail++; console.log(`  client[${i}]: MISSING broadcast`); }
  }
  for (const c of clients) c.close();
  console.log(`\nresult: ${pass}/${N} received  ${fail === 0 ? 'PASS' : 'FAIL'}`);
  process.exit(fail === 0 ? 0 : 1);
})().catch(e => { console.error(e); process.exit(2); });
