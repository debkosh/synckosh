package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sudiptadeb/synckosh"
)

// Limits. Each is generous for a browser's data and keeps one account from filling the server.
const (
	maxRequest  = 16 << 20 // bytes in one request
	maxItem     = 1 << 20  // bytes in one item
	maxPush     = 1000     // items in one push
	pageSize    = 500      // items in one page of changes, by default
	maxPageSize = 1000
	maxWait     = 60 * time.Second
)

// Features lists what this server can do beyond version 1's core, for clients to check. A
// later server adds to it and never takes away.
var features = []string{"wait"}

var (
	typeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	guidRe = regexp.MustCompile(`^[A-Za-z0-9._:~-]{1,128}$`)
)

type server struct {
	st  *store
	mux *http.ServeMux
	now func() time.Time

	mu      sync.Mutex
	changed map[string]chan struct{} // closed when an account's items change
}

func newServer(st *store) *server {
	s := &server{st: st, mux: http.NewServeMux(), now: time.Now, changed: map[string]chan struct{}{}}
	s.mux.HandleFunc("GET /v1", s.info)
	s.mux.HandleFunc("POST /v1/accounts", s.signup)
	s.mux.HandleFunc("GET /v1/devices", s.authed(s.listDevices))
	s.mux.HandleFunc("POST /v1/devices", s.authed(s.addDevice))
	s.mux.HandleFunc("DELETE /v1/devices/{id}", s.authed(s.removeDevice))
	s.mux.HandleFunc("POST /v1/items", s.authed(s.push))
	s.mux.HandleFunc("GET /v1/items", s.authed(s.pull))
	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequest)
	s.mux.ServeHTTP(w, r)
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
	log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	fail(w, http.StatusInternalServerError, "server error")
}

func (s *server) info(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, map[string]any{"version": 1, "features": features})
}

func readName(r *http.Request) (string, bool) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return "", false
	}
	name := strings.TrimSpace(body.Name)
	return name, name != "" && len(name) <= 100
}

func (s *server) signup(w http.ResponseWriter, r *http.Request) {
	name, ok := readName(r)
	if !ok {
		fail(w, http.StatusBadRequest, "name: the device's name, 1 to 100 characters")
		return
	}
	account, dev, token, err := s.st.signup(r.Context(), name)
	if err != nil {
		internal(w, r, err)
		return
	}
	log.Printf("signup: account %s, device %s %q", account, dev, name)
	reply(w, http.StatusCreated, map[string]string{"account": account, "device": dev, "token": token})
}

type caller struct{ account, device string }

func (s *server) authed(h func(http.ResponseWriter, *http.Request, caller)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			fail(w, http.StatusUnauthorized, "no token")
			return
		}
		account, dev, err := s.st.auth(r.Context(), token)
		if errors.Is(err, sql.ErrNoRows) {
			fail(w, http.StatusUnauthorized, "unknown token")
			return
		}
		if err != nil {
			internal(w, r, err)
			return
		}
		h(w, r, caller{account, dev})
	}
}

func (s *server) listDevices(w http.ResponseWriter, r *http.Request, c caller) {
	ds, err := s.st.devices(r.Context(), c.account)
	if err != nil {
		internal(w, r, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"devices": ds})
}

func (s *server) addDevice(w http.ResponseWriter, r *http.Request, c caller) {
	name, ok := readName(r)
	if !ok {
		fail(w, http.StatusBadRequest, "name: the device's name, 1 to 100 characters")
		return
	}
	dev, token, err := s.st.addDevice(r.Context(), c.account, name)
	if err != nil {
		internal(w, r, err)
		return
	}
	log.Printf("device added: account %s, device %s %q, by %s", c.account, dev, name, c.device)
	reply(w, http.StatusCreated, map[string]string{"account": c.account, "device": dev, "token": token})
}

func (s *server) removeDevice(w http.ResponseWriter, r *http.Request, c caller) {
	id := r.PathValue("id")
	err := s.st.removeDevice(r.Context(), c.account, id)
	if errors.Is(err, errNoDevice) {
		fail(w, http.StatusNotFound, "no such device")
		return
	}
	if err != nil {
		internal(w, r, err)
		return
	}
	log.Printf("device removed: account %s, device %s, by %s", c.account, id, c.device)
	w.WriteHeader(http.StatusNoContent)
}

// check reads one pushed item's envelope. The body is stored as the device sent it, fields
// this server doesn't know included, without its change number.
func (s *server) check(raw json.RawMessage) incoming {
	if len(raw) > maxItem {
		return incoming{err: "item over 1 MB"}
	}
	var it synckosh.Item
	if err := json.Unmarshal(raw, &it); err != nil {
		return incoming{err: err.Error()}
	}
	switch {
	case !typeRe.MatchString(it.Type):
		return incoming{err: "type: lowercase letters, digits, dot, dash or underscore, up to 64"}
	case !guidRe.MatchString(it.GUID):
		return incoming{err: "guid: letters, digits and ._:~- up to 128"}
	case it.TS.MS <= 0 || it.TS.N < 0 || it.TS.Dev == "":
		return incoming{err: "ts: [ms, n, device]"}
	case it.TS.MS > s.now().Add(synckosh.MaxAhead).UnixMilli():
		return incoming{err: "ts is more than a day ahead of the server's clock"}
	}
	base := it.Base
	it.Seq, it.Base = 0, 0
	body, err := json.Marshal(it)
	if err != nil {
		return incoming{err: err.Error()}
	}
	it.Base = base
	return incoming{it: it, body: body}
}

func (s *server) push(w http.ResponseWriter, r *http.Request, c caller) {
	var req struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "body: {\"items\": [...]}: "+err.Error())
		return
	}
	if len(req.Items) > maxPush {
		fail(w, http.StatusRequestEntityTooLarge, "more than 1000 items in one push")
		return
	}
	in := make([]incoming, len(req.Items))
	for i, raw := range req.Items {
		in[i] = s.check(raw)
	}
	res, changed, err := s.st.put(r.Context(), c.account, in)
	if err != nil {
		internal(w, r, err)
		return
	}
	if changed {
		s.notify(c.account)
	}
	reply(w, http.StatusOK, map[string]any{"results": res})
}

func (s *server) pull(w http.ResponseWriter, r *http.Request, c caller) {
	q := r.URL.Query()
	since, err := strconv.ParseInt(q.Get("since"), 10, 64)
	if q.Get("since") == "" {
		since, err = 0, nil
	}
	if err != nil || since < 0 {
		fail(w, http.StatusBadRequest, "since: a change number")
		return
	}
	limit := pageSize
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > maxPageSize {
			fail(w, http.StatusBadRequest, "limit: 1 to 1000")
			return
		}
	}
	var wait time.Duration
	if v := q.Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			fail(w, http.StatusBadRequest, "wait: seconds")
			return
		}
		wait = min(time.Duration(n)*time.Second, maxWait)
	}
	typ := q.Get("type")
	ch := s.watch(c.account)
	items, next, more, err := s.st.changes(r.Context(), c.account, since, typ, limit)
	if err == nil && len(items) == 0 && wait > 0 {
		select {
		case <-ch:
			items, next, more, err = s.st.changes(r.Context(), c.account, since, typ, limit)
		case <-time.After(wait):
		case <-r.Context().Done():
			return
		}
	}
	if err != nil {
		internal(w, r, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"items": items, "next": next, "more": more})
}

// watch returns a channel closed at the account's next change.
func (s *server) watch(account string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.changed[account]
	if !ok {
		ch = make(chan struct{})
		s.changed[account] = ch
	}
	return ch
}

func (s *server) notify(account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.changed[account]; ok {
		close(ch)
		delete(s.changed, account)
	}
}
