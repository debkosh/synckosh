package client

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/sudiptadeb/synckosh"
)

// Entry is an item as a device keeps it: opened, and marked pending until the server has it.
type Entry struct {
	Item    synckosh.Item `json:"item"`
	Pending bool          `json:"pending,omitempty"`
}

// Local is a device's own store. It is the real copy of the data: if the server loses
// anything, devices push it back from here. Package bolt has one.
type Local interface {
	Get(bucket, guid string) (Entry, bool, error)
	Put(entries ...Entry) error
	// Each calls fn for every entry of a bucket, in guid order.
	Each(bucket string, fn func(Entry) error) error
	Pending(bucket string) ([]Entry, error)
	Meta(key string) ([]byte, error) // nil if unset
	SetMeta(key string, value []byte) error
}

// ClearType is the type of an item that clears a bucket: every item in the bucket older than
// it is gone. Clearing history is one such item, not one delete per visit.
const ClearType = "sync.clear"

// Config is what a Replica needs.
type Config struct {
	Client  *Client
	Local   Local
	Key     *synckosh.DataKey
	Buckets []string // the buckets this device syncs
}

// Replica is one device's copy of an account. Its methods may be called from any goroutine,
// one at a time.
type Replica struct {
	mu    sync.Mutex
	c     *Client
	local Local
	key   *synckosh.DataKey
	clock *synckosh.Clock
	bkts  []string
}

// Open opens a device's replica, with its clock where it left off.
func Open(cfg Config) (*Replica, error) {
	r := &Replica{c: cfg.Client, local: cfg.Local, key: cfg.Key, clock: synckosh.NewClock(),
		bkts: append([]string(nil), cfg.Buckets...)}
	b, err := r.local.Meta("clock")
	if err != nil {
		return nil, err
	}
	if b != nil {
		var last int64
		if err := json.Unmarshal(b, &last); err != nil {
			return nil, fmt.Errorf("clock: %w", err)
		}
		r.clock.Observe(last)
	}
	return r, nil
}

func (r *Replica) saveClock() error {
	b, _ := json.Marshal(r.clock.Last())
	return r.local.SetMeta("clock", b)
}

// Put records edits made here, in one write. A new item gets a new guid; an existing one keeps
// its guid. Timestamps are set here. It returns the items as stored.
func (r *Replica) Put(its ...synckosh.Item) ([]synckosh.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, it := range its {
		if it.GUID == "" {
			it.GUID = synckosh.NewGUID()
		}
		old, has, err := r.local.Get(it.Bucket, it.GUID)
		if err != nil {
			return nil, err
		}
		if has {
			it.Extra = old.Item.Extra
		}
		it.Deleted, it.Seen, it.DeletedAt = false, 0, 0
		its[i] = it
	}
	return its, r.write(its...)
}

// Delete marks an item deleted. It keeps its data, so it can be restored by putting it again,
// until the server's cleanup removes it.
func (r *Replica) Delete(bucket, guid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, has, err := r.local.Get(bucket, guid)
	if err != nil || !has || e.Item.Deleted {
		return err
	}
	it := e.Item
	it.Deleted, it.Seen, it.DeletedAt = true, 0, 0
	return r.write(it)
}

// Clear removes everything in a bucket made before now, on every device.
func (r *Replica) Clear(bucket string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.write(synckosh.Item{Bucket: bucket, Type: ClearType, GUID: synckosh.NewGUID()})
}

// write stores edits made here, all with one timestamp, so a write of many items doesn't move
// the clock ahead of the wall clock.
func (r *Replica) write(its ...synckosh.Item) error {
	ts := r.clock.Now()
	es := make([]Entry, len(its))
	for i := range its {
		its[i].TS = ts
		es[i] = Entry{Item: its[i], Pending: true}
	}
	if err := r.local.Put(es...); err != nil {
		return err
	}
	return r.saveClock()
}

// Get returns an item, deleted or not.
func (r *Replica) Get(bucket, guid string) (synckosh.Item, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, has, err := r.local.Get(bucket, guid)
	return e.Item, has, err
}

// Items returns a bucket's live items, in guid order, which is the order they were made.
func (r *Replica) Items(bucket string) ([]synckosh.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []synckosh.Item
	var cleared int64
	err := r.local.Each(bucket, func(e Entry) error {
		if e.Item.Type == ClearType {
			cleared = max(cleared, e.Item.TS)
			return nil
		}
		all = append(all, e.Item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, it := range all {
		if !it.Deleted && it.TS > cleared {
			out = append(out, it)
		}
	}
	return out, nil
}

// maxRounds bounds the pull and push rounds of one Sync. A round repeats only when the server
// changed its store id.
const maxRounds = 5

// Sync pulls every bucket, then pushes what changed here. If the server's store id has changed
// since this device last synced, the server may have lost data, so the device pushes all it has.
func (r *Replica) Sync(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for round := 0; round < maxRounds; round++ {
		again := false
		for _, b := range r.bkts {
			restart, err := r.pull(ctx, b)
			if err != nil {
				return err
			}
			again = again || restart
		}
		restart, err := r.push(ctx)
		if err != nil {
			return err
		}
		if err := r.saveClock(); err != nil {
			return err
		}
		if !again && !restart {
			return nil
		}
	}
	return fmt.Errorf("sync: not settled after %d rounds", maxRounds)
}

// checkStore compares the server's store id with the one this device last synced with. On a
// new one, it marks everything here pending and reads every bucket from the start.
func (r *Replica) checkStore(id string) (bool, error) {
	old, err := r.local.Meta("store")
	if err != nil {
		return false, err
	}
	if string(old) == id {
		return false, nil
	}
	if err := r.local.SetMeta("store", []byte(id)); err != nil {
		return false, err
	}
	if old == nil {
		return false, nil // the first sync
	}
	for _, b := range r.bkts {
		var all []Entry
		if err := r.local.Each(b, func(e Entry) error {
			e.Pending = true
			all = append(all, e)
			return nil
		}); err != nil {
			return false, err
		}
		if err := r.local.Put(all...); err != nil {
			return false, err
		}
		if err := r.local.SetMeta("after:"+b, nil); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (r *Replica) cursor(bucket string) (uint64, error) {
	b, err := r.local.Meta("after:" + bucket)
	if err != nil || b == nil {
		return 0, err
	}
	var n uint64
	err = json.Unmarshal(b, &n)
	return n, err
}

func (r *Replica) pull(ctx context.Context, bucket string) (restart bool, err error) {
	after, err := r.cursor(bucket)
	if err != nil {
		return false, err
	}
	for {
		p, err := r.c.Pull(ctx, bucket, after)
		if err != nil {
			return false, err
		}
		changed, err := r.checkStore(p.Store)
		if err != nil {
			return false, err
		}
		if changed {
			restart, after = true, 0
			continue
		}
		var es []Entry
		for _, it := range p.Items {
			e, ok, err := r.take(it)
			if err != nil {
				return false, err
			}
			if ok {
				es = append(es, e)
			}
		}
		if err := r.local.Put(es...); err != nil {
			return false, err
		}
		after = p.Next
		b, _ := json.Marshal(after)
		if err := r.local.SetMeta("after:"+bucket, b); err != nil {
			return false, err
		}
		if !p.More {
			return restart, nil
		}
	}
}

// take returns the entry to store for an item from the server, unless this device has a version
// with a later timestamp. One made in the same millisecond is taken: the server's copy is the
// one pushed last, and every device ends up with it.
func (r *Replica) take(remote synckosh.Item) (Entry, bool, error) {
	remote, err := r.key.Open(remote)
	if err != nil {
		return Entry{}, false, err
	}
	r.clock.Observe(remote.TS)
	e, has, err := r.local.Get(remote.Bucket, remote.GUID)
	if err != nil {
		return Entry{}, false, err
	}
	if has && e.Item.TS > remote.TS {
		return Entry{}, false, nil // this device's version is later, and is pushed
	}
	return Entry{Item: remote}, true, nil
}

func (r *Replica) push(ctx context.Context) (restart bool, err error) {
	var pending []Entry
	for _, b := range r.bkts {
		p, err := r.local.Pending(b)
		if err != nil {
			return false, err
		}
		pending = append(pending, p...)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Item.TS < pending[j].Item.TS })
	for len(pending) > 0 {
		n := min(len(pending), 1000)
		batch := pending[:n]
		pending = pending[n:]
		out := make([]synckosh.Item, n)
		for i, e := range batch {
			if out[i], err = r.key.Seal(e.Item); err != nil {
				return false, err
			}
		}
		res, err := r.c.Push(ctx, out)
		if err != nil {
			return false, err
		}
		changed, err := r.checkStore(res.Store)
		if err != nil {
			return false, err
		}
		if changed {
			return true, nil
		}
		var es []Entry
		for i, rs := range res.Results {
			switch {
			case rs.OK:
				e := batch[i]
				e.Item.Seen, e.Pending = rs.Seen, false
				es = append(es, e)
			case rs.Current != nil:
				// The server holds a later version: it wins, here too.
				e, ok, err := r.take(*rs.Current)
				if err != nil {
					return false, err
				}
				if ok {
					es = append(es, e)
				}
			default:
				return false, fmt.Errorf("push %s %s: %s", batch[i].Item.Bucket, batch[i].Item.GUID, rs.Error)
			}
		}
		if err := r.local.Put(es...); err != nil {
			return false, err
		}
	}
	return false, nil
}
