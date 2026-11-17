# synckosh protocol, version 1

How devices sync a bucket's data through a synckosh server. The Go packages in this repo are
the reference: `client` for a device, `server` for the server, `bolt` for storage on both.

## The model

Three levels name where an item lives:
- **A bucket** is one synced space with its own data key, sequence numbers and store id. It is
  whatever the host makes it: one person's data, a shared list, a family's. The server is told
  which bucket a request is for, and buckets never see each other.
- **A collection** is a kind of data inside a bucket: `bookmarks`, `logins`, `history`.
- **A datatype** is what one item is: `folder`, `bookmark`, `todo`.

- **Everything is an item.** An item has a collection, a datatype, a guid, a timestamp, a deleted flag,
  a public part and a private part.
- **For each guid, the item with the newest timestamp wins;** in the same millisecond, the one
  the server stored last. That is all the server does with items. It never opens a private part.
- **Sync knows only items.** The protocol has no schema: what an item holds, and how items relate,
  is the app's choice. Nothing is ever deleted because of another item, but for a collection's
  delete-before.
- **Deletes are updates.** The server keeps a deleted item, marked deleted and with its data, so
  every device hears of it, until its cleanup removes it long after. A device removes the item once
  the server holds the delete, unless it is set to keep deleted items.
- **The devices hold the real copy.** If the server loses anything, devices push it back.

## The item

```json
{
  "collection": "todos",
  "datatype": "todo",
  "guid": "019a8f3c-5e21-7b40-9c3d-2f6a1e8b4d07",
  "timestamp": 1763380800000,
  "deleted": false,
  "public": {"parent": "019a8f3c-5e1f-7a10-8b2e-1c5d0f3a9e42", "order": "V"},
  "private": {"key": "k_sUwGzUYSPw", "box": "7lg3iKfl2s0W…"},
  "base": 40,
  "sequence": 42,
  "deleted_at": 1763390000000
}
```

| Field | Set by | What it is |
|---|---|---|
| `collection` | device | the kind of data: `bookmarks`, `logins`, `history`. The server learns of collections only from items. Lowercase letters, digits, `.`, `-`, `_`, up to 64, starting with a letter or digit. |
| `datatype` | device | what the item is, for the app: `folder`, `bookmark`, `todo`. Same characters. |
| `guid` | device | a UUIDv7 for a new item ([RFC 9562](https://www.rfc-editor.org/rfc/rfc9562)), so guids sort by when they were made. An edit keeps the guid. Letters, digits, `._:~-`, up to 128, not starting with `_`. |
| `timestamp` | device | when the edit was made: the device's clock, in milliseconds. |
| `deleted` | device | true for a deleted item. |
| `public` | device | plain JSON, readable by the server and by a device that hasn't unlocked yet. |
| `private` | device | JSON sealed with the bucket's data key, below. |
| `base` | device | the `sequence` of the version this edit was made from; absent for a new item. |
| `sequence` | server | the server's sequence number for this version: the order it was stored in, from the bucket's own counter, which only goes up. |
| `deleted_at` | server | the server's clock, in milliseconds, when it first stored the item deleted. |

Fields a reader doesn't know are kept and written back unchanged, by the server and by clients.

### Reserved names

Names that start with `_` belong to the protocol; an app's names can't start with one. The
collection `_sealedkeys` holds the data key's sealed copies, of datatype `_sealedkey`, and
nothing else. A collection's delete-before has datatype and guid `_deletebefore`, is never
deleted, and its public part is `{"before": <milliseconds>}`. A server refuses, and a device neither writes nor takes, an item whose names break
these rules.

### Timestamps

A timestamp is a plain number of milliseconds. A device's timestamps:
- are always after every one it has made or read: its clock, or one millisecond past the newest
  it has seen when its clock is behind;
- don't follow a timestamp more than an hour ahead of its own clock.

All the items of one write get one timestamp, so writing many items doesn't move the clock ahead.
The server refuses items more than an hour ahead of its own clock, with the code
`timestamp-ahead` and its own time. The device then moves its clock back to the server's time
and gives the refused items a new timestamp: they were never stored anywhere, so that is safe.
While its clock is still wrong they are refused again; once it is right, they go out.

Two devices that haven't seen each other's edit can make them in the same millisecond. Then the
one the server stores last wins: the server stores an edit unless it holds one with a later
timestamp, and a device takes what it pulls unless its own version has a later timestamp. Every
device pulls the server's copy, so they all end up with the same one. Nothing depends on which
device made an edit.

### Public and private parts

The app decides, for each datatype, what goes in the public part and what in the private part.

When a bucket has a data key, every item is sealed, an empty private part included, with
XChaCha20-Poly1305 under the data key. The wire's `private` is then
`{"key": "<data key id>", "box": "<base64>"}`, with `"public": true` added when the box holds a
copy of the public part. Base64 is the standard alphabet without padding; the box is the
24-byte nonce followed by the ciphertext.

- **The box** opens to the private part's JSON, or `null` when it has none. Sealed with its
  public part, it opens to the JSON array `[public, private]` instead, each `null` when absent.
- **The additional data** is the ASCII text of seven lines joined by `\n`, with no newline at the
  end: collection, datatype, guid, timestamp in decimal, deleted as `1` or `0`, base in decimal or
  empty when absent, and `1` or `0` for whether the box holds the public part. Valid names hold no
  newline, so the text can be read only one way.
- **A device that opens an item** sealed with its public part uses the copy from the box and
  ignores the one outside, which is there for the server and for devices that can't open the item.
  Without the copy, the public part outside is not checked: the server could change it unnoticed,
  but nothing else.
- **The library's own items**, whose datatypes start with `_`, are always sealed with their public
  part, and a device refuses one that isn't.

A device that holds the key takes from the server only items it can open:
- **Rejected:** an item that is invalid, not sealed, or whose seal doesn't open under the key: the
  server or someone else changed or forged it. It is not stored, Sync reports it, and the cursor
  moves on.
- **Locked:** an item sealed under a key the device doesn't hold. It is unverified, so it has no
  effect. It is kept only where the device holds nothing it can open, or an older locked version;
  otherwise it is passed over. A version the device can open always replaces a locked one,
  whatever the timestamps. A locked item is not a delete, is never merged, and is never pushed,
  not even in a refill. The app can't edit it, and must not trust what it shows.

Without a data key nothing is sealed, every part is plain, and the server is trusted. A device
with no key pointed at a sealed bucket writes plain items, which the devices that hold the key
reject.

### What the app decides

The protocol knows items and nothing else. How an app shapes its data is its own choice: one item
per record, or a record split across several items, with or without parents. A few facts hold
whatever it chooses:

- **Newest wins per item,** unless the datatype has a merger, below. Without one, two devices
  editing the same item keep only the later edit; edits to different items all survive.
- **A parent is just a field.** An item may name another item in its public part. Sync never
  follows it. An item whose parent is missing, deleted or part of a loop is still alive, and the
  app decides how to show it.
- **Order is a field too.** Two devices can give two items the same order key. `synckosh.Between`
  is an optional helper that makes a key between two others.

### Merging

The server keeps one version per item and every device ends on it, so convergence never depends
on merging. A merge is only a new edit that some device pushes, and devices with different
mergers, or none, still converge.

`base` is set when an edit is made: the `sequence` of the stored version it was made from; for an
edit of an edit not yet sent, that one's `base`; none for a new item. It is sealed, so a server
can't start a merge by changing it. A refill clears it on every entry.

A device that holds version L of an item and receives version R from the server, in a pull or as
`current`, both openable, decides in this order:

1. **R is the device's own earlier push**, whose answer it never saw. Before a push goes out, the
   device keeps the timestamp each entry is sent with; an edit of that entry carries it over. If
   R has that timestamp and L is still that version, the device takes R. If L was edited since, it
   keeps L, sets L's `base` to R's `sequence`, and forgets the sent timestamp.
2. **They never met** when either L is an edit not yet sent, with a `base`, and R's `sequence` is
   above it; or L is stored, R's `sequence` is above L's, R's timestamp is not before L's, and R
   has a `base` below L's `sequence`.
3. If they never met, both have the same datatype, and that datatype has a merger: when the two,
   or the merger's result and R, have the same deleted flag, public and private parts, the device
   takes R. Otherwise it stores the result as a new edit: timestamp from its clock, after R's;
   `base` R's `sequence`; pending.
4. In every other case newest wins: the device takes R unless L has a later timestamp.

A merger gets no common ancestor, so it is right only for data that can be joined without one.

One race stays open. Device B pulls; A pushes V; B pushes W, stored over V. The server now holds
only W, and B never saw V. A device that pulled V merges when it pulls W; but if a third device
edits W and pushes before any holder of V pulls, V's change is lost unmerged, which is newest wins.
A store reset between one sync's pull and its push lets edits carry old-store `base` numbers onto
the new store, where a device may merge once for no reason, or miss one merge.

### Deleting before a cutoff

A collection's delete-before deletes every item of the collection whose timestamp is at or before
its cutoff, `before`. Deleting a browser's history is one such item, not one delete per visit.

- **There is one per collection**, an ordinary item for newest wins: its timestamp is when it was
  written. A device writing one uses the later of the cutoff there and the one asked for, so the
  cutoff only moves forward, and the library always seals it with its public part.
- **The server reads it:** it is the one item whose content the server interprets. On storing one,
  in the same write, it removes every item of the collection at or before the cutoff; the
  delete-before itself stays. From then on it answers a pushed item of the collection at or before
  the cutoff `{"ok": true}`, with no sequence number, and doesn't store it, so a refill or a device
  away for long can't bring it back. It answers a delete-before with an earlier cutoff than the one
  it holds with `current`. It refuses one whose cutoff is more than the ahead limit past its clock,
  as `timestamp-ahead`.
- **A device** that writes or receives one moves its clock past the cutoff, so its later edits come
  after it, and removes its entries at or before the cutoff; set to keep deleted items, it keeps
  them, reported as deleted. An edit of such an item gets a timestamp past the cutoff and syncs as
  usual.

## Keys

- **One data key per bucket** seals every item. Devices keep it in the OS keychain.
- **The data key is never shown.** It never leaves a device's key store: no export, no QR code,
  no copy on screen. Anything a person sees, writes down or scans, such as a backup code or a QR
  code to join a device, is a random passphrase with its own sealed copy, so it can be removed on
  its own.
- **Each passphrase seals its own copy** of the data key, as an item in the `_sealedkeys`
  collection, datatype `_sealedkey`. Its public part is what a device that hasn't unlocked can
  read: `{"label", "key", "kdf", "t", "m", "p", "salt", "box"}`. `key` is the data key's id; `kdf`
  is `argon2id`, with `t` passes, `m` KiB of memory and `p` lanes, over the base64 `salt`, making
  32 bytes; `box` is the data key's 32 bytes sealed with XChaCha20-Poly1305 under those 32 bytes,
  with the data key's id as additional data. The copy is then sealed with the data key like any
  item, with its public part.
- **Joining a device:** it pulls `_sealedkeys`, asks for a passphrase, and for a live copy checks
  the settings first, since it can't verify them yet: `kdf` is `argon2id`, `1 <= t <= 10`,
  `1 <= p <= 16`, `8p <= m <= 262144`, and the salt is 8 to 64 bytes. It opens `box` with the
  passphrase, then checks the copy's own seal with the key it got, which proves a holder of that
  key wrote the copy. Then it keeps the key.
- **The first device** makes the key only when the bucket holds no item at all, and pushes the
  key's first sealed copy. A bucket that holds items has a key on its devices even when no copy is
  left. Two devices that both start a bucket at the very same moment make two keys; each then sees
  the other's items as locked, and the fix is to join one device to the other.
- **A backup or join code** is a random passphrase of 128 bits with its own sealed copy.
- **Adding, changing or removing a passphrase** is putting, editing or deleting its copy. Removing
  one doesn't change the data key.
- **Changing the data key** after a leak is not part of version 1.

## What sealing protects

When a bucket has a data key, the server, and anyone who controls it or the connection, cannot read
a private part. It cannot make a device that holds the key use anything a key holder did not
write: it cannot forge, alter or move an item, mark one deleted, or make a device delete a
collection's items before a cutoff. Public parts are checked only
for items sealed with their public part; for the others, the server can change the public part.

An item sealed under a key the device doesn't hold is kept as locked. A locked item is
unverified: it changes nothing the device holds and is never sent on, and an app must not trust
what it shows.

The server can still see collection and datatype names, guids, timestamps, public parts, sizes and
timing. It can withhold items or stop answering. It can hand a device an older version that a key
holder really wrote: a new device may get old data or miss deletes, and a merger may merge an old
version back in. It can try passphrases offline against the sealed copies, so a passphrase a
person chose protects the data only as well as it was chosen; backup and join codes are random,
128 bits. Removing a passphrase does not change the data key. Every device that holds the key can
write anything. Without a key, everything is plain and the server is trusted.

The seal leaves out `sequence`, `deleted_at`, the answers' `store` and `next`, and fields a reader
doesn't know. Lying about them is withholding or replay, which the server can do anyway. A field
that matters goes into the additional data, which is a new protocol version.

If a person uses one passphrase for two buckets, a server can hand a device joining one the sealed
copy of the other. The device then sees its real bucket as locked.

## The server

JSON over HTTPS. Signing in is the host's business: the server asks it which bucket a request
belongs to, and every bucket's items are apart.

| Call | What it does |
|---|---|
| `GET /v1` | `{"version": 1}` |
| `POST /v1/push` `{"items": [...]}` | push, below |
| `GET /v1/pull?after=&collection=&bytes=` | pull, below |

Every answer to push and pull carries `store`: the bucket's store id.

### Push

As many items as the server's limits allow, stored in one write. A push is its bucket's only
writer while it runs, so the items of one push get sequence numbers that run on with no gaps, and
the next push gets the numbers after them. Each bucket has its own counter, which never goes
back, even when cleanup removes the item that had the highest number. For each item, in order:

- **Deleted already** — the item is at or before its collection's delete-before cutoff:
  `{"ok": true}`, with no sequence number. The server doesn't store it, and the device marks it sent.
- **Stored** — there is no item with that guid, or the one there has the same or an earlier
  timestamp: `{"ok": true, "sequence": n}`. The server gives it the next sequence number, and sets
  `deleted_at` if it is deleted. A retry of an edit the server already has is stored again: the
  same edit with a new `sequence`.
- **Older** — the server holds a version with a later timestamp:
  `{"ok": false, "current": {...}}`. The device takes `current`.
- **Refused** — the item is wrong: `{"ok": false, "code": "...", "error": "..."}`. The code is
  `timestamp-ahead` (more than an hour ahead of the server's clock), `too-big` (over the server's
  item limit) or `invalid`; the error says what, for a person.

A push over the server's item count or bytes is refused whole, with status 413 and
`{"error": "...", "limits": {"item_bytes", "push_items", "push_bytes", "page_bytes"}}`. The device
then fills its pushes to the lower of its limits and the server's, and carries on.

The answer is `{"store", "time", "results": [...]}`, with one result per item in the order sent.
`time` is the server's clock in milliseconds, so a device refused for `timestamp-ahead` can say
by how much its clock is wrong.

### Pull

The bucket's items with `sequence` greater than `after`, of every collection, lowest first, a page
at a time: `{"store", "items": [...], "next": n, "more": bool, "time": ms, "cleanup": ms}`. `time` is
the server's clock, and `cleanup` the age after which it removes deleted items, absent if it never
does. A page holds items up to the lower
of the server's page bytes and `bytes`, the most the device reads, but always at least one item. The
device keeps `next` and passes it as `after` next time. A collection another device starts using
just shows up. `collection=` limits a pull to one collection, for a device joining with a passphrase that
reads only `_sealedkeys` first.

The pull goes by when the server stored an item, not by when it was made, so an old item pushed
back later still reaches every device.

### The store id

The server makes a store id for each bucket. It changes it, `server.ResetStore`, whenever it
may have lost any of that bucket's data: a wipe, a restore from a backup, a move to a new host.
After restoring the whole store from a backup, `server.ResetAll` changes every bucket's.
A device that sees a store id it didn't sync with last pushes everything it has, live and
deleted, with the timestamps they were made with, and reads every collection from the start. Newest
wins settles the rest. To make that survive a crash, the device marks every entry it can open
pending, clears every entry's `sequence` and `base`, which mean nothing on the new store, resets
its cursor, and saves the new store id last: a crash anywhere before repeats the refill. A new device meanwhile gets what is there, then the rest as it arrives.
A device that removed deleted items has none to push, so after a reset an item can come back if a
device that never saw its delete still holds it, unless some device keeps deleted items.

### Limits

Each side sets its own limits. The defaults:

| Limit | Default |
|---|---|
| one item, as sealed JSON on the wire | 1 MiB |
| items in one push | 1000 |
| bytes in one push request | 16 MiB |
| bytes of items in one pull page | 16 MiB |

A device refuses an edit over its own item limit when it is made, rather than leave it pending.
The server refuses an item over its item limit as `too-big`; it stays pending and is reported. A
device fills each push by its item count and bytes, and reads a page up to its page bytes, plus
the one item a page always holds. Neither side's limits can stop the other's sync.

### Cleanup

The server removes items deleted more than a set time ago, such as 90 days. A device away longer
than that may hold items whose deletes were removed meanwhile, which no pull can bring it, so it
starts over. It keeps the `time` of its last finished pull. The store id is checked first: on a
new one the device refills, stale or not, as the server may have lost everything and this device
may hold the only copy. Only when the store id is unchanged, and a pull answer's `time` is more
than `cleanup` after the kept time, does the device start over: it removes every entry that isn't
pending, resets its cursor, clears the kept time last, and syncs as a new device would. Its edits
not yet sent go out as usual.

A stale device that refills a reset server can bring back items deleted long ago, which is far
better than losing the bucket; so can a device that went away in the middle of a refill. A server
clock that jumps forward past the cleanup age makes every device start over, which costs
bandwidth and nothing else.

A device removes nothing because of the server's cleanup. One set to keep deleted items keeps them
until the app removes those deleted, or before a cutoff, longer ago than an age it chooses.

## A device's sync

1. Pull from the cursor, every collection at once. Take each item, unless this device's version has
   a later timestamp.
2. If the store id changed, refill: mark everything pending, store id last, and start again from
   the beginning. Otherwise, if this device is away longer than the cleanup age, start over.
3. Push everything changed here. For each `current` that comes back, take it.
4. If a push's sequence numbers start just after the cursor, nothing else reached the server in
   between, so move the cursor to the last of them: the device doesn't pull its own items back.
   Otherwise leave the cursor, and the next pull brings what came in between, with these items.
   Results with no sequence number don't count.
5. Keep going past refused items, and report them: they stay pending.
6. Keep the clock, the store id, the cursor and the time of the last finished pull.

## Staying compatible

- New collections and datatypes need no change to the server.
- Fields a reader doesn't know are kept and written back.
- A change that would break a version 1 client is version 2, under `/v2`.
