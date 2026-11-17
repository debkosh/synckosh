# synckosh

Sync for apps that keep the user's data on their devices: bookmarks, logins, settings, notes,
anything. A small server keeps the newest version of each item, and never sees inside one. The
private part of every item is sealed with a key that only the user's passphrases open, as
[keykosh](https://github.com/sudiptadeb/keykosh) seals its keys.

- **One shape:** an item has a collection, a datatype, a guid, a timestamp, a deleted flag, a public part
  and a private part.
- **One rule:** for each guid, the latest timestamp wins; in the same millisecond, the later push.
- **Items, not relationships:** an item names its parent. Lists, objects and trees are built on
  the device; an item whose parent is gone is an orphan, never deleted.
- **The devices hold the real copy:** if the server loses data, it changes its store id and the
  devices push everything back.

Items live in a **bucket** (one synced space: a person's data, a shared list), grouped in
**collections** (bookmarks, logins), and each has a **datatype** (folder, bookmark). The protocol
is [SPEC.md](SPEC.md).

## Packages

| Package | What it is |
|---|---|
| `synckosh` | the item, timestamps, UUIDv7 guids, order keys, the data key and passphrase-sealed copies |
| `client` | a device's `Replica`: put, delete, delete before a cutoff, read and sync every collection, over a `Local` store |
| `server` | the server as an `http.Handler`, over a `Store`; signing in is the host's |
| `bolt` | a `server.Store` and a `client.Local` in bbolt files |

## A device

```go
c := &client.Client{URL: "https://sync.example.com", HTTP: signedInHTTPClient}
// First run only: make the bucket's key, or join with a passphrase. Keep it in the keychain.
key, _ := client.Create(ctx, c, passphrase, "Main passphrase") // or client.Join(ctx, c, passphrase)

local, _ := bolt.OpenLocal("sync.db")
r, _ := client.Open(client.Config{Client: c, Local: local, Key: key})
r.Put(synckosh.Item{Collection: "bookmarks", Datatype: "bookmark",
	Public:  json.RawMessage(`{"parent":"root","order":"V"}`),
	Private: json.RawMessage(`{"title":"Go","url":"https://go.dev/"}`)})
r.Sync(ctx)
```

## A server

```go
store, _ := bolt.OpenStore("server.db")
srv := server.New(store, func(r *http.Request) (string, error) {
	return bucketFromYourSignIn(r) // the bucket this request may use
})
http.ListenAndServe(":8484", srv)
```

`srv.ResetStore(bucket)` after any possible data loss, `srv.ResetAll()` after restoring the store
from a backup, and set `srv.CleanupAfter = 90 * 24 * time.Hour` before serving and run `srv.Cleanup()` now
and then.

## Tests

```sh
go test ./...
```

They sync devices through a real server and bbolt files: newest wins whichever device pushes
first, and the later push in the same millisecond; edits to different items all survive; the server's file
never holds a private part in the clear; a new store id refills the server, with a new device
getting the data as it comes back; 2,500 items page without loss; deleting before a cutoff; reopening a
device; retries, old versions, timestamps too far ahead and other buckets; cleanup; a device not pulling its own push back, unless another push came in between; a clock
two days ahead refused with a clear error, then healing; and joining with only a passphrase.

## Not yet

Keeping the data key in the OS keychain, signing in, and changing the data key after a leak.
