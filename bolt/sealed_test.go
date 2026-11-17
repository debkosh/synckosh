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

// forge stores items in bucket "me" as given, each with a new sequence number, as a server that
// doesn't hold the key could.
func forge(t *testing.T, st *Store, its ...synckosh.Item) {
	t.Helper()
	err := st.Update("me", func(tx server.Tx) error {
		n, err := tx.Last()
		if err != nil {
			return err
		}
		for i := range its {
			n++
			its[i].Sequence = n
		}
		return tx.Put(its)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// rejected syncs d and returns the guids it rejected.
func rejected(t *testing.T, d *device) []string {
	t.Helper()
	err := d.Sync(context.Background())
	var rj *client.RejectedError
	if err != nil && !errors.As(err, &rj) {
		t.Fatalf("%s: %v", d.name, err)
	}
	var out []string
	if rj != nil {
		for _, it := range rj.Items {
			out = append(out, it.GUID)
		}
	}
	return out
}

func title(t *testing.T, d *device, guid string) string {
	t.Helper()
	it, has, err := d.Get("bookmarks", guid)
	if err != nil || !has {
		t.Fatalf("%s: %s: %v %v", d.name, guid, has, err)
	}
	if it.Deleted {
		return "(deleted)"
	}
	s, _ := private(t, it)["title"].(string)
	return s
}

// A server that doesn't hold the key can't strip, alter, move or delete an item: the
// device rejects what it sends, says so, and syncs on past it.
func TestTamperedItemsAreRejected(t *testing.T) {
	e := newEnv(t)
	mac, phone := e.device("mac"), e.device("phone")
	b := put(t, mac, bookmark("root", "Go"))
	sync(t, mac, phone)
	real, _ := stored(t, e.store, "bookmarks", b.GUID)
	later := real.Timestamp + 1000

	for name, change := range map[string]func(*synckosh.Item){
		"strip the private part": func(i *synckosh.Item) { i.Private, i.Timestamp = nil, later },
		"mark it deleted":        func(i *synckosh.Item) { i.Deleted, i.Timestamp = true, later },
		"change its base":        func(i *synckosh.Item) { i.Base = 7 },
		"move it to a new guid":  func(i *synckosh.Item) { i.GUID = synckosh.NewGUID() },
		"move it elsewhere":      func(i *synckosh.Item) { i.Collection = "notes" },
		"delete everything before now": func(i *synckosh.Item) {
			*i = synckosh.Item{Collection: "bookmarks", Datatype: synckosh.DeleteBeforeItem, GUID: synckosh.DeleteBeforeItem,
				Timestamp: later, Public: json.RawMessage(fmt.Sprintf(`{"before":%d}`, real.Timestamp))}
		},
		"break a name": func(i *synckosh.Item) { i.Collection = "Bookmarks" },
	} {
		forged := real
		change(&forged)
		forge(t, e.store, forged)
		if got := rejected(t, phone); len(got) != 1 || got[0] != forged.GUID {
			t.Errorf("%s: rejected %v", name, got)
		}
		if got := items(t, phone, "bookmarks"); len(got) != 1 || title(t, phone, b.GUID) != "Go" {
			t.Fatalf("%s: the phone now has %+v", name, got)
		}
	}
	// The cursor moved past them: what comes next still arrives.
	n := put(t, mac, bookmark("root", "Next"))
	rejected(t, mac) // the Mac pulls the forgeries too
	if got := rejected(t, phone); len(got) != 0 || title(t, phone, n.GUID) != "Next" {
		t.Fatalf("after the forgeries: %v", got)
	}
}

// With the public part sealed too, the device uses the copy in the box; without, the public
// part outside the seal is taken as the server sends it.
func TestThePublicPartOutsideTheSeal(t *testing.T) {
	e := newEnv(t)
	mac, phone := e.device("mac"), e.device("phone")
	sealedPublic := e.deviceWith("mac2", client.Config{SealPublic: map[string]bool{"bookmark": true}})
	plain := put(t, mac, bookmark("root", "Plain"))
	sealed := put(t, sealedPublic, bookmark("root", "Sealed"))
	sync(t, mac, sealedPublic, phone)
	for _, guid := range []string{plain.GUID, sealed.GUID} {
		it, _ := stored(t, e.store, "bookmarks", guid)
		it.Public = json.RawMessage(`{"parent":"elsewhere"}`)
		forge(t, e.store, it)
	}
	sync(t, phone)
	get := func(guid string) string {
		it, _, _ := phone.Get("bookmarks", guid)
		return string(it.Public)
	}
	if p := get(sealed.GUID); p != `{"parent":"root","order":"V"}` {
		t.Errorf("sealed with its public part, the phone shows %s", p)
	}
	if p := get(plain.GUID); p != `{"parent":"elsewhere"}` {
		t.Errorf("sealed without its public part, the phone shows %s", p)
	}
}

// An item under a key the device doesn't hold is locked. It changes nothing the device holds,
// even as a delete or a delete-before, and sync goes on past it.
func TestUnknownKeyIsLocked(t *testing.T) {
	e := newEnv(t)
	mac, phone := e.device("mac"), e.device("phone")
	var guids []string
	for _, name := range []string{"A", "B", "C"} {
		guids = append(guids, put(t, mac, bookmark("root", name)).GUID)
	}
	sync(t, mac, phone)

	server, _ := synckosh.NewDataKey() // a key the server made up
	later := time.Now().Add(time.Hour).UnixMilli()
	var forged []synckosh.Item
	for i, guid := range guids {
		it, _ := stored(t, e.store, "bookmarks", guid)
		it.Private, it.Timestamp, it.Deleted = json.RawMessage(`{"title":"Forged"}`), later, i == 0
		s, _ := server.Seal(it, false)
		forged = append(forged, s)
	}
	cut, _ := server.Seal(synckosh.Item{Collection: "bookmarks", Datatype: synckosh.DeleteBeforeItem, GUID: synckosh.DeleteBeforeItem,
		Timestamp: later, Public: json.RawMessage(`{"before":1}`)}, false)
	forge(t, e.store, append(forged, cut)...)
	good := put(t, mac, bookmark("root", "Good"))
	sync(t, mac, phone)

	for i, want := range []string{"A", "B", "C"} {
		if got := title(t, phone, guids[i]); got != want {
			t.Errorf("%s became %q", want, got)
		}
	}
	if got := items(t, phone, "bookmarks"); len(got) != 4 || title(t, phone, good.GUID) != "Good" {
		t.Fatalf("the phone shows %d bookmarks, want the three and the good one", len(got))
	}
	locked, _ := phone.Locked("bookmarks")
	if len(locked) != 1 || locked[0].GUID != cut.GUID {
		t.Fatalf("locked: %+v, want only the delete-before, which the phone held no version of", locked)
	}
}

// A forged locked version with a later timestamp can't block the real one: any version the
// device can open replaces a locked one.
func TestTheRealVersionReplacesALockedOne(t *testing.T) {
	e := newEnv(t)
	mac := e.device("mac")
	b := put(t, mac, bookmark("root", "Real"))
	sync(t, mac)
	real, _ := stored(t, e.store, "bookmarks", b.GUID)

	other, _ := synckosh.NewDataKey()
	fake := real
	fake.Timestamp += int64(time.Hour / time.Millisecond)
	fake, _ = other.Seal(fake, false)
	forge(t, e.store, fake)
	phone := e.device("phone")
	sync(t, phone)
	if got := items(t, phone, "bookmarks"); len(got) != 0 {
		t.Fatalf("a locked item shown: %+v", got)
	}
	if _, _, err := phone.Get("bookmarks", b.GUID); !errors.Is(err, client.ErrLocked) {
		t.Fatalf("Get: %v", err)
	}
	if _, err := phone.Put(real); !errors.Is(err, client.ErrLocked) {
		t.Fatalf("Put: %v", err)
	}
	if err := phone.Delete("bookmarks", b.GUID); !errors.Is(err, client.ErrLocked) {
		t.Fatalf("Delete: %v", err)
	}

	forge(t, e.store, real)
	sync(t, phone)
	if title(t, phone, b.GUID) != "Real" {
		t.Fatal("the real version didn't replace the locked one")
	}
	if locked, _ := phone.Locked("bookmarks"); len(locked) != 0 {
		t.Fatalf("still locked: %+v", locked)
	}
}

// A refill never pushes a locked entry: it is unverified, and the devices that can open it hold
// the real copy.
func TestARefillNeverPushesALockedEntry(t *testing.T) {
	e := newEnv(t)
	phone := e.device("phone")
	other, _ := synckosh.NewDataKey()
	fake, _ := other.Seal(synckosh.Item{Collection: "bookmarks", Datatype: "bookmark", GUID: synckosh.NewGUID(),
		Timestamp: time.Now().UnixMilli()}, false)
	forge(t, e.store, fake)
	mine := put(t, phone, bookmark("root", "Mine"))
	sync(t, phone)

	st := e.newServer()
	e.reopen(phone)
	sync(t, phone)
	if _, has := stored(t, st, "bookmarks", mine.GUID); !has {
		t.Fatal("the refill didn't push the phone's own item")
	}
	if _, has := stored(t, st, "bookmarks", fake.GUID); has {
		t.Fatal("the refill pushed a locked entry")
	}
}

func TestPublicPartsRoundTrip(t *testing.T) {
	e := newEnv(t)
	mac := e.deviceWith("mac", client.Config{SealPublic: map[string]bool{"sealed": true}})
	phone := e.device("phone")
	pub := json.RawMessage("{\"t\":\"a<b&c> d\"}")
	var guids []string
	for _, datatype := range []string{"sealed", "plain"} {
		guids = append(guids, put(t, mac, synckosh.Item{Collection: "notes", Datatype: datatype, Public: pub}).GUID)
	}
	sync(t, mac, phone)
	for _, guid := range guids {
		it, _, err := phone.Get("notes", guid)
		if err != nil || !bytes.Equal(it.Public, pub) {
			t.Errorf("%s: %v %s, want %s", it.Datatype, err, it.Public, pub)
		}
	}
}

// Without a key everything is plain, and the server is trusted.
func TestPlainModeWithNoKey(t *testing.T) {
	e := newEnv(t)
	e.key = nil
	mac, phone := e.device("mac"), e.device("phone")
	b := put(t, mac, bookmark("root", "Plain"))
	sync(t, mac, phone)
	if title(t, phone, b.GUID) != "Plain" {
		t.Fatal("not synced")
	}
	e.store.Close()
	raw, _ := os.ReadFile(filepath.Join(e.dir, "server.db"))
	if !bytes.Contains(raw, []byte(`"title":"Plain"`)) {
		t.Fatal("the private part isn't plain on the server")
	}
}

// newServer replaces the server with an empty one, as a server that lost everything, and
// returns its store. Devices reach it once reopened.
func (e *env) newServer() *Store {
	e.t.Helper()
	e.store.Close()
	st, err := OpenStore(filepath.Join(e.dir, "server-"+synckosh.NewGUID()+".db"))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { st.Close() })
	e.srv = server.New(st, auth)
	ts := httptest.NewServer(e.srv)
	e.t.Cleanup(ts.Close)
	e.url, e.store = ts.URL, st
	return st
}

// reopen opens a device's replica again, on the env's server.
func (e *env) reopen(d *device) {
	e.t.Helper()
	cfg := d.cfg
	cfg.Client, cfg.Local, cfg.Key = &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}}, d.local, e.key
	r, err := client.Open(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	d.Replica = r
}

// A removed passphrase stays removed: the server can't bring its sealed copy back, as the
// copy's seal binds its deleted flag and timestamp.
func TestJoinRefusesARemovedCopyBroughtBack(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}}
	key, err := client.Create(ctx, c, "old passphrase", "Old")
	if err != nil {
		t.Fatal(err)
	}
	e.key = key
	mac := e.device("mac")
	sync(t, mac)
	copies, _ := mac.Items(synckosh.KeysCollection)
	if err := mac.Delete(synckosh.KeysCollection, copies[0].GUID); err != nil {
		t.Fatal(err)
	}
	sync(t, mac)
	if _, err := client.Join(ctx, c, "old passphrase"); err == nil {
		t.Fatal("joined with a removed passphrase")
	}
	back, _ := stored(t, e.store, synckosh.KeysCollection, copies[0].GUID)
	back.Deleted, back.DeletedAt, back.Timestamp = false, 0, back.Timestamp+1
	forge(t, e.store, back)
	if _, err := client.Join(ctx, c, "old passphrase"); err == nil {
		t.Fatal("joined with a removed passphrase the server brought back")
	}
}
