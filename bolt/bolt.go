// Package bolt stores synckosh items in a bbolt file: a server.Store for the server and a
// client.Local for a device. Each is one file, and bbolt can copy it while it is in use.
package bolt

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"time"

	bbolt "go.etcd.io/bbolt"

	"github.com/sudiptadeb/synckosh"
	"github.com/sudiptadeb/synckosh/client"
	"github.com/sudiptadeb/synckosh/server"
)

var (
	_ server.Store = (*Store)(nil)
	_ client.Local = (*Local)(nil)
)

func open(path string) (*bbolt.DB, error) {
	return bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
}

// sub returns a nested bucket, made if write is true; nil if it doesn't exist and write is false.
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

// Store is a server.Store. Per account, "a/<account>" holds the store id, the last sequence
// number, "seq" (sequence number to bucket and guid, across buckets) and, for each sync bucket,
// "b/<bucket>" with "items" (guid to item) and "seq" (sequence number to guid).
type Store struct{ db *bbolt.DB }

func OpenStore(path string) (*Store, error) {
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) StoreID(account string) (string, error) {
	var id string
	err := s.db.View(func(tx *bbolt.Tx) error {
		b, _ := sub(tx, false, "a", account)
		if b != nil {
			id = string(b.Get([]byte("store")))
		}
		return nil
	})
	if err != nil || id != "" {
		return id, err
	}
	id = server.NewStoreID()
	return id, s.SetStoreID(account, id)
}

func (s *Store) SetStoreID(account, id string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := sub(tx, true, "a", account)
		if err != nil {
			return err
		}
		return b.Put([]byte("store"), []byte(id))
	})
}

func (s *Store) Get(account, bucket, guid string) (synckosh.Item, bool, error) {
	var it synckosh.Item
	var has bool
	err := s.db.View(func(tx *bbolt.Tx) error {
		b, _ := sub(tx, false, "a", account, "b", bucket, "items")
		if b == nil {
			return nil
		}
		v := b.Get([]byte(guid))
		if v == nil {
			return nil
		}
		has = true
		return json.Unmarshal(v, &it)
	})
	return it, has, err
}

// ref is how the account-wide index names an item: its bucket and guid.
func ref(bucket, guid string) []byte { return []byte(bucket + "\x00" + guid) }

func (s *Store) Put(account string, items []synckosh.Item) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		acct, err := sub(tx, true, "a", account)
		if err != nil {
			return err
		}
		all, err := acct.CreateBucketIfNotExists([]byte("seq"))
		if err != nil {
			return err
		}
		var last uint64
		if v := acct.Get([]byte("last")); v != nil {
			last = binary.BigEndian.Uint64(v)
		}
		for _, it := range items {
			bk, err := sub(tx, true, "a", account, "b", it.Bucket)
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
			}
			v, err := json.Marshal(it)
			if err != nil {
				return err
			}
			if err := byGUID.Put([]byte(it.GUID), v); err != nil {
				return err
			}
			if err := bySeq.Put(u64(it.Sequence), []byte(it.GUID)); err != nil {
				return err
			}
			if err := all.Put(u64(it.Sequence), ref(it.Bucket, it.GUID)); err != nil {
				return err
			}
			last = max(last, it.Sequence)
		}
		return acct.Put([]byte("last"), u64(last))
	})
}

func (s *Store) Since(account, bucket string, after uint64, limit int) ([]synckosh.Item, error) {
	var out []synckosh.Item
	err := s.db.View(func(tx *bbolt.Tx) error {
		read := func(bucket, guid []byte) error {
			var it synckosh.Item
			bk, _ := sub(tx, false, "a", account, "b", string(bucket), "items")
			if err := json.Unmarshal(bk.Get(guid), &it); err != nil {
				return err
			}
			out = append(out, it)
			return nil
		}
		if bucket != "" {
			bk, _ := sub(tx, false, "a", account, "b", bucket, "seq")
			if bk == nil {
				return nil
			}
			c := bk.Cursor()
			for k, guid := c.Seek(u64(after + 1)); k != nil && len(out) < limit; k, guid = c.Next() {
				if err := read([]byte(bucket), guid); err != nil {
					return err
				}
			}
			return nil
		}
		all, _ := sub(tx, false, "a", account, "seq")
		if all == nil {
			return nil
		}
		c := all.Cursor()
		for k, r := c.Seek(u64(after + 1)); k != nil && len(out) < limit; k, r = c.Next() {
			b, guid, _ := bytes.Cut(r, []byte{0})
			if err := read(b, guid); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func (s *Store) LastSequence(account string) (uint64, error) {
	var last uint64
	err := s.db.View(func(tx *bbolt.Tx) error {
		if acct, _ := sub(tx, false, "a", account); acct != nil {
			if v := acct.Get([]byte("last")); v != nil {
				last = binary.BigEndian.Uint64(v)
			}
		}
		return nil
	})
	return last, err
}

func (s *Store) Cleanup(deletedBefore int64) (int, error) {
	n := 0
	err := s.db.Update(func(tx *bbolt.Tx) error {
		accounts := tx.Bucket([]byte("a"))
		if accounts == nil {
			return nil
		}
		return accounts.ForEachBucket(func(name []byte) error {
			acct := accounts.Bucket(name)
			buckets, all := acct.Bucket([]byte("b")), acct.Bucket([]byte("seq"))
			if buckets == nil {
				return nil
			}
			return buckets.ForEachBucket(func(name []byte) error {
				bk := buckets.Bucket(name)
				byGUID, bySeq := bk.Bucket([]byte("items")), bk.Bucket([]byte("seq"))
				var gone [][2][]byte
				err := byGUID.ForEach(func(guid, v []byte) error {
					var it synckosh.Item
					if err := json.Unmarshal(v, &it); err != nil {
						return err
					}
					if it.Deleted && it.DeletedAt < deletedBefore {
						gone = append(gone, [2][]byte{bytes.Clone(guid), u64(it.Sequence)})
					}
					return nil
				})
				if err != nil {
					return err
				}
				for _, g := range gone {
					if err := byGUID.Delete(g[0]); err != nil {
						return err
					}
					if err := bySeq.Delete(g[1]); err != nil {
						return err
					}
					if err := all.Delete(g[1]); err != nil {
						return err
					}
				}
				n += len(gone)
				return nil
			})
		})
	})
	return n, err
}

// Local is a client.Local: "items/<bucket>" (guid to entry), "pending/<bucket>" (guid) and
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

func (l *Local) Get(bucket, guid string) (client.Entry, bool, error) {
	var e client.Entry
	var has bool
	err := l.db.View(func(tx *bbolt.Tx) error {
		b, _ := sub(tx, false, "items", bucket)
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
			items, err := sub(tx, true, "items", e.Item.Bucket)
			if err != nil {
				return err
			}
			pending, err := sub(tx, true, "pending", e.Item.Bucket)
			if err != nil {
				return err
			}
			v, err := json.Marshal(e)
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

func (l *Local) Each(bucket string, fn func(client.Entry) error) error {
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
		if bucket != "" {
			b, _ := sub(tx, false, "items", bucket)
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
