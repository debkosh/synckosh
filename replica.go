package synckosh

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

type key struct{ typ, guid string }

// Replica is one device's copy of the synced items, in memory. A store writes its changes
// with Put and Delete, and reads others' with Items after Sync.
type Replica struct {
	Client  *Client
	Keys    *Keys
	Clock   *Clock
	Mergers map[string]Merger // by type; a type without one uses Newest

	items   map[key]Item
	pending map[key]bool  // changed here, not yet stored by the server
	seen    map[key]int64 // the change number of the server's item each local one is based on
	cursor  int64         // the last change number read from the server
}

func NewReplica(c *Client, k *Keys, clock *Clock) *Replica {
	return &Replica{Client: c, Keys: k, Clock: clock, Mergers: map[string]Merger{},
		items: map[key]Item{}, pending: map[key]bool{}, seen: map[key]int64{}}
}

// Put records a local edit, sent on the next Sync.
func (r *Replica) Put(typ, guid string, data json.RawMessage) Item {
	return r.set(Item{Type: typ, GUID: guid, Data: data})
}

// Delete records a local delete. The item stays, marked deleted, so other devices learn of it.
func (r *Replica) Delete(typ, guid string) Item {
	return r.set(Item{Type: typ, GUID: guid, Deleted: true})
}

func (r *Replica) set(it Item) Item {
	k := key{it.Type, it.GUID}
	if old, ok := r.items[k]; ok {
		it.Extra = old.Extra
	}
	it.TS = r.Clock.Now()
	r.items[k] = it
	r.pending[k] = true
	return it
}

// Get returns an item, deleted ones included.
func (r *Replica) Get(typ, guid string) (Item, bool) {
	it, ok := r.items[key{typ, guid}]
	return it, ok
}

// Items returns the live items of a type, by guid.
func (r *Replica) Items(typ string) []Item {
	var out []Item
	for k, it := range r.items {
		if k.typ == typ && !it.Deleted {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GUID < out[j].GUID })
	return out
}

// Pending is how many local changes the server hasn't stored yet.
func (r *Replica) Pending() int { return len(r.pending) }

// maxRounds bounds push and pull rounds in one Sync. Each round settles every conflict it
// sees; another round is needed only if a device wrote again in between.
const maxRounds = 5

// Sync sends local changes and takes in everyone else's, merging where both changed.
func (r *Replica) Sync(ctx context.Context) error {
	for round := 0; round < maxRounds; round++ {
		if err := r.push(ctx); err != nil {
			return err
		}
		if err := r.pull(ctx); err != nil {
			return err
		}
		if len(r.pending) == 0 {
			return nil
		}
	}
	return fmt.Errorf("sync: %d items still changing after %d rounds", len(r.pending), maxRounds)
}

func (r *Replica) push(ctx context.Context) error {
	if len(r.pending) == 0 {
		return nil
	}
	keys := make([]key, 0, len(r.pending))
	for k := range r.pending {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].typ != keys[j].typ {
			return keys[i].typ < keys[j].typ
		}
		return keys[i].guid < keys[j].guid
	})
	out := make([]Item, len(keys))
	for i, k := range keys {
		s, err := r.Keys.Seal(r.items[k])
		if err != nil {
			return err
		}
		s.Base = r.seen[k]
		out[i] = s
	}
	res, err := r.Client.Push(ctx, out)
	if err != nil {
		return err
	}
	for i, k := range keys {
		switch {
		case res[i].OK:
			delete(r.pending, k)
			r.seen[k] = res[i].Seq
		case res[i].Current != nil:
			cur, err := r.Keys.Open(*res[i].Current)
			if err != nil {
				return err
			}
			r.take(cur)
		default:
			return fmt.Errorf("push %s %s: %s", k.typ, k.guid, res[i].Error)
		}
	}
	return nil
}

func (r *Replica) pull(ctx context.Context) error {
	for {
		p, err := r.Client.Pull(ctx, r.cursor, "", 0)
		if err != nil {
			return err
		}
		for _, it := range p.Items {
			o, err := r.Keys.Open(it)
			if err != nil {
				return err
			}
			r.take(o)
		}
		r.cursor = p.Next
		if !p.More {
			return nil
		}
	}
}

// take merges an item from the server into this replica.
func (r *Replica) take(remote Item) {
	r.Clock.Observe(remote.TS)
	k := key{remote.Type, remote.GUID}
	if remote.Seq <= r.seen[k] {
		return // this version is known here already
	}
	r.seen[k] = remote.Seq
	local := r.items[k]
	if !r.pending[k] {
		r.items[k] = remote
		return
	}
	merge := r.Mergers[k.typ]
	if merge == nil {
		merge = Newest
	}
	m := merge(local, remote)
	switch {
	case sameValue(m, remote):
		r.items[k] = remote
		delete(r.pending, k)
	case sameValue(m, local) && local.TS.After(remote.TS):
		// local stands as it is and is sent again, now based on remote
	default:
		m.Type, m.GUID = k.typ, k.guid
		m.TS = r.Clock.Now()
		r.items[k] = m
	}
}
