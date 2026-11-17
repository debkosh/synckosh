package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sudiptadeb/synckosh"
)

func startServer(t *testing.T) string {
	t.Helper()
	st, err := openStore(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(newServer(st))
	t.Cleanup(func() { ts.Close(); st.Close() })
	return ts.URL
}

// peer is one browser: its replica, synced through the server.
type peer struct {
	*synckosh.Replica
	name string
}

// twoDevices signs the Mac up, and joins the phone the way a new device joins: a token from
// the Mac, then the passphrase to open the keys item it finds on the server.
func twoDevices(t *testing.T, url string) (mac, phone peer) {
	t.Helper()
	ctx := context.Background()
	mc, acct, err := synckosh.Signup(ctx, url, "MacBook Pro")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := synckosh.NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	mclock := synckosh.NewClock(acct.Device)
	mac = peer{synckosh.NewReplica(mc, keys, mclock), "mac"}
	locked, err := keys.Lock("correct horse battery staple", mclock.Now())
	if err != nil {
		t.Fatal(err)
	}
	mac.Put(locked.Type, locked.GUID, locked.Data)
	syncAll(t, mac)

	pa, err := mc.AddDevice(ctx, "Pixel 9")
	if err != nil {
		t.Fatal(err)
	}
	pc := &synckosh.Client{URL: url, Token: pa.Token}
	page, err := pc.Pull(ctx, 0, synckosh.KeysType, 0)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("keys item: %v %v", err, page.Items)
	}
	pkeys, err := synckosh.Unlock(page.Items[0], "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	phone = peer{synckosh.NewReplica(pc, pkeys, synckosh.NewClock(pa.Device)), "phone"}
	syncAll(t, phone)
	return mac, phone
}

func syncAll(t *testing.T, ds ...peer) {
	t.Helper()
	for _, d := range ds {
		if err := d.Sync(context.Background()); err != nil {
			t.Fatalf("%s: %v", d.name, err)
		}
	}
}

func data(t *testing.T, it synckosh.Item) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(it.Data, &m); err != nil {
		t.Fatalf("%s %s: %v", it.Type, it.GUID, err)
	}
	return m
}

func TestTwoDevicesSync(t *testing.T) {
	mac, phone := twoDevices(t, startServer(t))
	mac.Put("bookmark", "bm_1", json.RawMessage(`{"title":"Go","url":"https://go.dev/"}`))
	mac.Put("setting", "setting:theme", json.RawMessage(`{"value":"dark"}`))
	syncAll(t, mac, phone)
	got := phone.Items("bookmark")
	if len(got) != 1 || data(t, got[0])["title"] != "Go" {
		t.Fatalf("phone bookmarks: %v", got)
	}

	phone.Delete("bookmark", "bm_1")
	syncAll(t, phone, mac)
	if got := mac.Items("bookmark"); len(got) != 0 {
		t.Fatalf("deleted on the phone, still on the Mac: %v", got)
	}
	if it, ok := mac.Get("bookmark", "bm_1"); !ok || !it.Deleted {
		t.Fatal("the Mac doesn't know of the delete")
	}
}

func TestNewestWins(t *testing.T) {
	mac, phone := twoDevices(t, startServer(t))
	mac.Put("setting", "setting:theme", json.RawMessage(`{"value":"dark"}`))
	syncAll(t, mac, phone)

	// Both change it before syncing; the phone's edit is the later one.
	mac.Put("setting", "setting:theme", json.RawMessage(`{"value":"light"}`))
	time.Sleep(2 * time.Millisecond)
	phone.Put("setting", "setting:theme", json.RawMessage(`{"value":"system"}`))
	syncAll(t, mac, phone, mac)
	for _, d := range []peer{mac, phone} {
		it, _ := d.Get("setting", "setting:theme")
		if v := data(t, it)["value"]; v != "system" {
			t.Errorf("%s: theme %v, want the phone's later edit", d.name, v)
		}
		if d.Pending() != 0 {
			t.Errorf("%s: %d pending", d.name, d.Pending())
		}
	}
}

// fields is a custom merger for a test type whose data holds a timestamp per field:
// {"title":{"v":..,"ts":[..]}, ...}. Each field's newest value wins.
func fields(local, remote synckosh.Item) synckosh.Item {
	if local.Deleted || remote.Deleted {
		return synckosh.Newest(local, remote)
	}
	type field struct {
		V  json.RawMessage `json:"v"`
		TS synckosh.TS     `json:"ts"`
	}
	var a, b map[string]field
	if json.Unmarshal(local.Data, &a) != nil || json.Unmarshal(remote.Data, &b) != nil {
		return synckosh.Newest(local, remote)
	}
	for k, f := range b {
		if cur, ok := a[k]; !ok || f.TS.After(cur.TS) {
			a[k] = f
		}
	}
	out := local
	out.Data, _ = json.Marshal(a)
	return out
}

func TestCustomMerger(t *testing.T) {
	mac, phone := twoDevices(t, startServer(t))
	for _, d := range []peer{mac, phone} {
		d.Mergers["login"] = fields
	}
	mac.Put("login", "lg_1", json.RawMessage(`{"username":{"v":"me","ts":[1,0,"a"]},"password":{"v":"old","ts":[1,0,"a"]}}`))
	syncAll(t, mac, phone)

	// The Mac fixes the username while the phone changes the password: both survive.
	mac.Put("login", "lg_1", json.RawMessage(`{"username":{"v":"me@example.com","ts":[5,0,"mac"]},"password":{"v":"old","ts":[1,0,"a"]}}`))
	phone.Put("login", "lg_1", json.RawMessage(`{"username":{"v":"me","ts":[1,0,"a"]},"password":{"v":"new","ts":[6,0,"phone"]}}`))
	syncAll(t, mac, phone, mac)
	for _, d := range []peer{mac, phone} {
		it, _ := d.Get("login", "lg_1")
		var m map[string]struct{ V string }
		json.Unmarshal(it.Data, &m)
		if m["username"].V != "me@example.com" || m["password"].V != "new" {
			t.Errorf("%s: %s, want both edits", d.name, it.Data)
		}
	}
}

func TestServerNeverSeesData(t *testing.T) {
	url := startServer(t)
	mac, _ := twoDevices(t, url)
	mac.Put("login", "lg_1", json.RawMessage(`{"password":"hunter2"}`))
	syncAll(t, mac)
	page, err := mac.Client.Pull(context.Background(), 0, "login", 0)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("%v %v", err, page.Items)
	}
	if bytes.Contains(page.Items[0].Data, []byte("hunter2")) {
		t.Fatalf("the server holds the password in the clear: %s", page.Items[0].Data)
	}
}

func TestOlderDeviceKeepsNewerFields(t *testing.T) {
	url := startServer(t)
	mac, phone := twoDevices(t, url)
	// A later version of the app writes an envelope field this one doesn't know.
	k, _ := synckosh.Unlock(mustKeysItem(t, mac), "correct horse battery staple")
	it := synckosh.Item{Type: "bookmark", GUID: "bm_1", TS: synckosh.TS{MS: time.Now().UnixMilli(), Dev: "future"},
		Data: json.RawMessage(`{"title":"Go"}`), Extra: map[string]json.RawMessage{"key": json.RawMessage(`2`)}}
	sealed, err := k.Seal(it)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := mac.Client.Push(context.Background(), []synckosh.Item{sealed}); err != nil || !res[0].OK {
		t.Fatalf("%v %+v", err, res)
	}
	syncAll(t, phone)
	phone.Put("bookmark", "bm_1", json.RawMessage(`{"title":"Go docs"}`))
	syncAll(t, phone)
	page, _ := phone.Client.Pull(context.Background(), 0, "bookmark", 0)
	if len(page.Items) != 1 || string(page.Items[0].Extra["key"]) != "2" {
		t.Fatalf("the field a newer version wrote was dropped: %+v", page.Items)
	}
}

func mustKeysItem(t *testing.T, d peer) synckosh.Item {
	t.Helper()
	it, ok := d.Get(synckosh.KeysType, synckosh.KeysGUID)
	if !ok {
		t.Fatal("no keys item")
	}
	return it
}

func TestRetryAndFutureAndRemovedDevice(t *testing.T) {
	url := startServer(t)
	ctx := context.Background()
	c, acct, err := synckosh.Signup(ctx, url, "MacBook Pro")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	it := synckosh.Item{Type: "setting", GUID: "setting:theme", TS: synckosh.TS{MS: now, Dev: acct.Device}}
	for i := 0; i < 2; i++ {
		res, err := c.Push(ctx, []synckosh.Item{it})
		if err != nil || !res[0].OK {
			t.Fatalf("push %d: %v %+v", i, err, res)
		}
	}
	page, _ := c.Pull(ctx, 0, "", 0)
	if len(page.Items) != 1 || page.Next != 1 {
		t.Fatalf("a retried push stored twice: %+v", page)
	}

	future := it
	future.TS = synckosh.TS{MS: now + 2*24*3600*1000, Dev: acct.Device}
	res, err := c.Push(ctx, []synckosh.Item{future})
	if err != nil || res[0].OK || !strings.Contains(res[0].Error, "ahead") {
		t.Fatalf("a timestamp two days ahead: %v %+v", err, res)
	}

	p, err := c.AddDevice(ctx, "Pixel 9")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDevice(ctx, p.Device); err != nil {
		t.Fatal(err)
	}
	pc := &synckosh.Client{URL: url, Token: p.Token}
	if _, err := pc.Pull(ctx, 0, "", 0); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a removed device still reads: %v", err)
	}
}

func TestWaitReturnsOnChange(t *testing.T) {
	url := startServer(t)
	mac, phone := twoDevices(t, url)
	ctx := context.Background()
	page, _ := phone.Client.Pull(ctx, 0, "", 0)
	got := make(chan synckosh.Page, 1)
	go func() {
		p, err := phone.Client.Pull(ctx, page.Next, "", 30*time.Second)
		if err != nil {
			t.Error(err)
		}
		got <- p
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	mac.Put("tabs", "tabs:mac", json.RawMessage(`{"windows":[]}`))
	syncAll(t, mac)
	select {
	case p := <-got:
		if len(p.Items) != 1 {
			t.Fatalf("woke with %v", p.Items)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("took %v", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting pull never woke")
	}
}

func TestInfo(t *testing.T) {
	resp, err := http.Get(startServer(t) + "/v1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"version":1`) {
		t.Fatalf("%s", b)
	}
}
