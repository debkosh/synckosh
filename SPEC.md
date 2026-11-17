# synckosh protocol, version 1

How a device syncs items through a synckosh server. The Go package in this repo is the
reference client, and `server/` is the reference server.

## The model

Everything synced is an **item**: a type, a guid, a timestamp, a deleted flag and the type's
data. Items live in buckets by type. For each type and guid there is one current item.

- **Making something:** the device makes a new guid.
- **Changing something:** the device keeps the guid it synced with.
- **Deleting:** the item stays, marked deleted, with no data, so every device learns of it.
- **When two devices changed one item:** the type's merger decides. The default merger takes
  the item with the newer timestamp. A type can have its own merger; the server never needs to
  know.
- **What belongs to the type:** parent and child, order in a list, and what its data means.
  Sync only moves items.

The server never sees inside the data. Devices seal it with a key that only the account's
passphrase opens.

## The item

```json
{
  "type": "bookmark",
  "guid": "019a8f3c-5e21-7b40-9c3d-2f6a1e8b4d07",
  "ts": [1763376127123, 0, "d_qm53mdauGC7vF2k_"],
  "deleted": false,
  "data": "seal1:Qm9hcmQ...",
  "seq": 42
}
```

| Field | What it is |
|---|---|
| `type` | the bucket: lowercase letters, digits, `.`, `-` or `_`, up to 64. `sync.keys` is reserved. |
| `guid` | letters, digits and `._:~-`, up to 128. |
| `ts` | when the edit was made: `[ms, n, device]`, see below. |
| `deleted` | true for a deleted item, which has no `data`. |
| `data` | the type's data, sealed: a string `seal1:` and base64. |
| `seq` | set by the server: the change number at which it stored this version. |
| `base` | sent by a device on a push: the `seq` of the version this edit was made on, or 0 for none. |

### Guids

A new item's guid is a **UUIDv7** ([RFC 9562](https://www.rfc-editor.org/rfc/rfc9562)), which
sorts by when it was made. Items that are the same thing on every device get a fixed guid
instead, such as `setting:theme` or `tabs:<device>`, so two devices don't make two of them.

### Timestamps

`[ms, n, device]`: the wall clock in milliseconds, a counter, and the device's id. Timestamps
compare by `ms`, then `n`, then `device`. A device:

- never writes a timestamp lower than one it has written or read: it takes the larger of its
  clock and the newest it has seen, and counts up `n` when that is not later than its last;
- doesn't follow a timestamp more than a day ahead of its own clock.

So an edit made after seeing another always sorts after it, whatever the wall clocks say. The
server refuses items more than a day ahead of its own clock.

### Order in a list

An item in a list keeps its own **order key** in its data, never a list of children in its
parent. Adding or moving one item then changes only that item, and two devices adding to one
list both keep their adds.

- An order key is a string that sorts with plain byte order. `Between(a, b)` makes one between
  two others.
- Two devices may give two items the same key. A list shows its items by order key, then by
  guid, which is when each was made. Duplicates are fine.
- Any device may later give items new keys, to tidy a list. That is an ordinary edit, and the
  list settles.

### Sealed data

The account's key is 32 random bytes. `data` is sealed with XChaCha20-Poly1305 under that key.
The sealed bytes are a 24-byte nonce, then the ciphertext. The additional data is
`type`, `guid`, `ts` as JSON and `deleted` ("true" or "false"), joined with a zero byte. So the
server can't move sealed data to another item or another timestamp.

The key itself is the one item not sealed with it: type `sync.keys`, guid `keys`. Its data is a
base64 string of the key encrypted with an [age](https://age-encryption.org) scrypt passphrase
recipient, as [keykosh](https://github.com/sudiptadeb/keykosh) seals its identity. A new device
reads that item, asks for the passphrase and opens it. Changing the passphrase writes the item
again.

## The server

The server keeps, for each account, the current item for each type and guid, and a change number
that goes up by one for each item it stores. JSON over HTTPS. Every call but the first two needs
`Authorization: Bearer <token>`.

| Call | What it does |
|---|---|
| `GET /v1` | `{"version": 1, "features": ["wait"]}` |
| `POST /v1/accounts` `{"name"}` | a new account with this device in it: `{"account", "device", "token"}` |
| `GET /v1/devices` | `{"devices": [{"id", "name", "created"}]}` |
| `POST /v1/devices` `{"name"}` | a token for another device: `{"account", "device", "token"}` |
| `DELETE /v1/devices/{id}` | ends that device's token |
| `POST /v1/items` `{"items": [...]}` | push, below |
| `GET /v1/items?since=&type=&limit=&wait=` | pull, below |

### Push

Up to 1000 items, each up to 1 MB. For each item the server answers, in order:

- `{"ok": true, "seq": n}`: stored. It is stored if there is no item with that guid yet, or if
  its `base` is the `seq` of the item the server holds.
- `{"ok": true, "seq": n}` too, if the server holds an item with the same `ts`: a device's
  timestamp names one edit, so this is a retry.
- `{"ok": false, "current": {...}}`: the server holds a version this edit wasn't made on. The
  device merges its item with `current`, using the type's merger, and pushes the result with
  `base` set to `current.seq`.
- `{"ok": false, "error": "..."}`: the item is wrong, for example a `ts` too far ahead.

The server stores each item as the device sent it, fields it doesn't know included, without
`seq` and `base`.

### Pull

The items stored after change number `since`, oldest change first, of one type if `type` is
given, up to `limit` (default 500, at most 1000):
`{"items": [...], "next": n, "more": bool}`. The device keeps `next` and passes it as `since`
next time. With `wait` (seconds, at most 60) and nothing new, the server holds the request until
something changes.

## A device's sync, step by step

1. Push the items changed here, each with `base`.
2. For each `current` that comes back, merge, and keep the result to push.
3. Pull from the last `next`. For each item: if it wasn't changed here, take it. If it was,
   merge.
4. Merging: if the merger's result says the same as the server's item, take the server's. If it
   says the same as the local item and the local one is newer, push the local one. Otherwise the
   result is a new edit, with a new timestamp.
5. Repeat until nothing is left to push.

## Staying compatible

- New types need no change to the server.
- The server keeps envelope fields it doesn't know, and clients keep them when they write an
  item back. So a newer client can add a field and an older one won't strip it.
- A merger that reads `data` keeps the fields it doesn't know, for the same reason.
- `ts` may gain elements after the third. Readers ignore them.
- The server adds to `features` and never takes away. A change that would break a version 1
  client is version 2, under `/v2`.
