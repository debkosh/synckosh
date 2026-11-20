// The API without a server: order keys, guids, the clock, names, sealing, sealed copies and a
// replica's own edits.
import assert from 'node:assert/strict';
import {
  between, less, NoRoomError, newGUID, Clock, MaxAhead, valid, cutoff, validName, DataKey, openSealedCopy,
  fromWire, toWire, Replica, Client, MemoryLocal, LockedError, PutError, KeysCollection, SealedKeyDatatype,
} from '../dist/synckosh.js';
import { base32, base64, base64URL, unbase64 } from '../dist/encoding.js';

let n = 0;
async function test(name, fn) {
  try {
    await fn();
    n++;
  } catch (e) {
    console.error(`api: ${name}: ${e.stack}`);
    process.exit(1);
  }
}

await test('encodings are Go\'s', () => {
  for (let len = 0; len < 40; len++) {
    const b = new Uint8Array(len).map((_, i) => (i * 37 + len) & 0xff);
    const buf = Buffer.from(b);
    assert.equal(base64(b), buf.toString('base64').replace(/=+$/, ''));
    assert.equal(base64URL(b), buf.toString('base64url'));
    assert.deepEqual([...unbase64(base64(b))], [...b]);
  }
  assert.equal(base32(new Uint8Array([0xff, 0, 0x10])), '74ABA');
  assert.throws(() => unbase64('QQ=='));
  assert.throws(() => unbase64('Q'));
});

await test('between keeps a list in order', () => {
  const keys = [between('', '')];
  for (let i = 0; i < 2000; i++) {
    if (i % 3 === 0) keys.unshift(between('', keys[0]));
    else if (i % 3 === 1) keys.push(between(keys[keys.length - 1], ''));
    else {
      const j = keys.length >> 1;
      keys.splice(j, 0, between(keys[j - 1], keys[j]));
    }
  }
  for (let i = 1; i < keys.length; i++) assert.ok(keys[i - 1] < keys[i], `${keys[i - 1]} ${keys[i]}`);
  assert.ok(less('a', '2', 'b', '1') && less('a', '1', 'a', '2'));
});

await test('between without room', () => {
  for (const [a, b] of [['V', 'V0'], ['', '0'], ['', '000'], ['V', 'V'], ['W', 'V'], ['Vz', 'V']]) {
    assert.throws(() => between(a, b), NoRoomError, `${a} ${b}`);
  }
  for (const [a, b] of [['', '!'], ['a b', ''], ['V', 'é']]) {
    assert.throws(() => between(a, b), (e) => !(e instanceof NoRoomError));
  }
  for (const [a, b] of [['V0', 'V1'], ['zz', ''], ['V', 'V01'], ['0', '1']]) {
    const k = between(a, b);
    assert.ok(k > a && (b === '' || k < b) && !k.endsWith('0'));
  }
});

await test('guids are UUIDv7 and sort by when made', () => {
  const gs = Array.from({ length: 5000 }, newGUID);
  for (const g of gs) assert.match(g, /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  for (let i = 1; i < gs.length; i++) assert.ok(gs[i - 1] < gs[i]);
  assert.ok(Math.abs(parseInt(gs[0].replace(/-/g, '').slice(0, 12), 16) - Date.now()) < 5000);
});

await test('the clock never goes back', () => {
  let wall = 1000;
  const c = new Clock(() => wall);
  assert.equal(c.now(), 1000);
  assert.equal(c.now(), 1001);
  c.observe(5000);
  assert.equal(c.now(), 5001);
  c.observe(wall + MaxAhead + 1); // too far ahead: ignored
  assert.equal(c.last(), 5001);
  wall = 10000;
  assert.equal(c.now(), 10000);
  c.reset(2000);
  assert.equal(c.now(), 10000);
});

await test('valid', () => {
  const ok = { collection: 'bookmarks', datatype: 'bookmark', guid: newGUID(), timestamp: 1, deleted: false };
  valid(ok);
  valid({ collection: 'history', datatype: '_deletebefore', guid: '_deletebefore', timestamp: 1, deleted: false, public: { before: 5 } });
  // A name that starts with an underscore is the protocol's by convention, and taken all the same.
  for (const mine of [{ collection: '_x' }, { datatype: '_x' }, { guid: '_x' }]) valid({ ...ok, ...mine });
  for (const bad of [{ collection: 'Bookmarks' }, { guid: 'a b' }, { guid: '' }, { timestamp: 0 }, { collection: KeysCollection },
    { datatype: '_deletebefore', guid: '_deletebefore', public: {} }, { datatype: '_deletebefore', guid: '_deletebefore', public: { before: 5 }, deleted: true }]) {
    assert.throws(() => valid({ ...ok, ...bad }), undefined, JSON.stringify(bad));
  }
  assert.equal(cutoff({ ...ok, public: { before: 7 } }), 7);
  assert.ok(validName(KeysCollection) && validName('a.b') && validName('_x') && !validName('.x'));
});

await test('items keep fields they do not know', () => {
  const w = { collection: 'c', datatype: 'd', guid: 'g', timestamp: 1, deleted: false, later: { x: [1] }, sequence: 4, deleted_at: 9 };
  const it = fromWire(w);
  assert.deepEqual(it.extra, { later: { x: [1] } });
  assert.equal(it.deletedAt, 9);
  assert.equal(JSON.stringify(toWire(it)), JSON.stringify(Object.fromEntries(Object.entries(w).sort(([a], [b]) => (a < b ? -1 : 1)))));
  assert.throws(() => fromWire({ ...w, timestamp: '1' }));
});

await test('seal binds the item, and the library\'s items carry their public part', () => {
  const key = DataKey.create(), other = DataKey.create();
  const it = { collection: 'notes', datatype: 'note', guid: newGUID(), timestamp: 5, deleted: false, public: { a: 1 }, private: { b: 2 } };
  const s = key.seal(it);
  assert.deepEqual(key.open(s), it);
  assert.throws(() => other.open(s), (e) => e.name === 'UnknownKeyError');
  assert.throws(() => key.open(it), (e) => e.name === 'NotSealedError');
  // Without the public part in the box the server can change it; with it, the box's copy wins.
  assert.deepEqual(key.open({ ...s, public: { a: 2 } }).public, { a: 2 });
  const sp = key.seal(it, true);
  assert.deepEqual(key.open({ ...sp, public: { a: 2 } }).public, { a: 1 });
  const lib = key.seal({ ...it, collection: 'h', datatype: '_deletebefore', guid: '_deletebefore', public: { before: 1 } });
  assert.equal(lib.private.public, true);
  assert.deepEqual(key.open({ ...it, private: { key: key.id, box: key.seal(it).private.box } }), it);
});

await test('sealed copies and backup codes', async () => {
  const key = DataKey.create();
  const { code, copy } = await key.backupCode('Backup');
  assert.match(code, /^[A-Z2-7]{4}(-[A-Z2-7]{4}){5}-[A-Z2-7]{2}$/);
  assert.equal(copy.collection, KeysCollection);
  assert.equal(copy.datatype, SealedKeyDatatype);
  const sealed = key.seal({ ...copy, guid: newGUID(), timestamp: 1 });
  const got = await openSealedCopy(sealed, code);
  assert.equal(got.id, key.id);
  assert.deepEqual(got.bytes(), key.bytes());
  await assert.rejects(openSealedCopy(sealed, 'wrong'), (e) => e.name === 'PassphraseError');
  // Settings no honest writer makes are refused before any work.
  for (const bad of [{ kdf: 'scrypt' }, { t: 11 }, { p: 0 }, { m: 1 << 20 }, { salt: 'QUJD' }]) {
    await assert.rejects(openSealedCopy({ ...sealed, public: { ...sealed.public, ...bad } }, code), /sealed key/);
  }
  // A copy the key's holder didn't seal is refused even with the right passphrase.
  await assert.rejects(openSealedCopy({ ...sealed, private: DataKey.create().seal(sealed).private }, code), /not sealed by its own key/);
});

await test('a replica records edits here, and leaves the caller\'s items', async () => {
  const local = new MemoryLocal();
  const r = await Replica.open({ client: new Client({ url: 'http://127.0.0.1:1' }), local, key: DataKey.create() });
  const mine = { collection: 'todos', datatype: 'todo', guid: newGUID(), public: { order: 'V' }, private: { text: 'milk' } };
  const [put] = await r.put(mine);
  assert.deepEqual(mine, { collection: 'todos', datatype: 'todo', guid: put.guid, public: { order: 'V' }, private: { text: 'milk' } });
  put.private.text = 'changed';
  assert.equal((await r.get('todos', put.guid)).private.text, 'milk');
  assert.equal((await r.items('todos')).length, 1);
  assert.equal((await local.pending()).length, 1);
  await assert.rejects(r.put({ collection: 'Todos', datatype: 'todo', guid: newGUID() }), /collection/);
  await assert.rejects(r.put({ collection: 'todos', datatype: 'todo', guid: newGUID(), private: 'x'.repeat(1 << 20) }), /over the item limit/);
  // A put with bad items stores none, and names every one by its place in the call.
  const good = { collection: 'todos', datatype: 'todo', guid: newGUID() };
  await assert.rejects(r.put(good, { collection: 'todos', datatype: 'todo' }, { ...good, guid: 'todo 1' }, good, { ...good, collection: 'Todos' }),
    (e) => e instanceof PutError && JSON.stringify(e.items.map((it) => [it.index, it.guid])) === JSON.stringify([[1, ''], [2, 'todo 1'], [4, good.guid]]));
  assert.equal((await local.pending()).length, 1);
  await r.delete('todos', put.guid);
  const lockedGUID = newGUID();
  assert.equal((await r.items('todos')).length, 0);
  assert.equal((await r.get('todos', put.guid)).deleted, true);
  await local.put([{ item: { collection: 'todos', datatype: 'todo', guid: lockedGUID, timestamp: 1, deleted: false }, locked: true }]);
  await assert.rejects(r.put({ collection: 'todos', datatype: 'todo', guid: lockedGUID }), (e) => e instanceof PutError && e.items[0].error instanceof LockedError);
  await assert.rejects(r.get('todos', lockedGUID), LockedError);
  assert.equal((await r.locked('todos')).length, 1);
  // The clock carries over a restart.
  const again = await Replica.open({ client: new Client({ url: 'http://127.0.0.1:1' }), local });
  const [next] = await again.put({ collection: 'todos', datatype: 'todo', guid: newGUID() });
  assert.ok(next.timestamp > put.timestamp);
});

console.log(`api: all ${n} tests pass`);
