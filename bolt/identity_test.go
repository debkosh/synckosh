package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/debkosh/synckosh"
	"github.com/debkosh/synckosh/client"
)

// byURL is an app's identity for bookmarks: the address, whatever the title.
func byURL(it synckosh.Item) []byte {
	var p struct{ URL string }
	if json.Unmarshal(it.Private, &p) != nil || p.URL == "" {
		return nil
	}
	return []byte(p.URL)
}

func link(title, url string) synckosh.Item {
	b := bookmark("root", title)
	b.Private = json.RawMessage(`{"title":"` + title + `","url":"` + url + `"}`)
	return b
}

// Two devices that each make the same thing end with one item. The second finds it in its pull,
// before it pushes, and takes the server's.
func TestTheSameThingFromTwoDevicesIsOneItem(t *testing.T) {
	e := newEnv(t)
	mac, phone := e.device("mac"), e.device("phone")
	a := put(t, mac, bookmark("root", "Go"))
	b := put(t, phone, bookmark("root", "Go"))
	other := put(t, phone, bookmark("root", "Rust"))
	sync(t, mac, phone, mac)
	for _, d := range []*device{mac, phone} {
		got := items(t, d, "bookmarks")
		if len(got) != 2 || holds(t, d, "bookmarks", b.GUID) || !holds(t, d, "bookmarks", a.GUID) || !holds(t, d, "bookmarks", other.GUID) {
			t.Fatalf("%s holds %d bookmarks, want the Mac's Go and the phone's Rust", d.name, len(got))
		}
	}
	if _, has := stored(t, e.store, "bookmarks", b.GUID); has {
		t.Fatal("the server stored the phone's copy too")
	}
}

// pushFirst is a transport that runs before before a device's first push, as another device
// pushing between its pull and its push.
type pushFirst struct {
	signedIn
	before func()
}

func (p *pushFirst) RoundTrip(r *http.Request) (*http.Response, error) {
	if p.before != nil && strings.HasSuffix(r.URL.Path, "/v1/push") {
		f := p.before
		p.before = nil
		f()
	}
	return p.signedIn.RoundTrip(r)
}

// When the other device's copy reaches the server between this one's pull and its push, the
// server refuses this one's as identity-taken, with the holder, and the device takes that.
// An app's own identity makes two items with other titles one thing.
func TestAnIdentityTakenIsTheServersCopy(t *testing.T) {
	e := newEnv(t)
	mac := e.deviceWith("mac", client.Config{Identity: byURL})
	a := put(t, mac, link("Go", "https://go.dev/"))
	hook := &pushFirst{signedIn: signedIn{"me"}, before: func() { sync(t, mac) }}
	local, err := OpenLocal(e.dir + "/phone.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close() })
	r, err := client.Open(client.Config{Client: &client.Client{URL: e.url, HTTP: &http.Client{Transport: hook}},
		Local: local, Key: e.key, Identity: byURL})
	if err != nil {
		t.Fatal(err)
	}
	phone := &device{Replica: r, name: "phone", local: local}
	b := put(t, phone, link("The Go language", "https://go.dev/"))
	sync(t, phone)
	if holds(t, phone, "bookmarks", b.GUID) || title(t, phone, a.GUID) != "Go" {
		t.Fatalf("the phone kept its own copy, or not the server's")
	}
	if pending, _ := local.Pending(); len(pending) != 0 {
		t.Fatalf("%d entries pending after the identity was settled", len(pending))
	}
}

// An app's identity that returns nil makes every item its own thing, the same content or not.
func TestNoIdentityKeepsEveryItem(t *testing.T) {
	e := newEnv(t)
	none := client.Config{Identity: func(synckosh.Item) []byte { return nil }}
	mac, phone := e.deviceWith("mac", none), e.deviceWith("phone", none)
	put(t, mac, bookmark("root", "Go"))
	put(t, phone, bookmark("root", "Go"))
	sync(t, mac, phone, mac)
	if n := len(items(t, mac, "bookmarks")); n != 2 {
		t.Fatalf("%d bookmarks, want both", n)
	}
}

// The device never trusts the identity the server says an item has: it hashes the holder's own
// content. A holder whose content isn't the same thing is not taken.
func TestAForgedHolderIsNotTaken(t *testing.T) {
	e := newEnv(t)
	phone := e.device("phone")
	b := put(t, phone, bookmark("root", "Mine"))
	// A real item a key holder wrote, which the server says has the phone's item's identity.
	other := bookmark("root", "Someone else's")
	other.GUID, other.Timestamp = synckosh.NewGUID(), b.Timestamp
	sealed, _ := e.key.Seal(other, false)
	sealed.Identity = synckosh.Identity(e.key, synckosh.SameContent(b))
	forge(t, e.store, sealed)

	err := phone.Sync(context.Background())
	var refused *client.RefusedError
	if !errors.As(err, &refused) || refused.Items[0].Code != synckosh.RefusedIdentityTaken {
		t.Fatalf("%v, want the item refused as identity-taken and left pending", err)
	}
	if title(t, phone, b.GUID) != "Mine" || title(t, phone, other.GUID) != "Someone else's" {
		t.Fatal("the phone lost its item or the other")
	}
}
