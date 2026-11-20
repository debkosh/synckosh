// Devices syncing through a real Go server (test/server), and a Go device (test/device) reading
// what a TypeScript device wrote and writing back.
import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join as path } from 'node:path';
import { createInterface } from 'node:readline';
import {
  Client, MemoryLocal, Replica, create, join, between, newGUID, DataKey, HasKeyError, PassphraseError,
  RejectedError, KeysCollection,
} from '../dist/synckosh.js';

const repo = new URL('../..', import.meta.url).pathname;
const bin = mkdtempSync(path(tmpdir(), 'synckosh-js-'));
for (const cmd of ['server', 'device']) {
  execFileSync('go', ['build', '-o', path(bin, cmd), './js/test/' + cmd], { cwd: repo, stdio: 'inherit' });
}
const server = spawn(path(bin, 'server'), { stdio: ['ignore', 'pipe', 'inherit'] });
const url = await new Promise((resolve, reject) => {
  createInterface({ input: server.stdout }).once('line', resolve);
  server.once('exit', (code) => reject(new Error(`server exited ${code}`)));
});

let n = 0;
async function test(name, fn) {
  try {
    await fn();
    n++;
  } catch (e) {
    console.error(`sync: ${name}: ${e.stack}`);
    server.kill();
    rmSync(bin, { recursive: true, force: true });
    process.exit(1);
  }
}

const passphrase = 'correct horse battery staple';
let buckets = 0;
const bucket = () => `bucket${++buckets}-${Date.now()}`;
async function device(token, key, more = {}) {
  const client = new Client({ url, token });
  return Replica.open({ client, local: new MemoryLocal(), key, ...more });
}
const texts = async (r, collection) => (await r.items(collection)).map((it) => it.private?.text).sort();

await test('two devices create, join and see each other\'s items, sealed', async () => {
  const token = bucket();
  const keyA = await create(new Client({ url, token }), passphrase, 'Main passphrase');
  await assert.rejects(create(new Client({ url, token }), passphrase, 'again'), HasKeyError);
  await assert.rejects(join(new Client({ url, token }), 'wrong passphrase'), PassphraseError);
  const keyB = await join(new Client({ url, token }), passphrase);
  assert.equal(keyB.id, keyA.id);
  assert.deepEqual(keyB.bytes(), keyA.bytes());

  const a = await device(token, keyA), b = await device(token, keyB);
  const [milk] = await a.put(
    { guid: newGUID(), collection: 'todos', datatype: 'todo', public: { order: between('', '') }, private: { text: 'milk' } },
    { guid: newGUID(), collection: 'todos', datatype: 'todo', private: { text: 'eggs' } },
  );
  await a.sync();
  await b.sync();
  assert.deepEqual(await texts(b, 'todos'), ['eggs', 'milk']);
  assert.deepEqual((await b.get('todos', milk.guid)).public, { order: 'V' });

  await b.put({ ...(await b.get('todos', milk.guid)), private: { text: 'oat milk' } });
  const eggs = (await b.items('todos')).find((it) => it.private.text === 'eggs');
  await b.delete('todos', eggs.guid);
  await b.sync();
  await a.sync();
  assert.deepEqual(await texts(a, 'todos'), ['oat milk']);
  assert.equal(await a.get('todos', eggs.guid), undefined); // removed once the server holds the delete

  // The server holds only sealed private parts.
  const raw = await new Client({ url, token }).pull('', 0);
  assert.ok(raw.items.length >= 3);
  for (const it of raw.items) assert.deepEqual(Object.keys(it.private).sort().filter((k) => k !== 'public'), ['box', 'key']);
  const wire = JSON.stringify(raw);
  assert.ok(!wire.includes('milk') && !wire.includes('eggs'));
});

await test('a backup code joins a device, and its copy can be removed alone', async () => {
  const token = bucket();
  const key = await create(new Client({ url, token }), passphrase, 'Main');
  const a = await device(token, key);
  const { code, copy } = await key.backupCode('Backup code');
  const [stored] = await a.put(copy);
  await a.sync();
  assert.equal((await join(new Client({ url, token }), code)).id, key.id);
  await a.delete(KeysCollection, stored.guid);
  await a.sync();
  await assert.rejects(join(new Client({ url, token }), code), PassphraseError);
  assert.equal((await join(new Client({ url, token }), passphrase)).id, key.id);
});

await test('mergers join offline edits; without one the newest wins', async () => {
  const token = bucket();
  const key = await create(new Client({ url, token }), passphrase, 'Main');
  const union = (l, r) => ({ deleted: false, private: { tags: [...new Set([...l.private.tags, ...r.private.tags])].sort() } });
  const a = await device(token, key, { mergers: { tagset: union } });
  const b = await device(token, key, { mergers: { tagset: union } });
  const [set] = await a.put({ guid: newGUID(), collection: 'notes', datatype: 'tagset', private: { tags: ['x'] } });
  const [note] = await a.put({ guid: newGUID(), collection: 'notes', datatype: 'note', private: { text: 'one' } });
  await a.sync();
  await b.sync();
  await a.put({ ...set, private: { tags: ['x', 'a'] } }, { ...note, private: { text: 'from a' } });
  await b.put({ ...(await b.get('notes', set.guid)), private: { tags: ['x', 'b'] } });
  await b.put({ ...(await b.get('notes', note.guid)), private: { text: 'from b' } });
  await a.sync();
  await b.sync();
  await a.sync();
  for (const r of [a, b]) {
    assert.deepEqual((await r.get('notes', set.guid)).private.tags, ['a', 'b', 'x']);
    assert.equal((await r.get('notes', note.guid)).private.text, 'from b');
  }
});

await test('a delete-before reaches every device, and pushes fill to the limits', async () => {
  const token = bucket();
  const key = await create(new Client({ url, token }), passphrase, 'Main');
  const a = await device(token, key, { limits: { pushItems: 2 } }), b = await device(token, key);
  for (let i = 0; i < 5; i++) await a.put({ guid: newGUID(), collection: 'history', datatype: 'visit', private: { text: 'old' + i } });
  await a.sync();
  await b.sync();
  assert.equal((await b.items('history')).length, 5);
  await new Promise((r) => setTimeout(r, 5));
  await b.deleteBefore('history', Date.now());
  await b.put({ guid: newGUID(), collection: 'history', datatype: 'visit', private: { text: 'new' } });
  await b.sync();
  await a.sync();
  assert.deepEqual(await texts(a, 'history'), ['new']);
  // The server forgot them too: a new device gets only the new one.
  const c = await device(token, key);
  await c.sync();
  assert.deepEqual(await texts(c, 'history'), ['new']);
});

await test('after the server loses a bucket, the devices push everything back', async () => {
  const token = bucket();
  const key = await create(new Client({ url, token }), passphrase, 'Main');
  const a = await device(token, key);
  await a.put({ guid: newGUID(), collection: 'todos', datatype: 'todo', private: { text: 'kept' } });
  await a.sync();
  const reset = await fetch(`${url}/test/reset?bucket=${token}`, { method: 'POST' });
  assert.equal(reset.status, 200);
  await a.sync();
  const b = await device(token, key);
  await b.sync();
  assert.deepEqual(await texts(b, 'todos'), ['kept']);
});

await test('items under another key are locked, forged ones rejected', async () => {
  const token = bucket();
  const key = await create(new Client({ url, token }), passphrase, 'Main');
  const a = await device(token, key);
  await a.sync();
  const raw = new Client({ url, token });
  const base = { guid: newGUID(), collection: 'todos', datatype: 'todo', deleted: false, timestamp: Date.now() };
  await raw.push([DataKey.create().seal({ ...base, guid: newGUID(), private: { text: 'other key' } })]);
  const forged = key.seal({ ...base, guid: newGUID(), private: { text: 'real' } });
  await raw.push([{ ...forged, timestamp: forged.timestamp + 1 }]);
  await raw.push([{ ...base, guid: newGUID(), private: { text: 'plain' } }]);
  await assert.rejects(a.sync(), (e) => e instanceof RejectedError && e.items.length === 2);
  assert.deepEqual(await texts(a, 'todos'), []);
  assert.equal((await a.locked('todos')).length, 1);
});

await test('a Go device reads what a TypeScript device wrote, and writes back', async () => {
  const token = bucket();
  const key = await create(new Client({ url, token }), passphrase, 'Main');
  const a = await device(token, key, { sealPublic: ['todo'] });
  await a.put({ guid: newGUID(), collection: 'todos', datatype: 'todo', public: { order: 'V', t: 'a<b&c é' }, private: { text: 'from ts', n: [1, 2.5] } });
  await a.sync();
  const out = execFileSync(path(bin, 'device'), ['sync', url, token, passphrase], {
    input: JSON.stringify([{ guid: newGUID(), collection: 'todos', datatype: 'todo', public: { order: 'W' }, private: { text: 'from go' } }]),
  });
  const seen = JSON.parse(out.toString());
  const ts = seen.find((it) => it.private.text === 'from ts');
  assert.deepEqual(ts.private, { text: 'from ts', n: [1, 2.5] });
  assert.deepEqual(ts.public, { order: 'V', t: 'a<b&c é' });
  await a.sync();
  assert.deepEqual(await texts(a, 'todos'), ['from go', 'from ts']);
});

await test('order keys are the same as Go\'s', () => {
  const pairs = [['', ''], ['V', 'V0'], ['', '0'], ['W', 'V'], ['', '!'], ['V0', 'V1'], ['zz', ''], ['V', 'V01'], ['0', '1'], ['A0', 'A01']];
  const keys = [between('', '')];
  for (let i = 0; i < 300; i++) {
    const j = Math.floor(Math.random() * (keys.length + 1));
    const lo = keys[j - 1] ?? '', hi = keys[j] ?? '';
    pairs.push([lo, hi]);
    keys.splice(j, 0, between(lo, hi));
  }
  const ours = pairs.map(([a, b]) => {
    try {
      return between(a, b);
    } catch (e) {
      return e.name === 'NoRoomError' ? '!noroom' : '!error';
    }
  });
  const go = JSON.parse(execFileSync(path(bin, 'device'), ['between'], { input: JSON.stringify(pairs) }).toString());
  assert.deepEqual(ours, go);
});

server.kill();
rmSync(bin, { recursive: true, force: true });
console.log(`sync: all ${n} tests pass against the Go server`);
