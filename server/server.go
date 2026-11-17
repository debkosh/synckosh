// Package server is the synckosh server as an http.Handler. It keeps, for each bucket, the
// newest version of each item, and numbers every version it stores so clients can ask for what
// is new. It never opens an item's private part. A bucket is whatever the host makes it: one
// person's data, a shared list, a family's. Which bucket a request may use is the host's
// business: it passes an Auth function. Storage is behind Store; package bolt has one.
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sudiptadeb/synckosh"
)

// Store keeps items. Every Put the server makes is one atomic write, and the server makes one
// at a time. Items are stored as given, Sequence and DeletedAt included.
type Store interface {
	// StoreID returns the bucket's store id, making one on first use.
	StoreID(bucket string) (string, error)
	// SetStoreID replaces the bucket's store id, telling its clients to push all they have.
	SetStoreID(bucket, id string) error
	Get(bucket, collection, guid string) (synckosh.Item, bool, error)
	Put(bucket string, items []synckosh.Item) error
	// Since returns up to limit items with a sequence number greater than after, lowest first:
	// of one collection, or of every collection if collection is empty.
	Since(bucket, collection string, after uint64, limit int) ([]synckosh.Item, error)
	// LastSequence returns the highest sequence number stored for the bucket, 0 if none.
	LastSequence(bucket string) (uint64, error)
	// Cleanup removes deleted items whose DeletedAt is before the given time, in milliseconds.
	Cleanup(deletedBefore int64) (int, error)
}

// Auth tells the server which bucket a request is for. It returns an error to refuse it.
type Auth func(r *http.Request) (bucket string, err error)

// pageSize is the most items one pull answer holds.
const pageSize = 500

// Server serves the protocol. Writes go through one writer, which hands out each bucket's
// sequence numbers in the order its items are stored, with no gaps within one push.
type Server struct {
	// Limits are this server's; zero fields take synckosh.DefaultLimits. Set them before serving.
	Limits synckosh.Limits

	store Store
	auth  Auth
	mux   *http.ServeMux
	now   func() time.Time

	write sync.Mutex // the one writer
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
	s.write.Lock()
	defer s.write.Unlock()
	return s.store.SetStoreID(bucket, NewStoreID())
}

// Cleanup removes items deleted more than age ago.
func (s *Server) Cleanup(age time.Duration) (int, error) {
	s.write.Lock()
	defer s.write.Unlock()
	return s.store.Cleanup(s.now().Add(-age).UnixMilli())
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
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

func (s *Server) storeID(bucket string) (string, error) {
	s.write.Lock()
	defer s.write.Unlock()
	return s.store.StoreID(bucket)
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
	if it.Timestamp > s.now().Add(synckosh.MaxAhead).UnixMilli() {
		return it, refused(synckosh.RefusedTimestampAhead, "timestamp is more than a day ahead of the server's clock")
	}
	it.Sequence, it.DeletedAt = 0, 0 // the server's to set
	return it, nil
}

type pushRequest struct {
	Items []json.RawMessage `json:"items"`
}

func (s *Server) push(w http.ResponseWriter, r *http.Request, bucket string) {
	var req pushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, `body: {"items": [...]}: `+err.Error())
		return
	}
	limits := s.Limits.OrDefaults()
	if len(req.Items) > limits.PushItems {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("more than %d items in one push", limits.PushItems))
		return
	}
	in := make([]synckosh.Item, len(req.Items))
	errs := make([]*synckosh.Result, len(req.Items))
	for i, raw := range req.Items {
		in[i], errs[i] = s.check(raw, limits)
	}

	s.write.Lock()
	defer s.write.Unlock()
	id, err := s.store.StoreID(bucket)
	if err != nil {
		internal(w, r, err)
		return
	}
	seq, err := s.store.LastSequence(bucket)
	if err != nil {
		internal(w, r, err)
		return
	}
	res := make([]synckosh.Result, len(in))
	type key struct{ collection, guid string }
	batch := map[key]int{} // items of this push already chosen to store, by index in out
	var out []synckosh.Item
	now := s.now().UnixMilli()
	for i, it := range in {
		if errs[i] != nil {
			res[i] = *errs[i]
			continue
		}
		k := key{it.Collection, it.GUID}
		cur, has := synckosh.Item{}, false
		if j, ok := batch[k]; ok {
			cur, has = out[j], true
		} else if cur, has, err = s.store.Get(bucket, it.Collection, it.GUID); err != nil {
			internal(w, r, err)
			return
		}
		if has && it.Timestamp < cur.Timestamp {
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
		if j, ok := batch[k]; ok {
			out[j] = it
		} else {
			batch[k] = len(out)
			out = append(out, it)
		}
		res[i] = synckosh.Result{OK: true, Sequence: seq}
	}
	if len(out) > 0 {
		if err := s.store.Put(bucket, out); err != nil {
			internal(w, r, err)
			return
		}
	}
	reply(w, http.StatusOK, map[string]any{"store": id, "time": now, "results": res})
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
	id, err := s.storeID(bucket)
	if err != nil {
		internal(w, r, err)
		return
	}
	items, err := s.store.Since(bucket, collection, after, pageSize+1)
	if err != nil {
		internal(w, r, err)
		return
	}
	more := len(items) > pageSize
	if more {
		items = items[:pageSize]
	}
	next := after
	if len(items) > 0 {
		next = items[len(items)-1].Sequence
	}
	if items == nil {
		items = []synckosh.Item{}
	}
	reply(w, http.StatusOK, map[string]any{"store": id, "items": items, "next": next, "more": more})
}

// NewStoreID makes a store id, for a Store to use when a bucket has none.
func NewStoreID() string { return "s_" + synckosh.NewGUID() }
