// Package bolt stores synckosh items in a bbolt file: a server.Store for the server and a
// client.Local for a device. Each is one file, and bbolt can copy it while it is in use.
package bolt

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"slices"
	"time"

	bbolt "go.etcd.io/bbolt"

	"github.com/debkosh/synckosh"
	"github.com/debkosh/synckosh/client"
	"github.com/debkosh/synckosh/server"
)

var (
	_ server.Store = (*Store)(nil)
	_ client.Local = (*Local)(nil)
)

func open(path string) (*bbolt.DB, error) {
	return bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
}

// sub returns a nested bbolt bucket, made if write is true; nil if it doesn't exist and write is false.
func sub(tx *bbolt.Tx, write bool, names ...string) (*bbolt.Bucket, error) {
	var b *bbolt.Bucket
	for i, n := range names {
		var next *bbolt.Bucket
		switch {
		case i == 0 && write:
			var err error
			if next, err = tx.CreateBucketIfNotExists([]byte(n)); err != nil {
				return nil, err
			}
		case i == 0:
			next = tx.Bucket([]byte(n))
		case write:
			var err error
			if next, err = b.CreateBucketIfNotExists([]byte(n)); err != nil {
				return nil, err
			}
		default:
			next = b.Bucket([]byte(n))
		}
		if next == nil {
			return nil, nil
		}
		b = next
	}
	return b, nil
}

func u64(n uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, n)
	return b
}

// Store is a server.Store. For each sync bucket, "b/<bucket>" holds the store id, the last
// sequence number, "seq" (sequence number to collection and guid, across collections) and, for
// each collection, "c/<collection>" with "items" (guid to item) and "seq" (sequence number to
// guid).
type Store struct{ db *bbolt.DB }

func OpenStore(path string) (*Store, error) {
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// StoreID reads the bucket's store id, and makes one in a write that checks again, so two
// callers on first use get the same id.
func (s *Store) StoreID(bucket string) (string, error) {
	var id string
	err := s.db.View(func(tx *bbolt.Tx) error {
		b, _ := sub(tx, false, "b", bucket)
		if b != nil {
			id = string(b.Get([]byte("store")))
		}
		return nil
	})
	if err != nil || id != "" {
		return id, err
	}
	err = s.db.Update(func(tx *bbolt.Tx) error {
		b, err := sub(tx, true, "b", bucket)
		if err != nil {
			return err
		}
		if id = string(b.Get([]byte("store"))); id != "" {
			return nil
		}
		id = server.NewStoreID()
		return b.Put([]byte("store"), []byte(id))
	})
	return id, err
}

func (s *Store) SetStoreID(bucket, id string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := sub(tx, true, "b", bucket)
		if err != nil {
			return err
		}
		return b.Put([]byte("store"), []byte(id))
	})
}

func (s *Store) ResetAll() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		buckets := tx.Bucket([]byte("b"))
		if buckets == nil {
			return nil
		}
		return buckets.ForEachBucket(func(name []byte) error {
			return buckets.Bucket(name).Put([]byte("store"), []byte(server.NewStoreID()))
		})
	})
}

// Update runs fn in one bbolt write. bbolt has one writer per file, so this is the bucket's
// only writer, and what fn puts is stored all together or not at all.
func (s *Store) Update(bucket string, fn func(server.Tx) error) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bkt, err := sub(tx, true, "b", bucket)
		if err != nil {
			return err
		}
		return fn(&storeTx{tx: tx, bucket: bucket, bkt: bkt})
	})
}

type storeTx struct {
	tx     *bbolt.Tx
	bucket string
	bkt    *bbolt.Bucket
}

func (t *storeTx) Last() (uint64, error) {
	if v := t.bkt.Get([]byte("last")); v != nil {
		return binary.BigEndian.Uint64(v), nil
	}
	return 0, nil
}

func (t *storeTx) Get(collection, guid string) (synckosh.Item, bool, error) {
	var it synckosh.Item
	b, _ := sub(t.tx, false, "b", t.bucket, "c", collection, "items")
	if b == nil {
		return it, false, nil
	}
	v := b.Get([]byte(guid))
	if v == nil {
		return it, false, nil
	}
	return it, true, json.Unmarshal(v, &it)
}

// Identity reads the collection's index of live items by identity.
func (t *storeTx) Identity(collection, identity string) (synckosh.Item, bool, error) {
	b, _ := sub(t.tx, false, "b", t.bucket, "c", collection, "identity")
	if b == nil || identity == "" {
		return synckosh.Item{}, false, nil
	}
	prefix := []byte(identity + "\x00")
	k, _ := b.Cursor().Seek(prefix)
	if k == nil || !bytes.HasPrefix(k, prefix) {
		return synckosh.Item{}, false, nil
	}
	return t.Get(collection, string(k[len(prefix):]))
}

// byIdentity is how a collection's identity index names a live item: its identity and guid.
func byIdentity(it synckosh.Item) []byte { return []byte(it.Identity + "\x00" + it.GUID) }

// indexed says whether an item is in its collection's identity index.
func indexed(it synckosh.Item) bool { return it.Identity != "" && !it.Deleted }

// ref is how the bucket-wide index names an item: its collection and guid.
func ref(collection, guid string) []byte { return []byte(collection + "\x00" + guid) }

func (t *storeTx) Put(items []synckosh.Item) error {
	all, err := t.bkt.CreateBucketIfNotExists([]byte("seq"))
	if err != nil {
		return err
	}
	last, err := t.Last()
	if err != nil {
		return err
	}
	for _, it := range items {
		bk, err := sub(t.tx, true, "b", t.bucket, "c", it.Collection)
		if err != nil {
			return err
		}
		byGUID, err := bk.CreateBucketIfNotExists([]byte("items"))
		if err != nil {
			return err
		}
		bySeq, err := bk.CreateBucketIfNotExists([]byte("seq"))
		if err != nil {
			return err
		}
		ids, err := bk.CreateBucketIfNotExists([]byte("identity"))
		if err != nil {
			return err
		}
		if old := byGUID.Get([]byte(it.GUID)); old != nil {
			var o synckosh.Item
			if err := json.Unmarshal(old, &o); err != nil {
				return err
			}
			if err := bySeq.Delete(u64(o.Sequence)); err != nil {
				return err
			}
			if err := all.Delete(u64(o.Sequence)); err != nil {
				return err
			}
			if indexed(o) {
				if err := ids.Delete(byIdentity(o)); err != nil {
					return err
				}
			}
		}
		if indexed(it) {
			if err := ids.Put(byIdentity(it), nil); err != nil {
				return err
			}
		}
		v, err := synckosh.Marshal(it)
		if err != nil {
			return err
		}
		if err := byGUID.Put([]byte(it.GUID), v); err != nil {
			return err
		}
		if err := bySeq.Put(u64(it.Sequence), []byte(it.GUID)); err != nil {
			return err
		}
		if err := all.Put(u64(it.Sequence), ref(it.Collection, it.GUID)); err != nil {
			return err
		}
		last = max(last, it.Sequence)
	}
	return t.bkt.Put([]byte("last"), u64(last))
}

func (t *storeTx) RemoveBefore(collection string, before int64) error {
	bk, _ := sub(t.tx, false, "b", t.bucket, "c", collection)
	if bk == nil {
		return nil
	}
	_, err := remove(t.bkt.Bucket([]byte("seq")), bk, func(it synckosh.Item) bool {
		return it.Timestamp <= before
	})
	return err
}

// remove removes a collection's items that gone says to, from the collection and from the
// bucket-wide index all. It returns how many.
func remove(all, collection *bbolt.Bucket, gone func(synckosh.Item) bool) (int, error) {
	byGUID, bySeq := collection.Bucket([]byte("items")), collection.Bucket([]byte("seq"))
	if byGUID == nil {
		return 0, nil
	}
	type removal struct{ guid, seq, identity []byte }
	var rm []removal
	err := byGUID.ForEach(func(guid, v []byte) error {
		var it synckosh.Item
		if err := json.Unmarshal(v, &it); err != nil {
			return err
		}
		if gone(it) {
			r := removal{guid: bytes.Clone(guid), seq: u64(it.Sequence)}
			if indexed(it) {
				r.identity = byIdentity(it)
			}
			rm = append(rm, r)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	ids := collection.Bucket([]byte("identity"))
	for _, g := range rm {
		if err := byGUID.Delete(g.guid); err != nil {
			return 0, err
		}
		if err := bySeq.Delete(g.seq); err != nil {
			return 0, err
		}
		if err := all.Delete(g.seq); err != nil {
			return 0, err
		}
		if g.identity != nil && ids != nil {
			if err := ids.Delete(g.identity); err != nil {
				return 0, err
			}
		}
	}
	return len(rm), nil
}

// Since reads one collection's own index when the filter names just one, and the bucket's
// otherwise, skipping what the filter leaves out.
func (s *Store) Since(bucket string, f synckosh.Filter, after uint64, fn func(synckosh.Item) bool) error {
	return s.db.View(func(tx *bbolt.Tx) error {
		read := func(collection, guid []byte) (bool, error) {
			if len(f.Collections) > 0 && !slices.Contains(f.Collections, string(collection)) {
				return true, nil
			}
			var it synckosh.Item
			bk, _ := sub(tx, false, "b", bucket, "c", string(collection), "items")
			if err := json.Unmarshal(bk.Get(guid), &it); err != nil {
				return false, err
			}
			if !f.Match(it) {
				return true, nil
			}
			return fn(it), nil
		}
		if len(f.Collections) == 1 {
			collection := f.Collections[0]
			bk, _ := sub(tx, false, "b", bucket, "c", collection, "seq")
			if bk == nil {
				return nil
			}
			c := bk.Cursor()
			for k, guid := c.Seek(u64(after + 1)); k != nil; k, guid = c.Next() {
				if more, err := read([]byte(collection), guid); err != nil || !more {
					return err
				}
			}
			return nil
		}
		all, _ := sub(tx, false, "b", bucket, "seq")
		if all == nil {
			return nil
		}
		c := all.Cursor()
		for k, r := c.Seek(u64(after + 1)); k != nil; k, r = c.Next() {
			b, guid, _ := bytes.Cut(r, []byte{0})
			if more, err := read(b, guid); err != nil || !more {
				return err
			}
		}
		return nil
	})
}

func (s *Store) Cleanup(deletedBefore int64) (int, error) {
	n := 0
	err := s.db.Update(func(tx *bbolt.Tx) error {
		buckets := tx.Bucket([]byte("b"))
		if buckets == nil {
			return nil
		}
		return buckets.ForEachBucket(func(name []byte) error {
			bkt := buckets.Bucket(name)
			collections := bkt.Bucket([]byte("c"))
			if collections == nil {
				return nil
			}
			return collections.ForEachBucket(func(name []byte) error {
				m, err := remove(bkt.Bucket([]byte("seq")), collections.Bucket(name), func(it synckosh.Item) bool {
					return it.Deleted && it.DeletedAt < deletedBefore
				})
				n += m
				return err
			})
		})
	})
	return n, err
}

// Local is a client.Local: "items/<collection>" (guid to entry), "pending/<collection>" (guid) and
// "meta".
type Local struct{ db *bbolt.DB }

func OpenLocal(path string) (*Local, error) {
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	return &Local{db: db}, nil
}

func (l *Local) Close() error { return l.db.Close() }

func (l *Local) Get(collection, guid string) (client.Entry, bool, error) {
	var e client.Entry
	var has bool
	err := l.db.View(func(tx *bbolt.Tx) error {
		b, _ := sub(tx, false, "items", collection)
		if b == nil {
			return nil
		}
		v := b.Get([]byte(guid))
		if v == nil {
			return nil
		}
		has = true
		return json.Unmarshal(v, &e)
	})
	return e, has, err
}

func (l *Local) Put(entries ...client.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	return l.db.Update(func(tx *bbolt.Tx) error {
		for _, e := range entries {
			items, err := sub(tx, true, "items", e.Item.Collection)
			if err != nil {
				return err
			}
			pending, err := sub(tx, true, "pending", e.Item.Collection)
			if err != nil {
				return err
			}
			v, err := synckosh.Marshal(e)
			if err != nil {
				return err
			}
			if err := items.Put([]byte(e.Item.GUID), v); err != nil {
				return err
			}
			if e.Pending {
				err = pending.Put([]byte(e.Item.GUID), nil)
			} else {
				err = pending.Delete([]byte(e.Item.GUID))
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (l *Local) Remove(collection string, guids ...string) error {
	if len(guids) == 0 {
		return nil
	}
	return l.db.Update(func(tx *bbolt.Tx) error {
		items, _ := sub(tx, false, "items", collection)
		if items == nil {
			return nil
		}
		pending, _ := sub(tx, false, "pending", collection)
		for _, g := range guids {
			if err := items.Delete([]byte(g)); err != nil {
				return err
			}
			if pending != nil {
				if err := pending.Delete([]byte(g)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (l *Local) Each(collection string, fn func(client.Entry) error) error {
	return l.db.View(func(tx *bbolt.Tx) error {
		each := func(b *bbolt.Bucket) error {
			return b.ForEach(func(_, v []byte) error {
				var e client.Entry
				if err := json.Unmarshal(v, &e); err != nil {
					return err
				}
				return fn(e)
			})
		}
		if collection != "" {
			b, _ := sub(tx, false, "items", collection)
			if b == nil {
				return nil
			}
			return each(b)
		}
		all := tx.Bucket([]byte("items"))
		if all == nil {
			return nil
		}
		return all.ForEachBucket(func(name []byte) error { return each(all.Bucket(name)) })
	})
}

func (l *Local) Pending() ([]client.Entry, error) {
	var out []client.Entry
	err := l.db.View(func(tx *bbolt.Tx) error {
		pending := tx.Bucket([]byte("pending"))
		if pending == nil {
			return nil
		}
		return pending.ForEachBucket(func(name []byte) error {
			items, _ := sub(tx, false, "items", string(name))
			return pending.Bucket(name).ForEach(func(guid, _ []byte) error {
				var e client.Entry
				if err := json.Unmarshal(items.Get(guid), &e); err != nil {
					return err
				}
				out = append(out, e)
				return nil
			})
		})
	})
	return out, err
}

func (l *Local) Meta(key string) ([]byte, error) {
	var v []byte
	err := l.db.View(func(tx *bbolt.Tx) error {
		if m := tx.Bucket([]byte("meta")); m != nil {
			if b := m.Get([]byte(key)); b != nil {
				v = bytes.Clone(b)
			}
		}
		return nil
	})
	return v, err
}

func (l *Local) SetMeta(key string, value []byte) error {
	return l.db.Update(func(tx *bbolt.Tx) error {
		m, err := tx.CreateBucketIfNotExists([]byte("meta"))
		if err != nil {
			return err
		}
		if value == nil {
			return m.Delete([]byte(key))
		}
		return m.Put([]byte(key), value)
	})
}
