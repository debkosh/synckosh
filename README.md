# synckosh

Sync for apps that keep the user's data on their devices: bookmarks, logins, settings, notes,
anything. A small server keeps the newest version of each item, and never sees inside one. The
private part of every item is sealed with a key that only the user's passphrases open, as
[keykosh](https://github.com/sudiptadeb/keykosh) seals its keys.

- **One shape:** an item has a bucket, a type, a guid, a timestamp, a deleted flag, a public part
  and a private part.
- **One rule:** for each guid, the newest timestamp wins.
- **Items, not relationships:** an item names its parent. Lists, objects and trees are built on
  the device; an item whose parent is gone is an orphan, never deleted.
- **The devices hold the real copy:** if the server loses data, it changes its store id and the
  devices push everything back.

The protocol is [SPEC.md](SPEC.md).

## Packages

| Package | What it is |
|---|---|
| `synckosh` | the item, timestamps, UUIDv7 guids, order keys, the data key and passphrase-sealed copies |
| `client` | a device's `Replica`: put, delete, clear, read and sync, over a `Local` store |
| `server` | the server as an `http.Handler`, over a `Store`; signing in is the host's |
| `bolt` | a `server.Store` and a `client.Local` in bbolt files |

## A device

```go
local, _ := bolt.OpenLocal("sync.db")
key, _ := synckosh.NewDataKey()             // or synckosh.OpenSealedCopy(item, passphrase)
r, _ := client.Open(client.Config{
	Client:  &client.Client{URL: "https://sync.example.com", HTTP: signedInHTTPClient},
	Local:   local,
	Key:     key,
	Device:  "mac",
	Buckets: []string{"keys", "bookmarks"},
})

sealed, _ := key.SealedCopy(passphrase, "Main passphrase")
r.Put(sealed)
r.Put(synckosh.Item{Bucket: "bookmarks", Type: "bookmark",
	Public:  json.RawMessage(`{"parent":"root","order":"V"}`),
	Private: json.RawMessage(`{"title":"Go","url":"https://go.dev/"}`)})
r.Sync(ctx)
```

## A server

```go
store, _ := bolt.OpenStore("server.db")
srv, _ := server.New(store, func(r *http.Request) (string, error) {
	return accountFromYourSignIn(r)
})
http.ListenAndServe(":8484", srv)
```

`srv.ResetStore(account)` after any possible data loss, and `srv.Cleanup(90 * 24 * time.Hour)` now
and then.

## Tests

```sh
go test ./...
```

They sync devices through a real server and bbolt files: newest wins whichever device pushes
first; a login whose fields and passwords are items keeps both devices' edits; the server's file
never holds a private part in the clear; a new store id refills the server, with a new device
getting the data as it comes back; 2,500 items page without loss; clearing a bucket; reopening a
device; retries, old versions, timestamps too far ahead and other accounts; cleanup; and joining
with only a passphrase.

## Not yet

Keeping the data key in the OS keychain, signing in, and changing the data key after a leak.
