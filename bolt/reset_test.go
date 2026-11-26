package bolt

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/debkosh/synckosh"
	"github.com/debkosh/synckosh/client"
)

// serverItems returns every item the server holds in bucket "me".
func serverItems(t *testing.T, e *env) []synckosh.Item {
	t.Helper()
	c := &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}}
	p, err := c.Pull(context.Background(), synckosh.Filter{}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return p.Items
}

// A bucket reset leaves no copy of the key on the server: a device that synced with it before
// pushes nothing back, and every Sync says so until it is set up again.
func TestAResetBucketStopsTheDevice(t *testing.T) {
	e := newEnv(t)
	e.seedKey()
	mac := e.device("mac")
	put(t, mac, bookmark("root", "Go"))
	sync(t, mac)

	e.wipe()
	e.reopen(mac)
	for i := 0; i < 2; i++ {
		if err := mac.Sync(context.Background()); !errors.Is(err, client.ErrBucketReset) {
			t.Fatalf("sync %d after the reset: %v, want ErrBucketReset", i, err)
		}
	}
	if its := serverItems(t, e); len(its) != 0 {
		t.Fatalf("the device pushed %d items into the reset bucket", len(its))
	}
}

// A bucket set up again under another key holds no copy of this device's: it stops, rather
// than push items no device of the new key can open.
func TestABucketSetUpAgainStopsTheOldDevice(t *testing.T) {
	e := newEnv(t)
	e.seedKey()
	phone := e.device("phone")
	put(t, phone, bookmark("root", "Old"))
	sync(t, phone)

	e.wipe()
	c := &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}}
	if _, err := client.Create(context.Background(), c, "a new passphrase", "Main"); err != nil {
		t.Fatal(err)
	}
	e.reopen(phone)
	if err := phone.Sync(context.Background()); !errors.Is(err, client.ErrBucketReset) {
		t.Fatalf("%v, want ErrBucketReset", err)
	}
	if its := serverItems(t, e); len(its) != 1 || its[0].Collection != synckosh.LibraryCollection {
		t.Fatalf("the server holds %d items, want only the new key's copy", len(its))
	}
}

// A server that lost items but kept a copy of the key, as one restored from a backup, is
// healed: the device pushes back what it holds.
func TestABucketThatKeptTheKeyHeals(t *testing.T) {
	e := newEnv(t)
	e.seedKey()
	mac := e.device("mac")
	b := put(t, mac, bookmark("root", "Go"))
	sync(t, mac)

	e.newServer()
	e.reopen(mac)
	sync(t, mac)
	if _, has := stored(t, e.store, "bookmarks", b.GUID); !has {
		t.Fatal("the device didn't push its bookmark back")
	}
}
