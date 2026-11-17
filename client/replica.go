package client

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

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
	Get(collection, guid string) (Entry, bool, error)
	Put(entries ...Entry) error
	// Each calls fn for every entry of a collection, or of every collection if collection is empty.
	Each(collection string, fn func(Entry) error) error
	// Pending returns the entries of every collection that the server doesn't have yet.
	Pending() ([]Entry, error)
	Meta(key string) ([]byte, error) // nil if unset
	SetMeta(key string, value []byte) error
}

// ClearDatatype is the datatype of an item that clears a collection: every item in the collection older than
// it is gone. Clearing history is one such item, not one delete per visit.
const ClearDatatype = "sync.clear"

// Config is what a Replica needs. It syncs every collection: one the app or another device starts
// using needs no setup.
type Config struct {
	Client *Client
	Local  Local
	Key    *synckosh.DataKey
	Limits synckosh.Limits // this device's; zero fields take synckosh.DefaultLimits
}

// Replica is one device's copy of a bucket. Its methods may be called from any goroutine,
// one at a time.
type Replica struct {
	mu     sync.Mutex
	c      *Client
	local  Local
	key    *synckosh.DataKey
	limits synckosh.Limits
	clock  *synckosh.Clock
}

// Open opens a device's replica, with its clock where it left off.
func Open(cfg Config) (*Replica, error) {
	r := &Replica{c: cfg.Client, local: cfg.Local, key: cfg.Key, limits: cfg.Limits.OrDefaults(), clock: synckosh.NewClock()}
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
// its guid. Timestamps are set here. It returns the items as stored. If any item is invalid, or
// too big to push once sealed, it returns an error and stores none of them.
func (r *Replica) Put(its ...synckosh.Item) ([]synckosh.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]synckosh.Item, len(its))
	for i, it := range its {
		if it.GUID == "" {
			it.GUID = synckosh.NewGUID()
		}
		old, has, err := r.local.Get(it.Collection, it.GUID)
		if err != nil {
			return nil, err
		}
		if has {
			it.Extra = old.Item.Extra
		}
		it.Deleted, it.Sequence, it.DeletedAt = false, 0, 0
		out[i] = it
	}
	if err := r.write(out...); err != nil {
		return nil, err
	}
	return out, nil
}

// Delete marks an item deleted. It keeps its data, so it can be restored by putting it again,
// until the server's cleanup removes it.
func (r *Replica) Delete(collection, guid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, has, err := r.local.Get(collection, guid)
	if err != nil || !has || e.Item.Deleted {
		return err
	}
	it := e.Item
	it.Deleted, it.Sequence, it.DeletedAt = true, 0, 0
	return r.write(it)
}

// Clear removes everything in a collection made before now, on every device.
func (r *Replica) Clear(collection string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.write(synckosh.Item{Collection: collection, Datatype: ClearDatatype, GUID: synckosh.NewGUID()})
}

// write stores edits made here, all with one timestamp, so a write of many items doesn't move
// the clock ahead of the wall clock.
func (r *Replica) write(its ...synckosh.Item) error {
	ts := r.clock.Now()
	es := make([]Entry, len(its))
	for i := range its {
		its[i].Timestamp = ts
		if err := r.check(its[i]); err != nil {
			return err
		}
		es[i] = Entry{Item: its[i], Pending: true}
	}
	if err := r.local.Put(es...); err != nil {
		return err
	}
	return r.saveClock()
}

// check refuses an edit the server would refuse for good: an invalid name, or an item over this
// device's item limit once sealed. Left pending, it would be refused on every Sync.
func (r *Replica) check(it synckosh.Item) error {
	if err := synckosh.Valid(it); err != nil {
		return fmt.Errorf("%s %s: %w", it.Collection, it.GUID, err)
	}
	sealed, err := r.key.Seal(it)
	if err != nil {
		return err
	}
	b, err := json.Marshal(sealed)
	if err != nil {
		return err
	}
	if len(b) > r.limits.ItemBytes {
		return fmt.Errorf("%s %s: %d bytes sealed, over the item limit of %d", it.Collection, it.GUID, len(b), r.limits.ItemBytes)
	}
	return nil
}

// Get returns an item, deleted or not.
func (r *Replica) Get(collection, guid string) (synckosh.Item, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, has, err := r.local.Get(collection, guid)
	return e.Item, has, err
}

// Items returns a collection's live items, in guid order, which is the order they were made.
func (r *Replica) Items(collection string) ([]synckosh.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []synckosh.Item
	var cleared int64
	err := r.local.Each(collection, func(e Entry) error {
		if e.Item.Datatype == ClearDatatype {
			cleared = max(cleared, e.Item.Timestamp)
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
		if !it.Deleted && it.Timestamp > cleared {
			out = append(out, it)
		}
	}
	return out, nil
}

// maxRounds bounds the pull and push rounds of one Sync. A round repeats only when the server
// changed its store id.
const maxRounds = 5

// Refused is a pushed item the server refused. It stays pending, and goes out again on the
// next Sync.
type Refused struct {
	Collection, GUID string
	Code             string // one of synckosh.Refused...
	Error            string
}

// RefusedError is what Sync returns when the server refused some items. Everything else was
// synced. When the code is synckosh.RefusedTimestampAhead, this device's clock is wrong: the
// app can tell the user by how much with Ahead. Once the clock is right, the next Sync sends
// those items with a new timestamp.
type RefusedError struct {
	Items []Refused
	Ahead time.Duration // how far the refused timestamps were ahead of the server's clock
}

func (e *RefusedError) Error() string {
	for _, it := range e.Items {
		if it.Code == synckosh.RefusedTimestampAhead {
			return fmt.Sprintf("sync: the server refused %d items: this device's clock is %s ahead of the server's",
				len(e.Items), e.Ahead.Round(time.Minute))
		}
	}
	return fmt.Sprintf("sync: the server refused %d items: %s %s: %s", len(e.Items),
		e.Items[0].Collection, e.Items[0].GUID, e.Items[0].Error)
}

// Sync pulls what is new, then pushes what changed here. If the server's store id has changed
// since this device last synced, the server may have lost data, so the device pushes all it has.
// If the server refused some items, Sync syncs the rest and returns a *RefusedError.
func (r *Replica) Sync(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for round := 0; round < maxRounds; round++ {
		again, err := r.pull(ctx)
		if err != nil {
			return err
		}
		restart, refused, err := r.push(ctx)
		if err != nil {
			return err
		}
		if err := r.saveClock(); err != nil {
			return err
		}
		if !again && !restart {
			if refused != nil {
				return refused
			}
			return nil
		}
	}
	return fmt.Errorf("sync: not settled after %d rounds", maxRounds)
}

// checkStore compares the server's store id with the one this device last synced with. On a
// new one, it marks everything here pending and reads from the start.
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
	var all []Entry
	if err := r.local.Each("", func(e Entry) error {
		e.Pending = true
		all = append(all, e)
		return nil
	}); err != nil {
		return false, err
	}
	if err := r.local.Put(all...); err != nil {
		return false, err
	}
	return true, r.setCursor(0)
}

// The cursor is the sequence number of the last item this device has from the server.
func (r *Replica) cursor() (uint64, error) {
	b, err := r.local.Meta("after")
	if err != nil || b == nil {
		return 0, err
	}
	var n uint64
	err = json.Unmarshal(b, &n)
	return n, err
}

func (r *Replica) setCursor(n uint64) error {
	b, _ := json.Marshal(n)
	return r.local.SetMeta("after", b)
}

func (r *Replica) pull(ctx context.Context) (restart bool, err error) {
	after, err := r.cursor()
	if err != nil {
		return false, err
	}
	for {
		p, err := r.c.Pull(ctx, "", after)
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
		if err := r.setCursor(after); err != nil {
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
	r.clock.Observe(remote.Timestamp)
	e, has, err := r.local.Get(remote.Collection, remote.GUID)
	if err != nil {
		return Entry{}, false, err
	}
	if has && e.Item.Timestamp > remote.Timestamp {
		return Entry{}, false, nil // this device's version is later, and is pushed
	}
	return Entry{Item: remote}, true, nil
}

// push sends what changed here, as many items at a time as the limits allow. When a push's sequence numbers follow
// straight on from the cursor, nothing else reached the server in between, so the cursor moves
// past them and this device doesn't pull its own items back. Otherwise the next pull brings
// what came in between, and these items with it.
func (r *Replica) push(ctx context.Context) (restart bool, refused *RefusedError, err error) {
	pending, err := r.local.Pending()
	if err != nil {
		return false, nil, err
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Item.Timestamp < pending[j].Item.Timestamp })
	cur, err := r.cursor()
	if err != nil {
		return false, nil, err
	}
	for len(pending) > 0 {
		n := min(len(pending), r.limits.PushItems)
		batch := pending[:n]
		pending = pending[n:]
		out := make([]synckosh.Item, n)
		for i, e := range batch {
			if out[i], err = r.key.Seal(e.Item); err != nil {
				return false, nil, err
			}
		}
		res, err := r.c.Push(ctx, out)
		if err != nil {
			return false, nil, err
		}
		changed, err := r.checkStore(res.Store)
		if err != nil {
			return false, nil, err
		}
		if changed {
			return true, nil, nil
		}
		var es []Entry
		var lo, hi uint64
		for i, rs := range res.Results {
			switch {
			case rs.OK:
				e := batch[i]
				e.Item.Sequence, e.Pending = rs.Sequence, false
				es = append(es, e)
				if lo == 0 || rs.Sequence < lo {
					lo = rs.Sequence
				}
				hi = max(hi, rs.Sequence)
			case rs.Current != nil:
				// The server holds a later version: it wins, here too.
				e, ok, err := r.take(*rs.Current)
				if err != nil {
					return false, nil, err
				}
				if ok {
					es = append(es, e)
				}
			default:
				if refused == nil {
					refused = &RefusedError{}
				}
				it := batch[i].Item
				refused.Items = append(refused.Items, Refused{it.Collection, it.GUID, rs.Code, rs.Error})
				if rs.Code == synckosh.RefusedTimestampAhead {
					// Never stored anywhere, so it can take a new timestamp. While this device's
					// clock is wrong the new one is refused too; once it is right, it goes out.
					refused.Ahead = max(refused.Ahead, time.Duration(it.Timestamp-res.Time)*time.Millisecond)
					r.clock.Reset(res.Time)
					e := batch[i]
					e.Item.Timestamp = r.clock.Now()
					es = append(es, e)
				}
			}
		}
		if err := r.local.Put(es...); err != nil {
			return false, nil, err
		}
		if lo == cur+1 {
			cur = hi
			if err := r.setCursor(cur); err != nil {
				return false, nil, err
			}
		}
	}
	return false, refused, nil
}
