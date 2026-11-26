# synckosh

End-to-end encrypted sync for apps that keep the user's data on their devices: the device side,
for browsers and Node. It syncs items through a [synckosh server](https://github.com/debkosh/synckosh)
(Go) that stores them but can't read them, with the same wire, sealing and sync rules as the Go
client, checked by shared test vectors and against a real Go server.

```sh
npm install synckosh
```

```ts
import { Client, MemoryLocal, Replica, create, join, newGUID, HasKeyError } from 'synckosh';

const client = new Client({ url: 'https://sync.example.com', token });
let key = await keyFromKeyStore(); // DataKey.from(id, bytes), or undefined on the first run
if (!key) {
  key = await create(client, passphrase, 'Main passphrase').catch((e) => {
    if (e instanceof HasKeyError) return join(client, passphrase); // the bucket has a key already
    throw e;
  });
  await saveToKeyStore(key.id, key.bytes());
}
const r = await Replica.open({ client, local: new MemoryLocal(), key }); // or your own Local
await r.put({ collection: 'bookmarks', datatype: 'bookmark', guid: newGUID(), public: { order: 'V' }, private: { title: 'Go', url: 'https://go.dev/' } });
await r.sync(); // throws RefusedError or RejectedError for what didn't sync: the rest did
const bookmarks = await r.items('bookmarks');
```

A person only ever sees a passphrase or a backup code, never the key. On a device that holds the
key, `r.passphrases()` lists the bucket's copies (label, kind, creation time), `r.addBackupCode(label)`
returns a code such as `7K4Q-M2XH-9RTD-WC6F-3NVA-8PSE` to show once, `r.checkPassphrase` checks a
passphrase or a code, `r.setPassphrase` sets a new passphrase and keeps the codes, and
`r.removePassphrase(guid)` removes one. `join` takes a code as a person types it, so a person who
forgot the passphrase joins with the code and sets a new one.

`MemoryLocal` keeps the device's copy in memory; for a copy that lasts, implement `Local` (seven
async methods) over IndexedDB, a file or a database. Crypto is XChaCha20-Poly1305 and Argon2id
from the audited [@noble/ciphers](https://github.com/paulmillr/noble-ciphers) and
[@noble/hashes](https://github.com/paulmillr/noble-hashes).

The full README, the wire protocol ([SPEC.md](https://github.com/debkosh/synckosh/blob/main/SPEC.md))
and the server: https://github.com/debkosh/synckosh
