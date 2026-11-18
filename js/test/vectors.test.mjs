// Every vector in ../testdata/vectors.json, checked as the Go package's vectors_test.go checks
// it: the additional data, the box made again from what it opens to with its own nonce, the item
// opened, and the sealed copy opened with its passphrase.
import { readFileSync } from 'node:fs';
import { DataKey, fromWire, openSealedCopy } from '../dist/synckosh.js';
import { additional, sealWith } from '../dist/keys.js';
import { unbase64, utf8 } from '../dist/encoding.js';

const v = JSON.parse(readFileSync(new URL('../../testdata/vectors.json', import.meta.url), 'utf8'));
const key = DataKey.from(v.data_key.id, unbase64(v.data_key.key));
const same = (a, b) => JSON.stringify(a ?? null) === JSON.stringify(b ?? null);
let n = 0, bad = 0;
const fail = (msg) => {
  bad++;
  console.error('differs: ' + msg);
};

for (const c of v.items) {
  n++;
  const it = fromWire(c.item), sealed = fromWire(c.sealed);
  if (additional(it, c.public_in_box) !== c.additional_data) fail(`${it.guid}: additional data ${JSON.stringify(additional(it, c.public_in_box))}`);
  const p = sealed.private;
  if (p.key !== key.id || (p.public === true) !== c.public_in_box) fail(`${it.guid}: sealed private part ${JSON.stringify(p)}`);
  const box = unbase64(p.box);
  const again = sealWith(key.bytes(), box.subarray(0, 24), utf8(c.box_opens_to), utf8(c.additional_data));
  if (again !== p.box) fail(`${it.guid}: sealing again with the same nonce gives another box`);
  const opened = key.open(sealed);
  if (!same(opened.public, it.public) || !same(opened.private, it.private)) fail(`${it.guid}: opened to ${JSON.stringify(opened)}`);
  // Sealed here and opened again, it is the item.
  const back = key.open(key.seal(it, c.public_in_box));
  if (!same(back.public, it.public) || !same(back.private, it.private)) fail(`${it.guid}: sealed and opened, ${JSON.stringify(back)}`);
  // Changing any field the seal binds makes it fail to open.
  for (const change of [{ guid: it.guid + 'x' }, { timestamp: it.timestamp + 1 }, { deleted: !it.deleted }, { base: (it.base ?? 0) + 1 }, { collection: 'other' }]) {
    try {
      key.open({ ...sealed, ...change });
      fail(`${it.guid}: opened with ${JSON.stringify(change)}`);
    } catch {}
  }
}

n++;
const sc = fromWire(v.sealed_copy.item);
const got = await openSealedCopy(sc, v.sealed_copy.passphrase);
if (got.id !== key.id || !same([...got.bytes()], [...key.bytes()])) fail('the sealed copy opens to another key');
try {
  await openSealedCopy(sc, v.sealed_copy.passphrase + '!');
  fail('the sealed copy opens with a wrong passphrase');
} catch (e) {
  if (e.name !== 'PassphraseError') fail('wrong passphrase: ' + e.message);
}

if (bad) {
  console.error(`vectors: ${bad} checks of ${n} vectors differ`);
  process.exit(1);
}
console.log(`vectors: all ${n} vectors match (${v.items.length} sealed items and the sealed copy)`);
