package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sudiptadeb/synckosh"
)

// Entry is an item as a device keeps it: opened, and marked pending until the server has it.
// A locked entry is one sealed under a key this device doesn't hold, kept as it came.
type Entry struct {
	Item    synckosh.Item `json:"item"`
	Pending bool          `json:"pending,omitempty"`
	Locked  bool          `json:"locked,omitempty"`
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

// ErrLocked is returned for an item sealed under a key this device doesn't hold: it can't be
// read or changed here.
var ErrLocked = errors.New("sealed with a key this device doesn't have")

// Config is what a Replica needs. It syncs every collection: one the app or another device starts
// using needs no setup.
type Config struct {
	Client *Client
	Local  Local
	// Key seals every item, and every item from the server must be sealed with it. Without a
	// key everything is plain, and the server is trusted.
	Key *synckosh.DataKey
	// SealPublic lists the datatypes whose public part is sealed too, with a copy in the box, so
	// the server can't change it. Otherwise only the private part and the item's fields are.
	SealPublic map[string]bool
	Limits     synckosh.Limits // this device's; zero fields take synckosh.DefaultLimits
}

// Replica is one device's copy of a bucket. Its methods may be called from any goroutine,
// one at a time.
type Replica struct {
	mu         sync.Mutex
	c          *Client
	local      Local
	key        *synckosh.DataKey
	sealPublic map[string]bool
	limits     synckosh.Limits
	clock      *synckosh.Clock
}

// Open opens a device's replica, with its clock where it left off.
func Open(cfg Config) (*Replica, error) {
	r := &Replica{c: cfg.Client, local: cfg.Local, key: cfg.Key, sealPublic: cfg.SealPublic,
		limits: cfg.Limits.OrDefaults(), clock: synckosh.NewClock()}
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

// seal is an item as it goes to the server.
func (r *Replica) seal(it synckosh.Item) (synckosh.Item, error) {
	if r.key == nil {
		return it, nil
	}
	return r.key.Seal(it, r.sealPublic[it.Datatype])
}

// open is an item from the server as this device keeps it. It fails with ErrUnknownKey for an
// item sealed under another key, and otherwise for an item the server changed or forged.
func (r *Replica) open(it synckosh.Item) (synckosh.Item, error) {
	if r.key == nil {
		return it, nil
	}
	return r.key.Open(it)
}

// Put records edits made here, in one write. A new item gets a new guid; an existing one keeps
// its guid. Timestamps are set here. It returns the items as stored. If any item is invalid,
// too big to push once sealed, or locked, it returns an error and stores none of them.
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
		if old.Locked {
			return nil, fmt.Errorf("%s %s: %w", it.Collection, it.GUID, ErrLocked)
		}
		if has {
			it.Extra = old.Item.Extra
		}
		it.Deleted, it.Base, it.Sequence, it.DeletedAt = false, 0, 0, 0
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
	if e.Locked {
		return fmt.Errorf("%s %s: %w", collection, guid, ErrLocked)
	}
	it := e.Item
	it.Deleted, it.Base, it.Sequence, it.DeletedAt = true, 0, 0, 0
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
	sealed, err := r.seal(it)
	if err != nil {
		return err
	}
	b, err := synckosh.Marshal(sealed)
	if err != nil {
		return err
	}
	if len(b) > r.limits.ItemBytes {
		return fmt.Errorf("%s %s: %d bytes sealed, over the item limit of %d", it.Collection, it.GUID, len(b), r.limits.ItemBytes)
	}
	return nil
}

// Get returns an item, deleted or not, or ErrLocked for one this device can't open.
func (r *Replica) Get(collection, guid string) (synckosh.Item, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, has, err := r.local.Get(collection, guid)
	if err == nil && e.Locked {
		err = fmt.Errorf("%s %s: %w", collection, guid, ErrLocked)
	}
	return e.Item, has, err
}

// Items returns a collection's live items, in guid order, which is the order they were made.
// Locked items are left out.
func (r *Replica) Items(collection string) ([]synckosh.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []synckosh.Item
	var cleared int64
	err := r.local.Each(collection, func(e Entry) error {
		switch {
		case e.Locked:
		case e.Item.Datatype == ClearDatatype:
			cleared = max(cleared, e.Item.Timestamp)
		default:
			all = append(all, e.Item)
		}
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

// Locked returns a collection's items sealed under a key this device doesn't hold, as they
// came. Nothing in them is verified, so an app must not trust what they show.
func (r *Replica) Locked(collection string) ([]synckosh.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []synckosh.Item
	err := r.local.Each(collection, func(e Entry) error {
		if e.Locked {
			out = append(out, e.Item)
		}
		return nil
	})
	return out, err
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

// Rejected is an item from the server that this device didn't take: invalid, not sealed, or
// changed after it was sealed. A key holder didn't write it as it came.
type Rejected struct {
	Collection, GUID string
	Error            string
}

// RejectedError is what Sync returns when it rejected items from the server. Everything else
// was synced, and the rejected items changed nothing here.
type RejectedError struct {
	Items []Rejected
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("sync: rejected %d items from the server: %s %s: %s", len(e.Items),
		e.Items[0].Collection, e.Items[0].GUID, e.Items[0].Error)
}

// report gathers what one round of a Sync couldn't sync.
type report struct {
	refused  RefusedError
	rejected RejectedError
}

func (rp *report) reject(it synckosh.Item, err error) {
	rp.rejected.Items = append(rp.rejected.Items, Rejected{it.Collection, it.GUID, err.Error()})
}

func (rp *report) err() error {
	var errs []error
	if len(rp.refused.Items) > 0 {
		errs = append(errs, &rp.refused)
	}
	if len(rp.rejected.Items) > 0 {
		errs = append(errs, &rp.rejected)
	}
	if len(errs) == 1 {
		return errs[0]
	}
	return errors.Join(errs...)
}

// Sync pulls what is new, then pushes what changed here. If the server's store id has changed
// since this device last synced, the server may have lost data, so the device pushes all it has.
// If the server refused some items, Sync syncs the rest and returns a *RefusedError; if it
// rejected items from the server, a *RejectedError; when both, the two joined.
func (r *Replica) Sync(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for round := 0; round < maxRounds; round++ {
		rp := &report{}
		again, err := r.pull(ctx, rp)
		if err != nil {
			return err
		}
		restart, err := r.push(ctx, rp)
		if err != nil {
			return err
		}
		if err := r.saveClock(); err != nil {
			return err
		}
		if !again && !restart {
			return rp.err()
		}
	}
	return fmt.Errorf("sync: not settled after %d rounds", maxRounds)
}

// checkStore compares the server's store id with the one this device last synced with. On a
// new one, it marks everything here pending and reads from the start. Locked entries are not
// pushed: the devices that can open them hold the real copy.
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
		if !e.Locked {
			e.Pending = true
			all = append(all, e)
		}
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

func (r *Replica) pull(ctx context.Context, rp *report) (restart bool, err error) {
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
			e, ok, err := r.take(it, rp)
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
//
// An item sealed under a key this device doesn't hold is locked. It is unverified, so it has no
// effect: it is kept only where this device holds nothing it can open, or an older locked
// version, and any version this device can open replaces it. An item that is invalid, not
// sealed, or changed after it was sealed is rejected.
func (r *Replica) take(remote synckosh.Item, rp *report) (Entry, bool, error) {
	if err := synckosh.Valid(remote); err != nil {
		rp.reject(remote, err)
		return Entry{}, false, nil
	}
	opened, err := r.open(remote)
	locked := errors.Is(err, synckosh.ErrUnknownKey)
	if err != nil && !locked {
		rp.reject(remote, err)
		return Entry{}, false, nil
	}
	e, has, err := r.local.Get(remote.Collection, remote.GUID)
	if err != nil {
		return Entry{}, false, err
	}
	if locked {
		if has && (!e.Locked || e.Item.Timestamp > remote.Timestamp) {
			return Entry{}, false, nil
		}
		return Entry{Item: remote, Locked: true}, true, nil
	}
	r.clock.Observe(opened.Timestamp)
	if has && !e.Locked && e.Item.Timestamp > opened.Timestamp {
		return Entry{}, false, nil // this device's version is later, and is pushed
	}
	return Entry{Item: opened}, true, nil
}

// push sends what changed here, as many items at a time as the limits allow. When a push's
// sequence numbers follow straight on from the cursor, nothing else reached the server in
// between, so the cursor moves past them and this device doesn't pull its own items back.
// Otherwise the next pull brings what came in between, and these items with it.
func (r *Replica) push(ctx context.Context, rp *report) (restart bool, err error) {
	pending, err := r.local.Pending()
	if err != nil {
		return false, err
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Item.Timestamp < pending[j].Item.Timestamp })
	cur, err := r.cursor()
	if err != nil {
		return false, err
	}
	for len(pending) > 0 {
		n := min(len(pending), r.limits.PushItems)
		batch := pending[:n]
		pending = pending[n:]
		out := make([]synckosh.Item, n)
		for i, e := range batch {
			if out[i], err = r.seal(e.Item); err != nil {
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
				e, ok, err := r.take(*rs.Current, rp)
				if err != nil {
					return false, err
				}
				if ok {
					es = append(es, e)
				}
			default:
				it := batch[i].Item
				rp.refused.Items = append(rp.refused.Items, Refused{it.Collection, it.GUID, rs.Code, rs.Error})
				if rs.Code == synckosh.RefusedTimestampAhead {
					// Never stored anywhere, so it can take a new timestamp. While this device's
					// clock is wrong the new one is refused too; once it is right, it goes out.
					rp.refused.Ahead = max(rp.refused.Ahead, time.Duration(it.Timestamp-res.Time)*time.Millisecond)
					r.clock.Reset(res.Time)
					e := batch[i]
					e.Item.Timestamp = r.clock.Now()
					es = append(es, e)
				}
			}
		}
		if err := r.local.Put(es...); err != nil {
			return false, err
		}
		if lo == cur+1 {
			cur = hi
			if err := r.setCursor(cur); err != nil {
				return false, err
			}
		}
	}
	return false, nil
}

// Join returns a bucket's data key, opened with one of its passphrases, for the device to keep
// in its own key store. It reads the bucket's sealed copies and takes the first that opens and
// whose own seal checks out under the key it holds.
func Join(ctx context.Context, c *Client, passphrase string) (*synckosh.DataKey, error) {
	var after uint64
	for {
		p, err := c.Pull(ctx, synckosh.KeysCollection, after)
		if err != nil {
			return nil, err
		}
		for _, it := range p.Items {
			if it.Deleted || synckosh.Valid(it) != nil {
				continue
			}
			if key, err := synckosh.OpenSealedCopy(it, passphrase); err == nil {
				return key, nil
			}
		}
		if !p.More {
			return nil, synckosh.ErrPassphrase
		}
		after = p.Next
	}
}

// ErrHasKey is Create's answer for a bucket that already holds items: its devices hold a key,
// even when no sealed copy is left, so this device joins instead.
var ErrHasKey = errors.New("the bucket already holds items: join it with a passphrase")

// Create makes the data key of a new bucket and pushes its first sealed copy, under passphrase,
// returning the key for the device to keep in its own key store. It refuses with ErrHasKey if
// the bucket holds any item at all, so two first runs don't make two keys; two at the very same
// moment still can, and then each device sees the other's items as locked until one joins.
func Create(ctx context.Context, c *Client, passphrase, label string) (*synckosh.DataKey, error) {
	p, err := c.Pull(ctx, "", 0)
	if err != nil {
		return nil, err
	}
	if len(p.Items) > 0 {
		return nil, ErrHasKey
	}
	key, err := synckosh.NewDataKey()
	if err != nil {
		return nil, err
	}
	copy, err := key.SealedCopy(passphrase, label)
	if err != nil {
		return nil, err
	}
	copy.GUID, copy.Timestamp = synckosh.NewGUID(), time.Now().UnixMilli()
	sealed, err := key.Seal(copy, true)
	if err != nil {
		return nil, err
	}
	res, err := c.Push(ctx, []synckosh.Item{sealed})
	if err != nil {
		return nil, err
	}
	if rs := res.Results[0]; !rs.OK {
		return nil, fmt.Errorf("create: the server didn't store the sealed copy: %s %s", rs.Code, rs.Error)
	}
	return key, nil
}
