package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/debkosh/synckosh"
	"github.com/debkosh/synckosh/client"
	"github.com/debkosh/synckosh/server"
)

// The tests sign in with a header naming the bucket: signing in is the host's business.
func auth(r *http.Request) (string, error) {
	if a := r.Header.Get("X-Bucket"); a != "" {
		return a, nil
	}
	return "", errors.New("no bucket")
}

type signedIn struct{ bucket string }

func (s signedIn) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Bucket", s.bucket)
	return http.DefaultTransport.RoundTrip(r)
}

type env struct {
	t     *testing.T
	url   string
	srv   *server.Server
	store *Store
	dir   string
	key   *synckosh.DataKey
	opts  []func(*server.Server)
}

func withLimits(l synckosh.Limits) func(*server.Server) {
	return func(s *server.Server) { s.Limits = l }
}

func cleanupAfter(d time.Duration) func(*server.Server) {
	return func(s *server.Server) { s.CleanupAfter = d }
}

// newEnv starts a server, set up by opts before it serves, and makes the bucket's key.
func newEnv(t *testing.T, opts ...func(*server.Server)) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := OpenStore(filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(st, auth)
	for _, o := range opts {
		o(srv)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { ts.Close(); st.Close() })
	key, err := synckosh.NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, url: ts.URL, srv: srv, store: st, dir: dir, key: key, opts: opts}
}

type device struct {
	*client.Replica
	name  string
	local *Local
	cfg   client.Config
}

func (e *env) device(name string) *device {
	e.t.Helper()
	return e.deviceWith(name, client.Config{})
}

// deviceWith opens a device with cfg, its client, store and key filled in.
func (e *env) deviceWith(name string, cfg client.Config) *device {
	e.t.Helper()
	local, err := OpenLocal(filepath.Join(e.dir, name+".db"))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { local.Close() })
	cfg.Client = &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}}
	cfg.Local, cfg.Key = local, e.key
	r, err := client.Open(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	return &device{r, name, local, cfg}
}

// stored returns the server's version of an item in bucket "me".
func stored(t *testing.T, st *Store, collection, guid string) (synckosh.Item, bool) {
	t.Helper()
	var it synckosh.Item
	var has bool
	err := st.Update("me", func(tx server.Tx) error {
		var err error
		it, has, err = tx.Get(collection, guid)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return it, has
}

// last returns bucket "me"'s last sequence number.
func last(t *testing.T, st *Store) uint64 {
	t.Helper()
	var n uint64
	err := st.Update("me", func(tx server.Tx) error {
		var err error
		n, err = tx.Last()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func sync(t *testing.T, ds ...*device) {
	t.Helper()
	for _, d := range ds {
		if err := d.Sync(context.Background()); err != nil {
			t.Fatalf("%s: %v", d.name, err)
		}
	}
}

// put puts one item, giving a new one a guid.
func put(t *testing.T, d *device, it synckosh.Item) synckosh.Item {
	t.Helper()
	if it.GUID == "" {
		it.GUID = synckosh.NewGUID()
	}
	its, err := d.Put(it)
	if err != nil {
		t.Fatal(err)
	}
	return its[0]
}

func private(t *testing.T, it synckosh.Item) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(it.Private, &m); err != nil {
		t.Fatalf("%s: %v", it.GUID, err)
	}
	return m
}

func items(t *testing.T, d *device, collection string) []synckosh.Item {
	t.Helper()
	its, err := d.Items(collection)
	if err != nil {
		t.Fatal(err)
	}
	return its
}

func bookmark(parent, title string) synckosh.Item {
	return synckosh.Item{Collection: "bookmarks", Datatype: "bookmark",
		Public:  json.RawMessage(fmt.Sprintf(`{"parent":%q,"order":"V"}`, parent)),
		Private: json.RawMessage(fmt.Sprintf(`{"title":%q,"url":"https://example.com/"}`, title))}
}

func TestTwoDevices(t *testing.T) {
	e := newEnv(t)
	mac, phone := e.device("mac"), e.device("phone")
	b := put(t, mac, bookmark("root", "Go"))
	sync(t, mac, phone)
	got := items(t, phone, "bookmarks")
	if len(got) != 1 || private(t, got[0])["title"] != "Go" || got[0].GUID != b.GUID {
		t.Fatalf("phone: %+v", got)
	}

	if err := phone.Delete("bookmarks", b.GUID); err != nil {
		t.Fatal(err)
	}
	sync(t, phone, mac)
	if got := items(t, mac, "bookmarks"); len(got) != 0 {
		t.Fatalf("deleted on the phone, still on the Mac: %+v", got)
	}
	it, has := stored(t, e.store, "bookmarks", b.GUID)
	if !has || !it.Deleted || it.DeletedAt == 0 {
		t.Fatalf("the server keeps a delete, marked: %+v", it)
	}
}

func TestNewestWins(t *testing.T) {
	for _, first := range []string{"older", "newer"} {
		t.Run(first+" pushes first", func(t *testing.T) {
			e := newEnv(t)
			mac, phone := e.device("mac"), e.device("phone")
			s := put(t, mac, synckosh.Item{Collection: "settings", Datatype: "setting", GUID: "setting:theme",
				Public: json.RawMessage(`{"key":"theme"}`), Private: json.RawMessage(`{"value":"dark"}`)})
			sync(t, mac, phone)
			s.Private = json.RawMessage(`{"value":"light"}`)
			put(t, mac, s)
			time.Sleep(2 * time.Millisecond)
			s.Private = json.RawMessage(`{"value":"system"}`)
			put(t, phone, s)
			if first == "older" {
				sync(t, mac, phone, mac)
			} else {
				sync(t, phone, mac, phone)
			}
			for _, d := range []*device{mac, phone} {
				it, _, _ := d.Get("settings", "setting:theme")
				if v := private(t, it)["value"]; v != "system" {
					t.Errorf("%s: %v, want the phone's later edit", d.name, v)
				}
			}
		})
	}
}

// Two devices that haven't seen each other's edit can stamp them in the same millisecond. The
// later push wins on the server, and every device takes the server's copy.
func TestSameMillisecondLaterPushWins(t *testing.T) {
	e := newEnv(t)
	mac, phone := e.device("mac"), e.device("phone")
	s := put(t, mac, synckosh.Item{Collection: "settings", Datatype: "setting", GUID: "setting:theme",
		Public: json.RawMessage(`{"key":"theme"}`), Private: json.RawMessage(`{"value":"dark"}`)})
	sync(t, mac, phone)

	// Another device's edit, in the same millisecond as the Mac's, reaches the server last.
	other := s
	other.Sequence = 0
	other.Private = json.RawMessage(`{"value":"light"}`)
	sealed, err := e.key.Seal(other, false)
	if err != nil {
		t.Fatal(err)
	}
	c := &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}}
	res, err := c.Push(context.Background(), []synckosh.Item{sealed})
	if err != nil || !res.Results[0].OK {
		t.Fatalf("a push in the same millisecond: %v %+v", err, res)
	}

	sync(t, mac, phone)
	for _, d := range []*device{mac, phone} {
		it, _, _ := d.Get("settings", s.GUID)
		if it.Timestamp != s.Timestamp || private(t, it)["value"] != "light" {
			t.Fatalf("%s kept %s, want the later push", d.name, it.Private)
		}
	}
}

func TestLoginAsItems(t *testing.T) {
	e := newEnv(t)
	mac, phone := e.device("mac"), e.device("phone")
	login := put(t, mac, synckosh.Item{Collection: "logins", Datatype: "login", Private: json.RawMessage(`{"origin":"https://x.com"}`)})
	field := func(key, value string) synckosh.Item {
		return synckosh.Item{Collection: "logins", Datatype: "login-field",
			Public: json.RawMessage(fmt.Sprintf(`{"parent":%q,"key":%q}`, login.GUID, key)), Private: json.RawMessage(fmt.Sprintf(`{"value":%q}`, value))}
	}
	user := put(t, mac, field("username", "me"))
	put(t, mac, field("password", "hunter2"))
	sync(t, mac, phone)

	user.Private = json.RawMessage(`{"value":"me@work.com"}`)
	put(t, phone, user)                        // the phone fixes the username
	put(t, mac, field("password", "n3w-pass")) // the Mac sets a new password
	put(t, phone, field("password", "other"))  // and so does the phone, offline
	sync(t, mac, phone, mac)

	for _, d := range []*device{mac, phone} {
		vals := map[string][]string{}
		for _, it := range items(t, d, "logins") {
			if it.Datatype == "login-field" {
				var p struct{ Key string }
				json.Unmarshal(it.Public, &p)
				vals[p.Key] = append(vals[p.Key], private(t, it)["value"].(string))
			}
		}
		if len(vals["username"]) != 1 || vals["username"][0] != "me@work.com" || len(vals["password"]) != 3 {
			t.Errorf("%s: %v, want the new username and all three passwords", d.name, vals)
		}
	}
}

func TestServerSeesOnlyThePublicPart(t *testing.T) {
	e := newEnv(t)
	mac := e.device("mac")
	put(t, mac, synckosh.Item{Collection: "logins", Datatype: "login-field",
		Public: json.RawMessage(`{"parent":"l1","key":"password"}`), Private: json.RawMessage(`{"value":"hunter2"}`)})
	sync(t, mac)
	e.store.Close()
	raw, err := os.ReadFile(filepath.Join(e.dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("hunter2")) {
		t.Fatal("the server's file holds the password in the clear")
	}
	if !bytes.Contains(raw, []byte(`"key":"password"`)) {
		t.Fatal("the public part should be readable by the server")
	}
}

// After the server may have lost data, it changes the store id. Devices push all they have
// with their original timestamps; a new device gets the partial data, then the backfill.
func TestNewStoreIDRefills(t *testing.T) {
	e := newEnv(t)
	// The Mac keeps deleted items, so its refill brings back the phone's delete too.
	mac, phone := e.deviceWith("mac", client.Config{KeepDeleted: true}), e.device("phone")
	a := put(t, mac, bookmark("root", "A"))
	b := put(t, phone, bookmark("root", "B"))
	sync(t, mac, phone, mac)
	if err := phone.Delete("bookmarks", b.GUID); err != nil {
		t.Fatal(err)
	}
	sync(t, phone, mac)

	// The server loses everything: a fresh store.
	st := e.newServer()
	e.reopen(mac)
	e.reopen(phone)
	tablet := e.device("tablet")

	sync(t, mac) // the Mac refills first
	sync(t, tablet)
	if got := items(t, tablet, "bookmarks"); len(got) != 1 || got[0].GUID != a.GUID {
		t.Fatalf("tablet after the Mac refilled: %+v", got)
	}
	it, _ := stored(t, st, "bookmarks", a.GUID)
	orig, _, _ := mac.Get("bookmarks", a.GUID)
	if it.Timestamp != orig.Timestamp {
		t.Fatalf("refilled with a new timestamp: %v, was %v", it.Timestamp, orig.Timestamp)
	}
	gone, has := stored(t, st, "bookmarks", b.GUID)
	if !has || !gone.Deleted {
		t.Fatal("the refill dropped a delete")
	}
	put(t, phone, bookmark("root", "C")) // the phone, offline through all this
	sync(t, phone, tablet)
	if got := items(t, tablet, "bookmarks"); len(got) != 2 {
		t.Fatalf("tablet after the phone refilled: %+v", got)
	}
}

func TestBigPushPagesWithoutLoss(t *testing.T) {
	e := newEnv(t)
	mac, phone := e.device("mac"), e.device("phone")
	var visits []synckosh.Item
	for i := 0; i < 2500; i++ {
		visits = append(visits, synckosh.Item{Collection: "history", Datatype: "visit", GUID: synckosh.NewGUID(),
			Private: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))})
	}
	if _, err := mac.Put(visits...); err != nil {
		t.Fatal(err)
	}
	sync(t, mac, phone)
	if got := items(t, phone, "history"); len(got) != 2500 {
		t.Fatalf("phone has %d visits, want 2500", len(got))
	}
	if err := mac.DeleteBefore("history", time.Now()); err != nil {
		t.Fatal(err)
	}
	put(t, mac, synckosh.Item{Collection: "history", Datatype: "visit", Private: json.RawMessage(`{"n":"after"}`)})
	sync(t, mac, phone)
	if got := items(t, phone, "history"); len(got) != 1 {
		t.Fatalf("after deleting before now, the phone shows %d visits, want the one made after", len(got))
	}
}

func TestReopenKeepsEverything(t *testing.T) {
	e := newEnv(t)
	mac := e.device("mac")
	b := put(t, mac, bookmark("root", "Go"))
	mac.Replica, _ = client.Open(client.Config{Client: &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}},
		Local: mac.local, Key: e.key})
	sync(t, mac) // the edit made before reopening is still pending, and goes out
	it, has := stored(t, e.store, "bookmarks", b.GUID)
	if !has || it.Timestamp != b.Timestamp {
		t.Fatalf("after reopening: %v %+v", has, it)
	}
	b2 := put(t, mac, bookmark("root", "Go docs"))
	if b2.Timestamp <= b.Timestamp {
		t.Fatalf("the clock went back after reopening: %v then %v", b.Timestamp, b2.Timestamp)
	}
}

func TestServerChecks(t *testing.T) {
	e := newEnv(t)
	c := &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}}
	ctx := context.Background()
	now := time.Now().UnixMilli()
	// Another bucket's pushes don't use up this bucket's sequence numbers.
	other := &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"someone-else"}}}
	for i := 0; i < 3; i++ {
		other.Push(ctx, []synckosh.Item{{Collection: "settings", Datatype: "setting", GUID: fmt.Sprint("o", i), Timestamp: now}})
	}
	it := synckosh.Item{Collection: "settings", Datatype: "setting", GUID: "setting:theme", Timestamp: now}
	for i := 0; i < 2; i++ {
		res, err := c.Push(ctx, []synckosh.Item{it})
		if err != nil || !res.Results[0].OK {
			t.Fatalf("push %d: %v %+v", i, err, res)
		}
	}
	p, _ := c.Pull(ctx, synckosh.Filter{Collections: []string{"settings"}}, 0, 0)
	if len(p.Items) != 1 || p.Items[0].Sequence != 2 {
		t.Fatalf("a retried push: want one item, stored again as sequence 2: %+v", p)
	}
	older := it
	older.Timestamp--
	res, _ := c.Push(ctx, []synckosh.Item{older})
	if res.Results[0].OK || res.Results[0].Current == nil {
		t.Fatalf("an older version: %+v", res.Results[0])
	}
	future := it
	future.Timestamp = now + 2*24*3600*1000
	res, _ = c.Push(ctx, []synckosh.Item{future})
	if r := res.Results[0]; r.OK || r.Code != synckosh.RefusedTimestampAhead || r.Error == "" || res.Time == 0 {
		t.Fatalf("a timestamp two days ahead: %+v %d", r, res.Time)
	}
	anon := &client.Client{URL: e.url}
	if _, err := anon.Pull(ctx, synckosh.Filter{Collections: []string{"settings"}}, 0, 0); err == nil {
		t.Fatal("pulled without signing in")
	}
	if p, _ := other.Pull(ctx, synckosh.Filter{}, 0, 0); len(p.Items) != 3 {
		t.Fatalf("another bucket sees my items: %d", len(p.Items))
	}
}

func TestCleanup(t *testing.T) {
	// The age is long next to a sync's writes, which each wait for the disk.
	e := newEnv(t, cleanupAfter(time.Second))
	mac := e.device("mac")
	b := put(t, mac, bookmark("root", "Go"))
	keep := put(t, mac, bookmark("root", "Keep"))
	sync(t, mac)
	mac.Delete("bookmarks", b.GUID)
	sync(t, mac)
	if n, err := e.srv.Cleanup(); err != nil || n != 0 {
		t.Fatalf("cleaned a fresh delete: %d %v", n, err)
	}
	time.Sleep(1100 * time.Millisecond)
	if n, err := e.srv.Cleanup(); err != nil || n != 1 {
		t.Fatalf("cleanup: %d %v", n, err)
	}
	if _, has := stored(t, e.store, "bookmarks", b.GUID); has {
		t.Fatal("cleaned item still there")
	}
	if _, has := stored(t, e.store, "bookmarks", keep.GUID); !has {
		t.Fatal("cleanup removed a live item")
	}
}

func TestCreateThenJoinWithPassphrase(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}}
	key, err := client.Create(ctx, c, "correct horse battery staple", "Main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Create(ctx, c, "another", "Second first run"); !errors.Is(err, client.ErrHasKey) {
		t.Fatalf("a second Create: %v", err)
	}
	e.key = key
	mac := e.device("mac")
	put(t, mac, bookmark("root", "Go"))
	sync(t, mac)
	if got, _ := mac.Items(synckosh.LibraryCollection); len(got) != 1 {
		t.Fatalf("the Mac holds %d sealed copies, want the one Create made", len(got))
	}

	// A new device has only the passphrase.
	if _, err := client.Join(ctx, c, "wrong"); !errors.Is(err, synckosh.ErrPassphrase) {
		t.Fatalf("join with a wrong passphrase: %v", err)
	}
	joined, err := client.Join(ctx, c, "correct horse battery staple")
	if err != nil || joined.ID != key.ID {
		t.Fatal(err)
	}
	e.key = joined
	phone := e.device("phone")
	sync(t, phone)
	got := items(t, phone, "bookmarks")
	if len(got) != 1 || private(t, got[0])["title"] != "Go" {
		t.Fatalf("%+v", got)
	}
}

// Create refuses a bucket with any item in it, not only one with a sealed copy: its passphrases
// may all be gone while its devices still hold the key.
func TestCreateRefusesABucketWithItems(t *testing.T) {
	e := newEnv(t)
	mac := e.device("mac")
	put(t, mac, bookmark("root", "Go"))
	sync(t, mac)
	c := &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}}
	if _, err := client.Create(context.Background(), c, "pass", "Main"); !errors.Is(err, client.ErrHasKey) {
		t.Fatalf("%v", err)
	}
}

// hook runs before each push a device makes, as if another device pushed just then.
type hook struct {
	signedIn
	before func()
}

func (h hook) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && h.before != nil {
		h.before()
	}
	return h.signedIn.RoundTrip(r)
}

func cursor(t *testing.T, d *device) uint64 {
	t.Helper()
	b, err := d.local.Meta("after")
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	json.Unmarshal(b, &n)
	return n
}

// A device's own push doesn't come back to it: when the push's sequence numbers follow straight
// on from its cursor, it moves its cursor past them. When another push got in between, it pulls
// from its cursor, and so gets that push.
func TestOwnPushDoesNotComeBack(t *testing.T) {
	e := newEnv(t)
	mac := e.device("mac")
	var visits []synckosh.Item
	for i := 0; i < 1500; i++ {
		visits = append(visits, synckosh.Item{Collection: "history", Datatype: "visit", GUID: synckosh.NewGUID(),
			Private: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))})
	}
	mac.Put(visits...)
	put(t, mac, bookmark("root", "Go"))
	sync(t, mac)
	if got := last(t, e.store); cursor(t, mac) != got || got != 1501 {
		t.Fatalf("cursor %d after pushing, the server's last is %d", cursor(t, mac), got)
	}

	// Another device pushes just before the Mac does.
	raw := &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}}
	var theirs synckosh.Item
	local, _ := OpenLocal(filepath.Join(e.dir, "mac2.db"))
	defer local.Close()
	busy, _ := client.Open(client.Config{Local: local, Key: e.key, Client: &client.Client{URL: e.url,
		HTTP: &http.Client{Transport: hook{signedIn{"me"}, func() {
			if theirs.GUID != "" {
				return
			}
			theirs = bookmark("root", "Theirs")
			theirs.GUID, theirs.Timestamp = synckosh.NewGUID(), time.Now().UnixMilli()
			sealed, _ := e.key.Seal(theirs, false)
			raw.Push(context.Background(), []synckosh.Item{sealed})
		}}}}})
	d := &device{busy, "busy", local, client.Config{}}
	sync(t, d) // pulls everything, with nothing to push
	put(t, d, bookmark("root", "Mine"))
	sync(t, d) // its push lands just after theirs, so the cursor stays
	if cursor(t, d) != 1501 {
		t.Fatalf("cursor moved to %d past a push it never pulled", cursor(t, d))
	}
	sync(t, d) // and the next pull brings theirs, and its own back
	if _, has, _ := d.Get("bookmarks", theirs.GUID); !has {
		t.Fatal("the push that came in between never arrived")
	}
	if got := last(t, e.store); cursor(t, d) != got {
		t.Fatalf("cursor %d, the server's last is %d", cursor(t, d), got)
	}
}

// A device whose clock was two days ahead made an edit the server refuses. Sync says so clearly,
// syncs the rest, and once the clock is right the edit goes out with a new timestamp.
func TestClockAheadIsRefusedThenHeals(t *testing.T) {
	e := newEnv(t)
	mac := e.device("mac")
	ahead := synckosh.Item{Collection: "bookmarks", Datatype: "bookmark", GUID: synckosh.NewGUID(),
		Timestamp: time.Now().Add(48 * time.Hour).UnixMilli(), Private: json.RawMessage(`{"title":"Ahead"}`)}
	if err := mac.local.Put(client.Entry{Item: ahead, Pending: true}); err != nil {
		t.Fatal(err)
	}
	fine := put(t, mac, bookmark("root", "Fine"))

	err := mac.Sync(context.Background())
	var refused *client.RefusedError
	if !errors.As(err, &refused) || len(refused.Items) != 1 || refused.Items[0].GUID != ahead.GUID ||
		refused.Items[0].Code != synckosh.RefusedTimestampAhead {
		t.Fatalf("sync: %v", err)
	}
	if refused.Ahead < 47*time.Hour {
		t.Fatalf("ahead by %s, want about two days", refused.Ahead)
	}
	if !strings.Contains(err.Error(), "clock is") {
		t.Fatalf("not a clear error: %v", err)
	}
	if _, has := stored(t, e.store, "bookmarks", fine.GUID); !has {
		t.Fatal("the refused item held back the rest")
	}

	sync(t, mac)
	it, has := stored(t, e.store, "bookmarks", ahead.GUID)
	if !has || it.Timestamp > time.Now().UnixMilli() {
		t.Fatalf("after the clock is right: %v %+v", has, it)
	}
}

// The server stores only the fields an item has: one it doesn't know is dropped, not kept.
func TestTheServerDropsFieldsItDoesNotKnow(t *testing.T) {
	e := newEnv(t)
	h := &http.Client{Transport: signedIn{"me"}}
	body := fmt.Sprintf(`{"items":[{"collection":"notes","datatype":"note","guid":"n1","timestamp":%d,"deleted":false,"later":{"x":1}}]}`,
		time.Now().UnixMilli())
	res, err := h.Post(e.url+"/v1/push", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	res, err = h.Get(e.url + "/v1/pull")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(res.Body)
	if !strings.Contains(b.String(), `"guid":"n1"`) || strings.Contains(b.String(), "later") {
		t.Fatalf("pulled %s, want the item without the field", b.String())
	}
}
