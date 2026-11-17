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
	"testing"
	"time"

	"github.com/sudiptadeb/synckosh"
	"github.com/sudiptadeb/synckosh/client"
	"github.com/sudiptadeb/synckosh/server"
)

// The tests sign in with a header naming the account: signing in is the host's business.
func auth(r *http.Request) (string, error) {
	if a := r.Header.Get("X-Account"); a != "" {
		return a, nil
	}
	return "", errors.New("no account")
}

type signedIn struct{ account string }

func (s signedIn) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Account", s.account)
	return http.DefaultTransport.RoundTrip(r)
}

type env struct {
	t     *testing.T
	url   string
	srv   *server.Server
	store *Store
	dir   string
	key   *synckosh.DataKey
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := OpenStore(filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(st, auth)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { ts.Close(); st.Close() })
	key, err := synckosh.NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, url: ts.URL, srv: srv, store: st, dir: dir, key: key}
}

type device struct {
	*client.Replica
	name  string
	local *Local
}

func (e *env) device(name string, buckets ...string) *device {
	e.t.Helper()
	local, err := OpenLocal(filepath.Join(e.dir, name+".db"))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { local.Close() })
	r, err := client.Open(client.Config{
		Client: &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}},
		Local:  local, Key: e.key, Buckets: buckets,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return &device{r, name, local}
}

func sync(t *testing.T, ds ...*device) {
	t.Helper()
	for _, d := range ds {
		if err := d.Sync(context.Background()); err != nil {
			t.Fatalf("%s: %v", d.name, err)
		}
	}
}

func put(t *testing.T, d *device, it synckosh.Item) synckosh.Item {
	t.Helper()
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

func items(t *testing.T, d *device, bucket string) []synckosh.Item {
	t.Helper()
	its, err := d.Items(bucket)
	if err != nil {
		t.Fatal(err)
	}
	return its
}

func bookmark(parent, title string) synckosh.Item {
	return synckosh.Item{Bucket: "bookmarks", Type: "bookmark",
		Public:  json.RawMessage(fmt.Sprintf(`{"parent":%q,"order":"V"}`, parent)),
		Private: json.RawMessage(fmt.Sprintf(`{"title":%q,"url":"https://example.com/"}`, title))}
}

func TestTwoDevices(t *testing.T) {
	e := newEnv(t)
	mac, phone := e.device("mac", "bookmarks"), e.device("phone", "bookmarks")
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
	it, _, _ := mac.Get("bookmarks", b.GUID)
	if !it.Deleted || it.DeletedAt == 0 || private(t, it)["title"] != "Go" {
		t.Fatalf("a delete is marked by the server and keeps its data: %+v", it)
	}

	// Putting it again restores it.
	put(t, mac, it)
	sync(t, mac, phone)
	if got := items(t, phone, "bookmarks"); len(got) != 1 {
		t.Fatalf("restored on the Mac, not on the phone: %+v", got)
	}
}

func TestNewestWins(t *testing.T) {
	for _, first := range []string{"older", "newer"} {
		t.Run(first+" pushes first", func(t *testing.T) {
			e := newEnv(t)
			mac, phone := e.device("mac", "settings"), e.device("phone", "settings")
			s := put(t, mac, synckosh.Item{Bucket: "settings", Type: "setting", GUID: "setting:theme",
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

// A login is items: the login, its fields, and each value of its password as its own item,
// never an edit, so two devices changing it at once both keep theirs.
// Two devices that haven't seen each other's edit can stamp them in the same millisecond. The
// later push wins on the server, and every device takes the server's copy.
func TestSameMillisecondLaterPushWins(t *testing.T) {
	e := newEnv(t)
	mac, phone := e.device("mac", "settings"), e.device("phone", "settings")
	s := put(t, mac, synckosh.Item{Bucket: "settings", Type: "setting", GUID: "setting:theme",
		Public: json.RawMessage(`{"key":"theme"}`), Private: json.RawMessage(`{"value":"dark"}`)})
	sync(t, mac, phone)

	// Another device's edit, in the same millisecond as the Mac's, reaches the server last.
	other := s
	other.Seen = 0
	other.Private = json.RawMessage(`{"value":"light"}`)
	sealed, err := e.key.Seal(other)
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
	mac, phone := e.device("mac", "logins"), e.device("phone", "logins")
	login := put(t, mac, synckosh.Item{Bucket: "logins", Type: "login", Private: json.RawMessage(`{"origin":"https://x.com"}`)})
	field := func(key, value string) synckosh.Item {
		return synckosh.Item{Bucket: "logins", Type: "login-field",
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
			if it.Type == "login-field" {
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
	mac := e.device("mac", "logins")
	put(t, mac, synckosh.Item{Bucket: "logins", Type: "login-field",
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
	mac, phone := e.device("mac", "bookmarks"), e.device("phone", "bookmarks")
	a := put(t, mac, bookmark("root", "A"))
	b := put(t, phone, bookmark("root", "B"))
	sync(t, mac, phone, mac)
	if err := phone.Delete("bookmarks", b.GUID); err != nil {
		t.Fatal(err)
	}
	sync(t, phone, mac)

	// The server loses everything: a fresh store.
	e.store.Close()
	st, err := OpenStore(filepath.Join(e.dir, "server2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, _ := server.New(st, auth)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	for _, d := range []*device{mac, phone} {
		d.Replica, _ = client.Open(client.Config{Client: &client.Client{URL: ts.URL, HTTP: &http.Client{Transport: signedIn{"me"}}},
			Local: d.local, Key: e.key, Buckets: []string{"bookmarks"}})
	}
	e.url = ts.URL
	tablet := e.device("tablet", "bookmarks")

	sync(t, mac) // the Mac refills first
	sync(t, tablet)
	if got := items(t, tablet, "bookmarks"); len(got) != 1 || got[0].GUID != a.GUID {
		t.Fatalf("tablet after the Mac refilled: %+v", got)
	}
	it, _, _ := st.Get("me", "bookmarks", a.GUID)
	orig, _, _ := mac.Get("bookmarks", a.GUID)
	if it.Timestamp != orig.Timestamp {
		t.Fatalf("refilled with a new timestamp: %v, was %v", it.Timestamp, orig.Timestamp)
	}
	gone, has, _ := st.Get("me", "bookmarks", b.GUID)
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
	mac, phone := e.device("mac", "history"), e.device("phone", "history")
	var visits []synckosh.Item
	for i := 0; i < 2500; i++ {
		visits = append(visits, synckosh.Item{Bucket: "history", Type: "visit", Private: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))})
	}
	if _, err := mac.Put(visits...); err != nil {
		t.Fatal(err)
	}
	sync(t, mac, phone)
	if got := items(t, phone, "history"); len(got) != 2500 {
		t.Fatalf("phone has %d visits, want 2500", len(got))
	}
	if err := mac.Clear("history"); err != nil {
		t.Fatal(err)
	}
	put(t, mac, synckosh.Item{Bucket: "history", Type: "visit", Private: json.RawMessage(`{"n":"after"}`)})
	sync(t, mac, phone)
	if got := items(t, phone, "history"); len(got) != 1 {
		t.Fatalf("after clearing, the phone shows %d visits, want the one made after", len(got))
	}
}

func TestReopenKeepsEverything(t *testing.T) {
	e := newEnv(t)
	mac := e.device("mac", "bookmarks")
	b := put(t, mac, bookmark("root", "Go"))
	mac.Replica, _ = client.Open(client.Config{Client: &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}},
		Local: mac.local, Key: e.key, Buckets: []string{"bookmarks"}})
	sync(t, mac) // the edit made before reopening is still pending, and goes out
	it, has, _ := e.store.Get("me", "bookmarks", b.GUID)
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
	it := synckosh.Item{Bucket: "settings", Type: "setting", GUID: "setting:theme", Timestamp: now,
		Extra: map[string]json.RawMessage{"later": json.RawMessage(`{"x":1}`)}}
	for i := 0; i < 2; i++ {
		res, err := c.Push(ctx, []synckosh.Item{it})
		if err != nil || !res.Results[0].OK {
			t.Fatalf("push %d: %v %+v", i, err, res)
		}
	}
	p, _ := c.Pull(ctx, "settings", 0)
	if len(p.Items) != 1 || p.Items[0].Seen != 2 {
		t.Fatalf("a retried push: want one item, stored again as seen 2: %+v", p)
	}
	if string(p.Items[0].Extra["later"]) != `{"x":1}` {
		t.Fatalf("the server dropped a field it doesn't know: %+v", p.Items[0])
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
	if res.Results[0].OK || res.Results[0].Error == "" {
		t.Fatalf("a timestamp two days ahead: %+v", res.Results[0])
	}
	anon := &client.Client{URL: e.url}
	if _, err := anon.Pull(ctx, "settings", 0); err == nil {
		t.Fatal("pulled without signing in")
	}
	other := &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"someone-else"}}}
	if p, _ := other.Pull(ctx, "settings", 0); len(p.Items) != 0 {
		t.Fatal("another account sees my items")
	}
}

func TestCleanup(t *testing.T) {
	e := newEnv(t)
	mac := e.device("mac", "bookmarks")
	b := put(t, mac, bookmark("root", "Go"))
	keep := put(t, mac, bookmark("root", "Keep"))
	sync(t, mac)
	mac.Delete("bookmarks", b.GUID)
	sync(t, mac)
	if n, err := e.srv.Cleanup(time.Hour); err != nil || n != 0 {
		t.Fatalf("cleaned a fresh delete: %d %v", n, err)
	}
	if n, err := e.srv.Cleanup(-time.Minute); err != nil || n != 1 {
		t.Fatalf("cleanup: %d %v", n, err)
	}
	if _, has, _ := e.store.Get("me", "bookmarks", b.GUID); has {
		t.Fatal("cleaned item still there")
	}
	if _, has, _ := e.store.Get("me", "bookmarks", keep.GUID); !has {
		t.Fatal("cleanup removed a live item")
	}
}

func TestJoinWithPassphrase(t *testing.T) {
	e := newEnv(t)
	mac := e.device("mac", "keys", "bookmarks")
	sealed, err := e.key.SealedCopy("correct horse battery staple", "Main")
	if err != nil {
		t.Fatal(err)
	}
	put(t, mac, sealed)
	put(t, mac, bookmark("root", "Go"))
	sync(t, mac)

	// A new device has only the passphrase: it reads the keys bucket, then everything.
	c := &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}}
	p, err := c.Pull(context.Background(), synckosh.KeysBucket, 0)
	if err != nil || len(p.Items) != 1 {
		t.Fatalf("%v %+v", err, p)
	}
	key, err := synckosh.OpenSealedCopy(p.Items[0], "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	local, _ := OpenLocal(filepath.Join(e.dir, "new.db"))
	defer local.Close()
	r, _ := client.Open(client.Config{Client: c, Local: local, Key: key, Buckets: []string{"keys", "bookmarks"}})
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Items("bookmarks")
	if len(got) != 1 || private(t, got[0])["title"] != "Go" {
		t.Fatalf("%+v", got)
	}
}
