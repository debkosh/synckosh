# synckosh

End-to-end encrypted sync for apps that keep the user's data on their devices.

synckosh syncs items between a person's devices through a small server that stores them but
can't read them. Each item has a public part the server can see and a private part sealed with a
key only the devices hold. For each item the newest edit wins, unless the app gives its datatype a
merger. Devices work offline and pull everything after their cursor. The server is an
`http.Handler` you mount in your own service, behind your own sign-in. Storage is bbolt, or bring
your own.

**Status:** experimental, protocol version 1. The server is Go; devices can be Go or
TypeScript, for browsers and Node ([js/](js)). The wire protocol is
[SPEC.md](SPEC.md), with test vectors in [testdata/vectors.json](testdata/vectors.json).

**What it is not:** no server-side queries, no real-time push, no partial sync, no sharing with
revocation, and no changing the data key yet.

```sh
go get github.com/sudiptadeb/synckosh
npm install synckosh
```

**What the server can and can't do.** When a bucket has a data key, the server, and anyone who
controls it or the connection, cannot read a private part. It cannot make a device that holds the
key use anything a key holder did not write: it cannot forge, alter or move an item, mark one
deleted, or delete a collection's items before a cutoff. Public parts are checked only for
datatypes sealed with their public part (`Config.SealPublic`). An item sealed under a key the
device doesn't hold is kept as locked: unverified, without effect, and never sent on; an app must
not trust what it shows. The server still sees collection and datatype names, guids, timestamps,
public parts, sizes and timing. It can withhold items, stop answering, or hand a device an older
version a key holder really wrote. It can try passphrases offline against the sealed copies, so a
passphrase protects the data only as well as it was chosen; backup codes are random, 128 bits.
Without a key, everything is plain and the server is trusted. The details are in
[SPEC.md](SPEC.md#what-sealing-protects).

### A server

```go
store, _ := bolt.OpenStore("server.db")
srv := server.New(store, server.Bearer(func(token string) (string, error) {
	return bucketForToken(token) // your sign-in decides which bucket a token syncs
}))
srv.CleanupAfter = 90 * 24 * time.Hour // how long deletes are kept; run srv.Cleanup() now and then
log.Fatal(http.ListenAndServe(":8484", srv))
```

`srv.ResetStore(bucket)` after any possible loss of a bucket's data, and `srv.ResetAll()` after
restoring the store from a backup: the devices then push everything back. Any other sign-in is an
`Auth` func that says which bucket a request is for.

### A device

```go
c := &client.Client{URL: "https://sync.example.com", Token: token}

key, err := keyFromKeychain() // synckosh.DataKeyFrom(id, bytes), or nil on the first run
if key == nil {
	// A new bucket: make its key, with a passphrase-sealed copy so other devices can join.
	key, err = client.Create(ctx, c, passphrase, "Main passphrase")
	if errors.Is(err, client.ErrHasKey) {
		key, err = client.Join(ctx, c, passphrase) // the bucket has a key already
	}
	saveToKeychain(key.ID, key.Bytes())
}

local, _ := bolt.OpenLocal("sync.db")
r, _ := client.Open(client.Config{Client: c, Local: local, Key: key})
r.Put(synckosh.Item{Collection: "bookmarks", Datatype: "bookmark", GUID: synckosh.NewGUID(),
	Public:  json.RawMessage(`{"order":"V"}`),
	Private: json.RawMessage(`{"title":"Go","url":"https://go.dev/"}`)})
if err := r.Sync(ctx); err != nil {
	// *client.RefusedError or *client.RejectedError: the rest synced
}
bookmarks, _ := r.Items("bookmarks")
```

The same device in TypeScript, from the `synckosh` npm package:

```ts
import { Client, MemoryLocal, Replica, create, newGUID } from 'synckosh';

const c = new Client({ url: 'https://sync.example.com', token });
const key = await create(c, passphrase, 'Main passphrase'); // join(c, passphrase) on HasKeyError; DataKey.from(id, bytes) later
const r = await Replica.open({ client: c, local: new MemoryLocal(), key }); // or a Local over IndexedDB
await r.put({ collection: 'bookmarks', datatype: 'bookmark', guid: newGUID(), public: { order: 'V' }, private: { title: 'Go', url: 'https://go.dev/' } });
await r.sync();
const bookmarks = await r.items('bookmarks');
```

The data key never leaves the device's key store. A person only ever sees a passphrase, or a
backup code from `key.BackupCode(label)`, each with its own sealed copy that can be removed alone.

## The model

- **One shape:** an item has a collection, a datatype, a guid, a timestamp, a deleted flag, a
  public part and a private part.
- **One rule:** for each item, the latest timestamp wins; in the same millisecond, the later push.
  A datatype's merger can join two versions that never saw each other instead.
- **Sync knows only items:** what an item holds, and how items relate, is the app's choice.
- **Deletes are updates:** the server keeps a delete until its cleanup. `DeleteBefore` deletes a
  collection's older items in one item, and the server forgets them at once.
- **The devices hold the real copy:** if the server loses data, it changes its store id and the
  devices push everything back.

Items live in a **bucket** (one synced space, whatever the host makes it), grouped in
**collections**, and each has a **datatype**.

## Packages

| Package | What it is |
|---|---|
| `synckosh` | the item, its checks, timestamps, UUIDv7 guids, order keys, the data key and passphrase-sealed copies |
| `client` | a device's `Replica`: put, delete, delete before a cutoff, read and sync every collection, over a `Local` store; `Create` and `Join` |
| `server` | the server as an `http.Handler`, over a `Store`; `Bearer` sign-in; `storetest` checks any `Store` |
| `bolt` | a `server.Store` and a `client.Local` in bbolt files |
| [`js/`](js) | the `synckosh` npm package: the device side in TypeScript, everything above but the server and bolt |

## Tests

```sh
go test -race ./...
cd js && npm ci && npm test
```

They sync devices through a real server and bbolt files. Among them: newest wins, and mergers join
offline edits without merging a refill, a locked version or a device's own lost answer; a server
that strips, forges, alters or moves items is caught; items under an unknown key stay locked; the
server's file never holds a private part in the clear; a refill survives a crash at every write;
pushes and pages stay within their bytes, whichever side has the smaller limits; deletes and
delete-befores leave the devices and the server; a device away past the cleanup age starts over,
but refills a wiped server; twenty pushes at once number in order on two servers sharing a store;
and the test vectors. The TypeScript package passes the same vectors, and syncs TypeScript and
Go devices through a real Go server.

## Not yet

Changing the data key after a leak, and a store other than bbolt.
