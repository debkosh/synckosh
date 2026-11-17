// Package server is the synckosh server as an http.Handler. It keeps, for each account, the
// newest version of each item, and numbers every version it stores so clients can ask for what
// is new. It never opens an item's private part. Who an account is, is the host's business:
// it passes an Auth function. Storage is behind Store; package bolt has one.
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/sudiptadeb/synckosh"
)

// Store keeps items. Every Put the server makes is one atomic write, and the server makes one
// at a time. Items are stored as given, Seen and DeletedAt included.
type Store interface {
	// StoreID returns the account's store id, making one on first use.
	StoreID(account string) (string, error)
	// SetStoreID replaces the account's store id, telling its clients to push all they have.
	SetStoreID(account, id string) error
	Get(account, bucket, guid string) (synckosh.Item, bool, error)
	Put(account string, items []synckosh.Item) error
	// Since returns up to limit items of a bucket with Seen greater than after, lowest first.
	Since(account, bucket string, after uint64, limit int) ([]synckosh.Item, error)
	// LastSeen returns the highest Seen ever stored, across accounts.
	LastSeen() (uint64, error)
	// Cleanup removes deleted items whose DeletedAt is before the given time, in milliseconds.
	Cleanup(deletedBefore int64) (int, error)
}

// Auth tells the server whose request this is. It returns an error to refuse it.
type Auth func(r *http.Request) (account string, err error)

const (
	maxRequest  = 16 << 20 // bytes in one request
	maxItem     = 1 << 20  // bytes in one item
	MaxPush     = 1000     // items in one push, stored in one write
	pageSize    = 500
	maxPageSize = 1000
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var guidRe = regexp.MustCompile(`^[A-Za-z0-9._:~-]{1,128}$`)

// Server serves the protocol. Writes go through one writer, which hands out Seen numbers in
// the order they are stored.
type Server struct {
	store Store
	auth  Auth
	mux   *http.ServeMux
	now   func() time.Time

	write sync.Mutex // the one writer
	last  uint64     // the last Seen handed out
}

// New makes a server on a store.
func New(store Store, auth Auth) (*Server, error) {
	last, err := store.LastSeen()
	if err != nil {
		return nil, err
	}
	s := &Server{store: store, auth: auth, mux: http.NewServeMux(), now: time.Now, last: last}
	s.mux.HandleFunc("GET /v1", s.info)
	s.mux.HandleFunc("POST /v1/push", s.authed(s.push))
	s.mux.HandleFunc("GET /v1/pull", s.authed(s.pull))
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequest)
	s.mux.ServeHTTP(w, r)
}

// ResetStore gives an account a new store id. Its clients then push everything they have.
// It is the one thing to do after the server may have lost any of the account's data.
func (s *Server) ResetStore(account string) error {
	s.write.Lock()
	defer s.write.Unlock()
	return s.store.SetStoreID(account, NewStoreID())
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
		account, err := s.auth(r)
		if err != nil || account == "" {
			fail(w, http.StatusUnauthorized, "not signed in")
			return
		}
		h(w, r, account)
	}
}

func (s *Server) storeID(account string) (string, error) {
	s.write.Lock()
	defer s.write.Unlock()
	return s.store.StoreID(account)
}

// Result is the answer for one pushed item: stored with its Seen number, or not stored because
// the server holds a version with a later timestamp, returned in Current, or refused with an
// Error.
type Result struct {
	OK      bool           `json:"ok"`
	Seen    uint64         `json:"seen,omitempty"`
	Current *synckosh.Item `json:"current,omitempty"`
	Error   string         `json:"error,omitempty"`
}

func (s *Server) check(raw json.RawMessage) (synckosh.Item, string) {
	if len(raw) > maxItem {
		return synckosh.Item{}, "item over 1 MB"
	}
	var it synckosh.Item
	if err := json.Unmarshal(raw, &it); err != nil {
		return synckosh.Item{}, err.Error()
	}
	switch {
	case !nameRe.MatchString(it.Bucket):
		return it, "bucket: lowercase letters, digits, dot, dash or underscore, up to 64"
	case !nameRe.MatchString(it.Type):
		return it, "type: lowercase letters, digits, dot, dash or underscore, up to 64"
	case !guidRe.MatchString(it.GUID):
		return it, "guid: letters, digits and ._:~- up to 128"
	case it.TS <= 0:
		return it, "ts: the time of the edit, in milliseconds"
	case it.TS > s.now().Add(synckosh.MaxAhead).UnixMilli():
		return it, "ts is more than a day ahead of the server's clock"
	}
	it.Seen, it.DeletedAt = 0, 0 // the server's to set
	return it, ""
}

type pushRequest struct {
	Items []json.RawMessage `json:"items"`
}

func (s *Server) push(w http.ResponseWriter, r *http.Request, account string) {
	var req pushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, `body: {"items": [...]}: `+err.Error())
		return
	}
	if len(req.Items) > MaxPush {
		fail(w, http.StatusRequestEntityTooLarge, "more than 1000 items in one push")
		return
	}
	in := make([]synckosh.Item, len(req.Items))
	errs := make([]string, len(req.Items))
	for i, raw := range req.Items {
		in[i], errs[i] = s.check(raw)
	}

	s.write.Lock()
	defer s.write.Unlock()
	id, err := s.store.StoreID(account)
	if err != nil {
		internal(w, r, err)
		return
	}
	res := make([]Result, len(in))
	type key struct{ bucket, guid string }
	batch := map[key]int{} // items of this push already chosen to store, by index in out
	var out []synckosh.Item
	seen := s.last
	now := s.now().UnixMilli()
	for i, it := range in {
		if errs[i] != "" {
			res[i] = Result{Error: errs[i]}
			continue
		}
		k := key{it.Bucket, it.GUID}
		cur, has := synckosh.Item{}, false
		if j, ok := batch[k]; ok {
			cur, has = out[j], true
		} else if cur, has, err = s.store.Get(account, it.Bucket, it.GUID); err != nil {
			internal(w, r, err)
			return
		}
		if has && it.TS < cur.TS {
			c := cur
			res[i] = Result{Current: &c}
			continue
		}
		// Newer, or made in the same millisecond: the later push wins. A retry of an edit
		// already here is stored again, which is the same edit with a new seen number.
		if it.Deleted {
			it.DeletedAt = now
			if has && cur.Deleted && cur.DeletedAt != 0 {
				it.DeletedAt = cur.DeletedAt
			}
		}
		seen++
		it.Seen = seen
		if j, ok := batch[k]; ok {
			out[j] = it
		} else {
			batch[k] = len(out)
			out = append(out, it)
		}
		res[i] = Result{OK: true, Seen: seen}
	}
	if len(out) > 0 {
		if err := s.store.Put(account, out); err != nil {
			internal(w, r, err)
			return
		}
		s.last = seen
	}
	reply(w, http.StatusOK, map[string]any{"store": id, "results": res})
}

func (s *Server) pull(w http.ResponseWriter, r *http.Request, account string) {
	q := r.URL.Query()
	bucket := q.Get("bucket")
	if !nameRe.MatchString(bucket) {
		fail(w, http.StatusBadRequest, "bucket: the bucket to pull")
		return
	}
	var after uint64
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			fail(w, http.StatusBadRequest, "after: a seen number")
			return
		}
		after = n
	}
	limit := pageSize
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageSize {
			fail(w, http.StatusBadRequest, "limit: 1 to 1000")
			return
		}
		limit = n
	}
	id, err := s.storeID(account)
	if err != nil {
		internal(w, r, err)
		return
	}
	items, err := s.store.Since(account, bucket, after, limit+1)
	if err != nil {
		internal(w, r, err)
		return
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	next := after
	if len(items) > 0 {
		next = items[len(items)-1].Seen
	}
	if items == nil {
		items = []synckosh.Item{}
	}
	reply(w, http.StatusOK, map[string]any{"store": id, "items": items, "next": next, "more": more})
}

// NewStoreID makes a store id, for a Store to use when an account has none.
func NewStoreID() string { return "s_" + synckosh.NewGUID() }
