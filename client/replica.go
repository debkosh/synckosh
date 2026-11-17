package client

import (
	"bytes"
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
// A locked entry is one sealed under a key this device doesn't hold, kept as it came. Sent is
// the timestamp the entry was last pushed with, until the device sees the server's answer: a
// version with that timestamp is this device's own push, even when the answer was lost.
type Entry struct {
	Item    synckosh.Item `json:"item"`
	Pending bool          `json:"pending,omitempty"`
	Locked  bool          `json:"locked,omitempty"`
	Sent    int64         `json:"sent,omitempty"`
}

// Merger joins two versions of one item that never saw each other: one edited here, or held
// here, and one from the server. It gets no common ancestor, so it is only right for data that
// can be joined without one: fields that carry their own clocks, or sets that only grow. A
// merger that unions plain fields brings deletions back. It must be a pure function, and should
// give the same content whichever version is local. Only Deleted, Public and Private of the
// result are used.
type Merger func(local, remote synckosh.Item) synckosh.Item

// Local is a device's own store. It is the real copy of the data: if the server loses
// anything, devices push it back from here. Package bolt has one.
type Local interface {
	Get(collection, guid string) (Entry, bool, error)
	Put(entries ...Entry) error
	Remove(collection string, guids ...string) error
	// Each calls fn for every entry of a collection, or of every collection if collection is empty.
	Each(collection string, fn func(Entry) error) error
	// Pending returns the entries of every collection that the server doesn't have yet.
	Pending() ([]Entry, error)
	Meta(key string) ([]byte, error) // nil if unset
	SetMeta(key string, value []byte) error
}

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
	// KeepDeleted keeps deleted items here, with their data, until the app calls Cleanup:
	// Items hides them, Get reports them deleted, and putting one again restores it. Without it,
	// a device removes an item once the server holds its delete, and removes a collection's
	// items at or before a delete-before cutoff as soon as it writes or receives it.
	KeepDeleted bool
	// Mergers join concurrent versions, by datatype. A datatype with none keeps the newest.
	Mergers map[string]Merger
	Limits  synckosh.Limits // this device's; zero fields take synckosh.DefaultLimits
}

// Replica is one device's copy of a bucket. Its methods may be called from any goroutine,
// one at a time.
type Replica struct {
	mu          sync.Mutex
	c           *Client
	local       Local
	key         *synckosh.DataKey
	sealPublic  map[string]bool
	keepDeleted bool
	mergers     map[string]Merger
	limits      synckosh.Limits
	clock       *synckosh.Clock
}

// Open opens a device's replica, with its clock where it left off.
func Open(cfg Config) (*Replica, error) {
	r := &Replica{c: cfg.Client, local: cfg.Local, key: cfg.Key, sealPublic: cfg.SealPublic,
		keepDeleted: cfg.KeepDeleted, mergers: cfg.Mergers, limits: cfg.Limits.OrDefaults(), clock: synckosh.NewClock()}
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
		if it.Datatype == synckosh.DeleteBeforeItem {
			return nil, errors.New("put: a delete-before is written with DeleteBefore")
		}
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
		it.Deleted, it.Sequence, it.DeletedAt = false, 0, 0
		out[i] = it
	}
	if err := r.write(out...); err != nil {
		return nil, err
	}
	return out, nil
}

// Delete marks an item deleted. The server keeps it, with its data, until its cleanup. This
// device removes it once the server holds the delete, or with KeepDeleted keeps it, so putting it
// again restores it.
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
	it.Deleted, it.Sequence, it.DeletedAt = true, 0, 0
	return r.write(it)
}

// DeleteBefore deletes every item of a collection with a timestamp at or before before, on every
// device, in one item: the collection's delete-before. The server forgets those items at once,
// and stores none at or before the cutoff again. The cutoff only moves forward: an earlier one
// than the collection has changes nothing. An item edited after the cutoff lives as usual.
func (r *Replica) DeleteBefore(collection string, before time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := before.UnixMilli()
	if cutoff > time.Now().Add(synckosh.MaxAhead).UnixMilli() {
		return fmt.Errorf("delete before %s: more than %s ahead", before, synckosh.MaxAhead)
	}
	old, has, err := r.local.Get(collection, synckosh.DeleteBeforeItem)
	if err != nil {
		return err
	}
	if old.Locked {
		return fmt.Errorf("%s %s: %w", collection, synckosh.DeleteBeforeItem, ErrLocked)
	}
	it := synckosh.Item{Collection: collection, Datatype: synckosh.DeleteBeforeItem, GUID: synckosh.DeleteBeforeItem}
	if has {
		held, _ := synckosh.Cutoff(old.Item)
		cutoff = max(cutoff, held)
		it.Extra = old.Item.Extra
	}
	it.Public, _ = json.Marshal(map[string]int64{"before": cutoff})
	if err := r.write(it); err != nil {
		return err
	}
	return r.cut(it)
}

// cut follows a delete-before this device wrote or took. Its clock moves past the cutoff, so an
// edit made from now on is after it; and unless the device keeps deleted items, it removes the
// collection's entries at or before it.
func (r *Replica) cut(it synckosh.Item) error {
	cutoff, err := synckosh.Cutoff(it)
	if err != nil {
		return err
	}
	r.clock.Observe(cutoff)
	if r.keepDeleted {
		return nil
	}
	var gone []string
	err = r.local.Each(it.Collection, func(e Entry) error {
		if e.Item.GUID != synckosh.DeleteBeforeItem && e.Item.Timestamp <= cutoff {
			gone = append(gone, e.Item.GUID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return r.local.Remove(it.Collection, gone...)
}

// cutoff is a collection's delete-before cutoff, 0 if it has none this device can open.
func (r *Replica) cutoff(collection string) (int64, error) {
	e, has, err := r.local.Get(collection, synckosh.DeleteBeforeItem)
	if err != nil || !has || e.Locked {
		return 0, err
	}
	return synckosh.Cutoff(e.Item)
}

// write stores edits made here, all with one timestamp, so a write of many items doesn't move
// the clock ahead of the wall clock.
//
// Each edit's base is the sequence number of the stored version it was made from. An edit of an
// edit not yet sent keeps that one's base, and its Sent, as the server may hold it already.
func (r *Replica) write(its ...synckosh.Item) error {
	ts := r.clock.Now()
	es := make([]Entry, len(its))
	for i := range its {
		e := Entry{Pending: true}
		its[i].Base = 0
		old, has, err := r.local.Get(its[i].Collection, its[i].GUID)
		switch {
		case err != nil:
			return err
		case has && old.Pending:
			its[i].Base, e.Sent = old.Item.Base, old.Sent
		case has:
			its[i].Base = old.Item.Sequence
		}
		its[i].Timestamp = ts
		if err := r.check(its[i]); err != nil {
			return err
		}
		e.Item = its[i]
		es[i] = e
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

// Get returns an item, deleted or not, or ErrLocked for one this device can't open. An item at
// or before its collection's delete-before cutoff is reported deleted.
func (r *Replica) Get(collection, guid string) (synckosh.Item, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, has, err := r.local.Get(collection, guid)
	if err != nil || !has {
		return synckosh.Item{}, false, err
	}
	if e.Locked {
		return e.Item, true, fmt.Errorf("%s %s: %w", collection, guid, ErrLocked)
	}
	cutoff, err := r.cutoff(collection)
	if guid != synckosh.DeleteBeforeItem && e.Item.Timestamp <= cutoff {
		e.Item.Deleted = true
	}
	return e.Item, true, err
}

// Items returns a collection's live items, in guid order, which is the order they were made.
// Deleted and locked items are left out, and so are items at or before the collection's
// delete-before cutoff.
func (r *Replica) Items(collection string) ([]synckosh.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff, err := r.cutoff(collection)
	if err != nil {
		return nil, err
	}
	var out []synckosh.Item
	err = r.local.Each(collection, func(e Entry) error {
		if !e.Locked && !e.Item.Deleted && e.Item.GUID != synckosh.DeleteBeforeItem && e.Item.Timestamp > cutoff {
			out = append(out, e.Item)
		}
		return nil
	})
	return out, err
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

// Cleanup removes this device's entries deleted longer ago than age: by when the server stored
// the delete if this device knows it, else by the delete's timestamp; for items at or before a
// delete-before cutoff, by when the delete-before was written. Pending entries stay, as the
// server doesn't have them yet. A device holds deleted entries only with KeepDeleted, and nothing
// removes them but this: the app decides whether and when to call it. It returns how many it
// removed.
func (r *Replica) Cleanup(age time.Duration) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := time.Now().Add(-age).UnixMilli()
	cuts := map[string]int64{} // by collection: the cutoff of a delete-before written before then
	err := r.local.Each("", func(e Entry) error {
		if !e.Locked && e.Item.GUID == synckosh.DeleteBeforeItem && e.Item.Timestamp < before {
			cuts[e.Item.Collection], _ = synckosh.Cutoff(e.Item)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	gone := map[string][]string{}
	n := 0
	err = r.local.Each("", func(e Entry) error {
		it := e.Item
		deletedAt := it.DeletedAt
		if deletedAt == 0 {
			deletedAt = it.Timestamp
		}
		if !e.Pending && !e.Locked && it.GUID != synckosh.DeleteBeforeItem &&
			(it.Deleted && deletedAt < before || it.Timestamp <= cuts[it.Collection]) {
			gone[it.Collection] = append(gone[it.Collection], it.GUID)
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for collection, guids := range gone {
		if err := r.local.Remove(collection, guids...); err != nil {
			return 0, err
		}
	}
	return n, nil
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
// new one, the server may have lost data, so the device refills it. The store id is saved last:
// a crash anywhere before repeats the refill.
func (r *Replica) checkStore(id string) (bool, error) {
	old, err := r.local.Meta("store")
	if err != nil {
		return false, err
	}
	if string(old) == id {
		return false, nil
	}
	if old != nil { // not the first sync
		if err := r.refill(); err != nil {
			return false, err
		}
	}
	return old != nil, r.local.SetMeta("store", []byte(id))
}

// refill marks every entry this device can open pending, to push it all back, and reads from
// the start. Locked entries are not pushed: the devices that can open them hold the real copy.
// Sequence numbers of the old store mean nothing on the new one, so every entry loses its
// sequence, base and Sent, and a refill never merges.
func (r *Replica) refill() error {
	var all []Entry
	err := r.local.Each("", func(e Entry) error {
		e.Pending = e.Pending || !e.Locked
		e.Item.Sequence, e.Item.Base, e.Sent = 0, 0, 0
		all = append(all, e)
		return nil
	})
	if err != nil {
		return err
	}
	if err := r.local.Put(all...); err != nil {
		return err
	}
	return r.setCursor(0)
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

// changes are what one page, or one push's answer, does to this device's entries, stored
// together.
type changes struct {
	put  []Entry
	gone map[string][]string // by collection: deleted, and this device doesn't keep deletes
}

func (ch *changes) remove(it synckosh.Item) {
	if ch.gone == nil {
		ch.gone = map[string][]string{}
	}
	ch.gone[it.Collection] = append(ch.gone[it.Collection], it.GUID)
}

// keep stores an entry, or removes it if it is deleted and this device doesn't keep deletes.
func (r *Replica) keep(ch *changes, e Entry) {
	if e.Item.Deleted && !e.Pending && !e.Locked && !r.keepDeleted {
		ch.remove(e.Item)
		return
	}
	ch.put = append(ch.put, e)
}

func (r *Replica) apply(ch changes) error {
	if err := r.local.Put(ch.put...); err != nil {
		return err
	}
	for collection, guids := range ch.gone {
		if err := r.local.Remove(collection, guids...); err != nil {
			return err
		}
	}
	for _, e := range ch.put {
		if !e.Locked && e.Item.GUID == synckosh.DeleteBeforeItem {
			if err := r.cut(e.Item); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Replica) pull(ctx context.Context, rp *report) (restart bool, err error) {
	after, err := r.cursor()
	if err != nil {
		return false, err
	}
	for {
		p, err := r.c.Pull(ctx, "", after, r.limits.PageBytes)
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
		var ch changes
		for _, it := range p.Items {
			if err := r.take(it, rp, &ch); err != nil {
				return false, err
			}
		}
		if err := r.apply(ch); err != nil {
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

// take decides what an item from the server does here: it is taken, unless this device has a
// version with a later timestamp. One made in the same millisecond is taken: the server's copy is
// the one pushed last, and every device ends up with it.
//
// An item sealed under a key this device doesn't hold is locked. It is unverified, so it has no
// effect: it is kept only where this device holds nothing it can open, or an older locked
// version, and any version this device can open replaces it. An item that is invalid, not
// sealed, or changed after it was sealed is rejected.
func (r *Replica) take(remote synckosh.Item, rp *report, ch *changes) error {
	if err := synckosh.Valid(remote); err != nil {
		rp.reject(remote, err)
		return nil
	}
	opened, err := r.open(remote)
	locked := errors.Is(err, synckosh.ErrUnknownKey)
	if err == nil {
		err = synckosh.Valid(opened) // a delete-before's cutoff, from the box
	}
	if err != nil && !locked {
		rp.reject(remote, err)
		return nil
	}
	e, has, err := r.local.Get(remote.Collection, remote.GUID)
	if err != nil {
		return err
	}
	if locked {
		if !has || e.Locked && e.Item.Timestamp <= remote.Timestamp {
			ch.put = append(ch.put, Entry{Item: remote, Locked: true})
		}
		return nil
	}
	r.clock.Observe(opened.Timestamp)
	if !has || e.Locked {
		r.keep(ch, Entry{Item: opened})
		return nil
	}
	if e.Sent != 0 && opened.Timestamp == e.Sent {
		// This device's own push, whose answer was lost: they have met.
		if e.Item.Timestamp == e.Sent {
			r.keep(ch, Entry{Item: opened})
		} else {
			e.Item.Base, e.Sent = opened.Sequence, 0 // edited again since
			ch.put = append(ch.put, e)
		}
		return nil
	}
	if merge := r.mergers[opened.Datatype]; merge != nil && neverMet(e, opened) && e.Item.Datatype == opened.Datatype {
		m := merge(e.Item, opened)
		if sameContent(e.Item, opened) || sameContent(m, opened) {
			r.keep(ch, Entry{Item: opened})
			return nil
		}
		// A new edit, after the remote one and made from it.
		it := opened
		it.Deleted, it.Public, it.Private = m.Deleted, m.Public, m.Private
		it.Timestamp, it.Base, it.Sequence, it.DeletedAt = r.clock.Now(), opened.Sequence, 0, 0
		ch.put = append(ch.put, Entry{Item: it, Pending: true})
		return nil
	}
	if e.Item.Timestamp > opened.Timestamp {
		return nil // this device's version is later, and is pushed
	}
	r.keep(ch, Entry{Item: opened})
	return nil
}

// neverMet says whether a version from the server was made without seeing the one held here:
// the held one is an edit not yet sent, made from a version before the remote one; or it is
// stored, and the remote one, newer, was made from a version before it. A version with no base,
// a new item or one pushed back in a refill, never merges.
func neverMet(local Entry, remote synckosh.Item) bool {
	if local.Pending {
		return local.Item.Base != 0 && remote.Sequence > local.Item.Base
	}
	return local.Item.Sequence != 0 && remote.Sequence > local.Item.Sequence &&
		remote.Timestamp >= local.Item.Timestamp && remote.Base != 0 && remote.Base < local.Item.Sequence
}

// sameContent says whether two versions hold the same: then a merge makes no new edit, so
// devices merging the same pair don't push one each.
func sameContent(a, b synckosh.Item) bool {
	return a.Deleted == b.Deleted && sameJSON(a.Public, b.Public) && sameJSON(a.Private, b.Private)
}

func sameJSON(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// push sends what changed here, as many items at a time as the limits allow, this device's and,
// once it has refused a push as too large, the server's. When a push's
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
		var out []synckosh.Item
		size := len(`{"items":[]}`)
		for len(out) < len(pending) && len(out) < r.limits.PushItems {
			sealed, err := r.seal(pending[len(out)].Item)
			if err != nil {
				return false, err
			}
			b, err := synckosh.Marshal(sealed)
			if err != nil {
				return false, err
			}
			if len(out) > 0 && size+len(b)+1 > r.limits.PushBytes {
				break
			}
			size += len(b) + 1
			out = append(out, sealed)
		}
		batch := pending[:len(out)]
		for i := range batch {
			batch[i].Sent = batch[i].Item.Timestamp
		}
		if err := r.local.Put(batch...); err != nil {
			return false, err
		}
		res, err := r.c.Push(ctx, out)
		var tooLarge *TooLargeError
		if errors.As(err, &tooLarge) && r.fit(tooLarge.Limits) {
			continue // split to fit, and carry on
		}
		if err != nil {
			return false, err
		}
		pending = pending[len(out):]
		changed, err := r.checkStore(res.Store)
		if err != nil {
			return false, err
		}
		if changed {
			return true, nil
		}
		var ch changes
		var lo, hi uint64
		for i, rs := range res.Results {
			switch {
			case rs.OK:
				// Stored, or, with no sequence number, at or before the collection's
				// delete-before cutoff, which the server doesn't store again.
				e := batch[i]
				e.Item.Sequence, e.Pending, e.Sent = rs.Sequence, false, 0
				r.keep(&ch, e)
				if rs.Sequence == 0 {
					continue
				}
				if lo == 0 || rs.Sequence < lo {
					lo = rs.Sequence
				}
				hi = max(hi, rs.Sequence)
			case rs.Current != nil:
				// The server holds a later version: it wins, here too.
				if err := r.take(*rs.Current, rp, &ch); err != nil {
					return false, err
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
					e.Item.Timestamp, e.Sent = r.clock.Now(), 0
					ch.put = append(ch.put, e)
				}
			}
		}
		if err := r.apply(ch); err != nil {
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

// fit lowers this device's push limits to the server's, and says whether that changed them: if
// not, splitting can't help.
func (r *Replica) fit(server synckosh.Limits) bool {
	l := r.limits
	if server.PushItems > 0 {
		l.PushItems = min(l.PushItems, server.PushItems)
	}
	if server.PushBytes > 0 {
		l.PushBytes = min(l.PushBytes, server.PushBytes)
	}
	changed := l != r.limits
	r.limits = l
	return changed
}

// Join returns a bucket's data key, opened with one of its passphrases, for the device to keep
// in its own key store. It reads the bucket's sealed copies and takes the first that opens and
// whose own seal checks out under the key it holds.
func Join(ctx context.Context, c *Client, passphrase string) (*synckosh.DataKey, error) {
	var after uint64
	for {
		p, err := c.Pull(ctx, synckosh.KeysCollection, after, 0)
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
	p, err := c.Pull(ctx, "", 0, 0)
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
