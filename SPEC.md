# synckosh protocol, version 1

How devices sync a bucket's data through a synckosh server. The Go packages in this repo are
the reference: `client` for a device, `server` for the server, `bolt` for storage on both.

## The model

Three levels name where an item lives:
- **A bucket** is one synced space with its own data key, sequence numbers and store id. It is
  whatever the host makes it: one person's data, a shared list, a family's. The server is told
  which bucket a request is for, and buckets never see each other.
- **A collection** is a kind of data inside a bucket: `bookmarks`, `logins`, `history`.
- **A datatype** is what one item is: `folder`, `bookmark`, `login-field`.

- **Everything is an item.** An item has a collection, a datatype, a guid, a timestamp, a deleted flag,
  a public part and a private part.
- **For each guid, the item with the newest timestamp wins;** in the same millisecond, the one
  the server stored last. That is all the server does with items. It never opens a private part.
- **Items know their parent; sync knows only items.** An item may name a parent in its public
  part. Sync never follows it. Lists, objects and trees are built by the client from the items
  that name a parent.
- **Orphans stay.** An item whose parent is missing or deleted is still alive. Nothing is ever
  deleted because of another item. Each datatype decides how its orphans show: a bookmark at the top
  level, a login field not at all.
- **Deletes are updates.** A deleted item stays, marked deleted, so every device hears of it.
  Only the server's cleanup removes items, long after they were deleted.
- **The devices hold the real copy.** If the server loses anything, devices push it back.

## The item

```json
{
  "collection": "logins",
  "datatype": "login-field",
  "guid": "019a8f3c-5e21-7b40-9c3d-2f6a1e8b4d07",
  "timestamp": 1763380800000,
  "deleted": false,
  "public": {"parent": "019a8f3c-5e1f-7a10-8b2e-1c5d0f3a9e42", "key": "password"},
  "private": {"key": "k_sUwGzUYSPw", "box": "7lg3iKfl2s0W…"},
  "sequence": 42,
  "deleted_at": 1763390000000
}
```

| Field | Set by | What it is |
|---|---|---|
| `collection` | device | the kind of data: `bookmarks`, `logins`, `history`. The server learns of collections only from items. Lowercase letters, digits, `.`, `-`, `_`, up to 64. |
| `datatype` | device | what the item is, for the client: `folder`, `bookmark`, `login`, `login-field`. Same characters. |
| `guid` | device | a UUIDv7 for a new item ([RFC 9562](https://www.rfc-editor.org/rfc/rfc9562)), so guids sort by when they were made. An edit keeps the guid. Letters, digits, `._:~-`, up to 128. |
| `timestamp` | device | when the edit was made: the device's clock, in milliseconds. |
| `deleted` | device | true for a deleted item. |
| `public` | device | plain JSON, readable by the server and by a device that hasn't unlocked yet. |
| `private` | device | JSON sealed with the bucket's data key. |
| `sequence` | server | the server's sequence number for this version: the order it was stored in, from the bucket's own counter, which only goes up. |
| `deleted_at` | server | the server's clock, in milliseconds, when it first stored the item deleted. |

Fields a reader doesn't know are kept and written back unchanged, by the server and by clients.

### Timestamps

A timestamp is a plain number of milliseconds. A device's timestamps:
- are always after every one it has made or read: its clock, or one millisecond past the newest
  it has seen when its clock is behind;
- don't follow a timestamp more than a day ahead of its own clock.

All the items of one write get one timestamp, so writing many items doesn't move the clock ahead.
The server refuses items more than a day ahead of its own clock, with the code
`timestamp-ahead` and its own time. The device then moves its clock back to the server's time
and gives the refused items a new timestamp: they were never stored anywhere, so that is safe.
While its clock is still wrong they are refused again; once it is right, they go out.

Two devices that haven't seen each other's edit can make them in the same millisecond. Then the
one the server stores last wins: the server stores an edit unless it holds one with a later
timestamp, and a device takes what it pulls unless its own version has a later timestamp. Every
device pulls the server's copy, so they all end up with the same one. Nothing depends on which
device made an edit.

### Public and private parts

The private part is sealed with XChaCha20-Poly1305 under the bucket's data key:
`{"key": "<data key id>", "box": "<base64 of 24-byte nonce and ciphertext>"}`. The additional
data is the JSON array `[collection, datatype, guid, timestamp, deleted, public]`, so the server can't change
the public part, or move a private part to another item, without the device noticing.

Structure is public and content is private. For example:

| Datatype | Public | Private |
|---|---|---|
| folder, bookmark | parent, order | title, URL |
| login | | origin |
| login-field | parent, field name | value |
| visit, tab | | everything |
| setting | name | value |

### Lists, objects and trees

- **A list** is the items that name one parent, sorted by their order key, then by guid. Two
  devices may give two items the same order key; both show, and any device may later give them
  new keys as an ordinary edit. `synckosh.Between` makes a key between two others.
- **An object** is the same, with the field's name as the key. When one key appears twice, the
  datatype shows the newest, and a device may delete the other.
- **A tree** is a list whose children can be parents. An item whose parents loop, or never reach
  the top, is an orphan.
- **A value that must never be lost,** such as a password, is never edited: each new value is a
  new item under its parent, and the datatype shows the newest. Two devices changing it at once both
  keep theirs.

### Clearing a collection

An item of datatype `sync.clear` in a collection means everything in that collection made before it is gone.
Clearing history is one such item, not one delete per visit.

## Keys

- **One data key per bucket** seals every private part. Devices keep it in the OS keychain.
- **Each passphrase seals its own copy** of the data key, as an item in the `keys` collection, datatype
  `sealed-key`. Its public part holds what a new device needs: the label, the data key's id, and
  the Argon2id settings and salt. Its private part is the data key sealed with XChaCha20-Poly1305
  under the passphrase's Argon2id key, with the data key's id as additional data.
- **Joining a device:** it pulls the `keys` collection, asks for a passphrase, opens a copy, and keeps
  the data key.
- **Adding, changing or removing a passphrase** is putting, editing or deleting its copy.
- **Changing the data key** after a leak means sealing every item again under a new key, as
  ordinary edits. Each private part names its key, so a device meeting a key it doesn't have
  asks for the passphrase again.

## The server

JSON over HTTPS. Signing in is the host's business: the server asks it which bucket a request
belongs to, and every bucket's items are apart.

| Call | What it does |
|---|---|
| `GET /v1` | `{"version": 1}` |
| `POST /v1/push` `{"items": [...]}` | push, below |
| `GET /v1/pull?after=&limit=&collection=` | pull, below |

Every answer to push and pull carries `store`: the bucket's store id.

### Push

Up to 1000 items, each up to 1 MB, stored in one write. The server has one writer, so the items
of one push get sequence numbers that run on with no gaps, and the next push gets the numbers
after them. Each bucket has its own counter. For each item, in order:

- **Stored** — there is no item with that guid, or the one there has the same or an earlier
  timestamp: `{"ok": true, "sequence": n}`. The server gives it the next sequence number, and sets
  `deleted_at` if it is deleted. A retry of an edit the server already has is stored again: the
  same edit with a new `sequence`.
- **Older** — the server holds a version with a later timestamp:
  `{"ok": false, "current": {...}}`. The device takes `current`.
- **Refused** — the item is wrong: `{"ok": false, "code": "...", "error": "..."}`. The code is
  `timestamp-ahead` (more than a day ahead of the server's clock), `too-big` or `invalid`; the
  error says what, for a person.

The answer is `{"store", "time", "results": [...]}`, with one result per item in the order sent.
`time` is the server's clock in milliseconds, so a device refused for `timestamp-ahead` can say
by how much its clock is wrong.

### Pull

The bucket's items with `sequence` greater than `after`, of every collection, lowest first, up to
`limit` (default 500, at most 1000): `{"store", "items": [...], "next": n, "more": bool}`. The
device keeps `next` and passes it as `after` next time. A collection another device starts using
just shows up. `collection=` limits a pull to one collection, for a device joining with a passphrase that
reads only `keys` first.

The pull goes by when the server stored an item, not by when it was made, so an old item pushed
back later still reaches every device.

### The store id

The server makes a store id for each bucket. It changes it, `server.ResetStore`, whenever it
may have lost any of that bucket's data: a wipe, a restore from a backup, a move to a new host.
A device that sees a store id it didn't sync with last pushes everything it has, live and
deleted, with the timestamps they were made with, and reads every collection from the start. Newest
wins settles the rest. A new device meanwhile gets what is there, then the rest as it arrives.

### Cleanup

The server removes items deleted more than a set time ago, such as 90 days. A device away longer
than that may keep items that were deleted meanwhile.

## A device's sync

1. Pull from the cursor, every collection at once. Take each item, unless this device's version has
   a later timestamp.
2. If the store id changed, mark everything pending and start again from the beginning.
3. Push everything changed here. For each `current` that comes back, take it.
4. If a push's sequence numbers start just after the cursor, nothing else reached the server in
   between, so move the cursor to the last of them: the device doesn't pull its own items back.
   Otherwise leave the cursor, and the next pull brings what came in between, with these items.
5. Keep going past refused items, and report them: they stay pending.
6. Keep the clock, the store id and the cursor.

## Staying compatible

- New collections and datatypes need no change to the server.
- Fields a reader doesn't know are kept and written back.
- A change that would break a version 1 client is version 2, under `/v2`.
