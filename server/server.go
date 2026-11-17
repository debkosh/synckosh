// Package server is the synckosh server as an http.Handler. It keeps, for each bucket, the
// newest version of each item, and numbers every version it stores so clients can ask for what
// is new. It never opens an item's private part. A bucket is whatever the host makes it: one
// person's data, a shared list, a family's. Which bucket a request may use is the host's
// business: it passes an Auth function. Storage is behind Store; package bolt has one.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/sudiptadeb/synckosh"
)

// Store keeps each bucket's items. Items are stored as given, Sequence and DeletedAt included.
type Store interface {
	// StoreID returns the bucket's store id, making one on first use (atomically: two callers
	// get the same id).
	StoreID(bucket string) (string, error)
	// SetStoreID replaces the bucket's store id, telling its clients to push all they have.
	SetStoreID(bucket, id string) error
	// ResetAll gives every bucket a new store id, as after a restore from a backup.
	ResetAll() error
	// Since calls fn with the bucket's items whose sequence is above after, lowest first, of one
	// collection or of every collection if collection is empty, until fn returns false. It sees
	// only whole Updates, in order.
	Since(bucket, collection string, after uint64, fn func(synckosh.Item) bool) error
	// Update runs fn as the bucket's only writer: from fn's first call until Update returns, no
	// other Update of that bucket runs, on this server or any other sharing the store. What fn
	// puts is stored all together if fn returns nil, and not at all otherwise.
	Update(bucket string, fn func(Tx) error) error
	// Cleanup removes deleted items whose DeletedAt is before the given time, in milliseconds.
	// It never lowers a bucket's last sequence number.
	Cleanup(deletedBefore int64) (int, error)
}

// Tx is one bucket's Update.
type Tx interface {
	// Last is the highest sequence number ever given in the bucket. It is a counter, not the
	// highest one stored: it never goes back, even when cleanup removes the item that had it.
	Last() (uint64, error)
	Get(collection, guid string) (synckosh.Item, bool, error)
	// Put stores items, each replacing its guid's version. Last becomes the highest stored.
	Put(items []synckosh.Item) error
	// RemoveBefore removes every item of a collection with a timestamp at or before before,
	// except its synckosh.DeleteBeforeItem.
	RemoveBefore(collection string, before int64) error
}

// Auth tells the server which bucket a request is for. It returns an error to refuse it.
type Auth func(r *http.Request) (bucket string, err error)

// Server serves the protocol. Each push is one Update of its bucket, which hands out the
// bucket's sequence numbers in the order its items are stored, with no gaps within one push.
type Server struct {
	// Limits are this server's; zero fields take synckosh.DefaultLimits. Set them before serving.
	Limits synckosh.Limits
	// CleanupAfter is how long Cleanup keeps deleted items; 0 means they are kept for good. Pull
	// answers carry it, so a device away longer starts over. Set it before serving.
	CleanupAfter time.Duration

	store Store
	auth  Auth
	mux   *http.ServeMux
	now   func() time.Time
}

// New makes a server on a store.
func New(store Store, auth Auth) *Server {
	s := &Server{store: store, auth: auth, mux: http.NewServeMux(), now: time.Now}
	s.mux.HandleFunc("GET /v1", s.info)
	s.mux.HandleFunc("POST /v1/push", s.authed(s.push))
	s.mux.HandleFunc("GET /v1/pull", s.authed(s.pull))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.Limits.OrDefaults().PushBytes))
	s.mux.ServeHTTP(w, r)
}

// ResetStore gives a bucket a new store id. Its clients then push everything they have.
// It is the one thing to do after the server may have lost any of the bucket's data.
func (s *Server) ResetStore(bucket string) error {
	return s.store.SetStoreID(bucket, NewStoreID())
}

// ResetAll gives every bucket a new store id. It is the one thing to do after restoring the
// store from a backup.
func (s *Server) ResetAll() error { return s.store.ResetAll() }

// Cleanup removes items deleted more than CleanupAfter ago, and returns how many. The host runs
// it now and then.
func (s *Server) Cleanup() (int, error) {
	if s.CleanupAfter <= 0 {
		return 0, nil
	}
	return s.store.Cleanup(s.now().Add(-s.CleanupAfter).UnixMilli())
}

// reply writes JSON without Go's escaping of <, > and &, so a part goes out as it came in.
func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	reply(w, code, map[string]string{"error": msg})
}

func internal(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("synckosh: %s %s: %v", r.Method, r.URL.Path, err)
	fail(w, http.StatusInternalServerError, "server error")
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, map[string]any{"version": 1})
}

func (s *Server) authed(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bucket, err := s.auth(r)
		if err != nil || bucket == "" {
			fail(w, http.StatusUnauthorized, "not signed in")
			return
		}
		h(w, r, bucket)
	}
}

func refused(code, msg string) *synckosh.Result { return &synckosh.Result{Code: code, Error: msg} }

func (s *Server) check(raw json.RawMessage, limits synckosh.Limits) (synckosh.Item, *synckosh.Result) {
	if len(raw) > limits.ItemBytes {
		return synckosh.Item{}, refused(synckosh.RefusedTooBig, fmt.Sprintf("item over %d bytes", limits.ItemBytes))
	}
	var it synckosh.Item
	if err := json.Unmarshal(raw, &it); err != nil {
		return synckosh.Item{}, refused(synckosh.RefusedInvalid, err.Error())
	}
	if err := synckosh.Valid(it); err != nil {
		return it, refused(synckosh.RefusedInvalid, err.Error())
	}
	ahead := s.now().Add(synckosh.MaxAhead).UnixMilli()
	if it.Timestamp > ahead {
		return it, refused(synckosh.RefusedTimestampAhead, "timestamp is more than an hour ahead of the server's clock")
	}
	if cutoff, _ := synckosh.Cutoff(it); it.Datatype == synckosh.DeleteBeforeItem && cutoff > ahead {
		return it, refused(synckosh.RefusedTimestampAhead, "the cutoff is more than an hour ahead of the server's clock")
	}
	it.Sequence, it.DeletedAt = 0, 0 // the server's to set
	return it, nil
}

type pushRequest struct {
	Items []json.RawMessage `json:"items"`
}

// tooLarge refuses a whole push, telling the device the server's limits so it can split it.
func tooLarge(w http.ResponseWriter, limits synckosh.Limits, msg string) {
	reply(w, http.StatusRequestEntityTooLarge, map[string]any{"error": msg, "limits": limits})
}

func (s *Server) push(w http.ResponseWriter, r *http.Request, bucket string) {
	limits := s.Limits.OrDefaults()
	var req pushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if tooBig := (*http.MaxBytesError)(nil); errors.As(err, &tooBig) {
			tooLarge(w, limits, fmt.Sprintf("more than %d bytes in one push", limits.PushBytes))
			return
		}
		fail(w, http.StatusBadRequest, `body: {"items": [...]}: `+err.Error())
		return
	}
	if len(req.Items) > limits.PushItems {
		tooLarge(w, limits, fmt.Sprintf("more than %d items in one push", limits.PushItems))
		return
	}
	in := make([]synckosh.Item, len(req.Items))
	errs := make([]*synckosh.Result, len(req.Items))
	for i, raw := range req.Items {
		in[i], errs[i] = s.check(raw, limits)
	}

	id, err := s.store.StoreID(bucket)
	if err != nil {
		internal(w, r, err)
		return
	}
	now := s.now().UnixMilli()
	var res []synckosh.Result
	err = s.store.Update(bucket, func(tx Tx) error {
		var err error
		res, err = store(tx, in, errs, now)
		return err
	})
	if err != nil {
		internal(w, r, err)
		return
	}
	reply(w, http.StatusOK, synckosh.Pushed{Store: id, Time: now, Results: res})
}

// store keeps what a push brings, inside its bucket's Update: each item, unless the bucket holds
// a version with a later timestamp. It numbers what it stores on from the bucket's last number.
//
// A collection's delete-before item is the one item the server reads. Storing one removes every
// item of the collection at or before its cutoff, and from then on such an item is answered OK
// and not stored, so a refill or a device away for long can't bring it back. A delete-before
// with an earlier cutoff than the one held is answered with the one held: the cutoff only moves
// forward.
func store(tx Tx, in []synckosh.Item, errs []*synckosh.Result, now int64) ([]synckosh.Result, error) {
	seq, err := tx.Last()
	if err != nil {
		return nil, err
	}
	res := make([]synckosh.Result, len(in))
	type key struct{ collection, guid string }
	batch := map[key]int{} // items of this push already chosen to store, by index in out
	var out []synckosh.Item
	get := func(collection, guid string) (synckosh.Item, bool, error) {
		if j, ok := batch[key{collection, guid}]; ok {
			return out[j], true, nil
		}
		return tx.Get(collection, guid)
	}
	cutoffs := map[string]int64{} // by collection, 0 for none
	cutoff := func(collection string) (int64, error) {
		if c, ok := cutoffs[collection]; ok {
			return c, nil
		}
		it, has, err := get(collection, synckosh.DeleteBeforeItem)
		if err != nil || !has {
			cutoffs[collection] = 0
			return 0, err
		}
		cutoffs[collection], _ = synckosh.Cutoff(it)
		return cutoffs[collection], nil
	}
	for i, it := range in {
		if errs[i] != nil {
			res[i] = *errs[i]
			continue
		}
		held, err := cutoff(it.Collection)
		if err != nil {
			return nil, err
		}
		isCut := it.Datatype == synckosh.DeleteBeforeItem
		if !isCut && it.Timestamp <= held {
			res[i] = synckosh.Result{OK: true} // deleted already
			continue
		}
		cur, has, err := get(it.Collection, it.GUID)
		if err != nil {
			return nil, err
		}
		newCut, _ := synckosh.Cutoff(it)
		if has && (it.Timestamp < cur.Timestamp || isCut && newCut < held) {
			c := cur
			res[i] = synckosh.Result{Current: &c}
			continue
		}
		// Newer, or made in the same millisecond: the later push wins. A retry of an edit
		// already here is stored again, which is the same edit with a new sequence number.
		if it.Deleted {
			it.DeletedAt = now
			if has && cur.Deleted && cur.DeletedAt != 0 {
				it.DeletedAt = cur.DeletedAt
			}
		}
		seq++
		it.Sequence = seq
		if j, ok := batch[key{it.Collection, it.GUID}]; ok {
			out[j] = it
		} else {
			batch[key{it.Collection, it.GUID}] = len(out)
			out = append(out, it)
		}
		if isCut {
			cutoffs[it.Collection] = newCut
		}
		res[i] = synckosh.Result{OK: true, Sequence: seq}
	}
	if len(out) == 0 {
		return res, nil
	}
	if err := tx.Put(out); err != nil {
		return nil, err
	}
	// Remove what the push's delete-befores cover, its own earlier items included.
	for _, it := range out {
		if it.Datatype == synckosh.DeleteBeforeItem {
			if err := tx.RemoveBefore(it.Collection, cutoffs[it.Collection]); err != nil {
				return nil, err
			}
		}
	}
	return res, nil
}

func (s *Server) pull(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	collection := q.Get("collection")
	if collection != "" && !synckosh.ValidName(collection) {
		fail(w, http.StatusBadRequest, "collection: the collection to pull, or none for every collection")
		return
	}
	var after uint64
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			fail(w, http.StatusBadRequest, "after: a sequence number")
			return
		}
		after = n
	}
	most := s.Limits.OrDefaults().PageBytes
	if v := q.Get("bytes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fail(w, http.StatusBadRequest, "bytes: the most the device reads in one page")
			return
		}
		most = min(most, n)
	}
	id, err := s.store.StoreID(bucket)
	if err != nil {
		internal(w, r, err)
		return
	}
	p := synckosh.Page{Store: id, Items: []synckosh.Item{}, Next: after, Time: s.now().UnixMilli(),
		Cleanup: s.CleanupAfter.Milliseconds()}
	size := 0
	var bad error
	err = s.store.Since(bucket, collection, after, func(it synckosh.Item) bool {
		b, err := synckosh.Marshal(it)
		if err != nil {
			bad = err
			return false
		}
		if len(p.Items) > 0 && size+len(b)+1 > most {
			p.More = true
			return false
		}
		size += len(b) + 1
		p.Items = append(p.Items, it)
		p.Next = it.Sequence
		return true
	})
	if err == nil {
		err = bad
	}
	if err != nil {
		internal(w, r, err)
		return
	}
	reply(w, http.StatusOK, p)
}

// NewStoreID makes a store id, for a Store to use when a bucket has none.
func NewStoreID() string { return "s_" + synckosh.NewGUID() }
