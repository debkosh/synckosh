# synckosh

Sync for apps that keep the user's data on their devices: bookmarks, settings, passwords, notes,
anything. A small server keeps the newest version of each item, and never sees inside one.
The data is sealed with a key that only the user's passphrase opens, as
[keykosh](https://github.com/sudiptadeb/keykosh) seals its keys.

- **One shape for everything.** An item is a type, a guid, a timestamp, a deleted flag and data.
- **One rule.** For each item the newest version wins, unless its type has a merger of its own.
- **A server simple enough to keep compatible.** It stores items, numbers changes and checks
  that each edit was made on the version it holds. Types, mergers and data are the clients'.

The protocol is in [SPEC.md](SPEC.md). This repo holds it, the Go client library, and a
reference server.

## The library

```go
import "github.com/sudiptadeb/synckosh"

client, acct, _ := synckosh.Signup(ctx, "https://sync.example.com", "MacBook Pro")
keys, _ := synckosh.NewKeys()
clock := synckosh.NewClock(acct.Device)
r := synckosh.NewReplica(client, keys, clock)

locked, _ := keys.Lock(passphrase, clock.Now()) // the key, sealed with the passphrase
r.Put(locked.Type, locked.GUID, locked.Data)

r.Put("bookmark", synckosh.NewGUID(), json.RawMessage(`{"title":"Go","url":"https://go.dev/"}`))
r.Mergers["login"] = myLoginMerger // optional; the default is newest wins
r.Sync(ctx)
```

A second device gets a token from the first (`client.AddDevice`), reads the `sync.keys` item,
and opens it with `synckosh.Unlock(item, passphrase)`.

Also in the package: `NewGUID` makes UUIDv7 guids, which sort by when they were made. `Between`
makes an order key between two others, for lists.

## The server

```sh
cd server && go build -o synckosh-server .
./synckosh-server -addr 127.0.0.1:8484 -db sync.db
```

One binary and one SQLite file. Put TLS in front of it.

## Tests

```sh
go test ./... && (cd server && go test ./...)
```

The server's tests sync two devices through a real server: a join with the passphrase, newest
wins, a custom merger that keeps both devices' edits, a delete, a retry, a clock far ahead, a
removed device, a waiting pull, and that the server never holds data in the clear.

## Status

Version 1 of the protocol, as a first cut. Still to come: storing a device's token and key in
the OS keychain, a device's replica on disk, and changing the key when a device is removed.
