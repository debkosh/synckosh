# synckosh

End-to-end encrypted sync for apps that keep the user's data on their devices: the device side,
for browsers and Node. It syncs items through a [synckosh server](https://github.com/sudiptadeb/synckosh)
(Go) that stores them but can't read them, with the same wire, sealing and sync rules as the Go
client, checked by shared test vectors and against a real Go server.

```sh
npm install synckosh
```

```ts
import { Client, MemoryLocal, Replica, create, join, HasKeyError } from 'synckosh';

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
await r.put({ collection: 'bookmarks', datatype: 'bookmark', public: { order: 'V' }, private: { title: 'Go', url: 'https://go.dev/' } });
await r.sync(); // throws RefusedError or RejectedError for what didn't sync: the rest did
const bookmarks = await r.items('bookmarks');
```

`MemoryLocal` keeps the device's copy in memory; for a copy that lasts, implement `Local` (seven
async methods) over IndexedDB, a file or a database. Crypto is XChaCha20-Poly1305 and Argon2id
from the audited [@noble/ciphers](https://github.com/paulmillr/noble-ciphers) and
[@noble/hashes](https://github.com/paulmillr/noble-hashes).

The full README, the wire protocol ([SPEC.md](https://github.com/sudiptadeb/synckosh/blob/main/SPEC.md))
and the server: https://github.com/sudiptadeb/synckosh
