package bolt

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sudiptadeb/synckosh/client"
)

const cleanupAge = 400 * time.Millisecond

// A device away longer than the server keeps deletes may hold items whose deletes were cleaned
// up meanwhile. It starts over, and the item stays gone everywhere, even through a refill.
func TestAStaleDeviceStartsOver(t *testing.T) {
	e := newEnv(t, cleanupAfter(cleanupAge))
	mac, laptop := e.device("mac"), e.device("laptop")
	gone := put(t, mac, bookmark("root", "Gone"))
	kept := put(t, mac, bookmark("root", "Kept"))
	sync(t, mac, laptop)
	if err := mac.Delete("bookmarks", gone.GUID); err != nil {
		t.Fatal(err)
	}
	sync(t, mac)
	time.Sleep(cleanupAge + 100*time.Millisecond)
	if n, err := e.srv.Cleanup(); err != nil || n != 1 {
		t.Fatalf("cleanup: %d %v", n, err)
	}

	sync(t, laptop)
	if holds(t, laptop, "bookmarks", gone.GUID) || !holds(t, laptop, "bookmarks", kept.GUID) {
		t.Fatal("the stale laptop didn't start over")
	}
	e.seedKey() // the bucket holds a copy of the key, so the devices heal it
	if err := e.srv.ResetStore("me"); err != nil {
		t.Fatal(err)
	}
	sync(t, laptop, mac)
	if _, has := stored(t, e.store, "bookmarks", gone.GUID); has {
		t.Fatal("a refill brought back an item deleted and cleaned up")
	}
}

// A new store id comes first: a stale device that may hold the only copy refills the server
// rather than start over.
func TestAStaleDeviceRefillsAWipedServer(t *testing.T) {
	e := newEnv(t, cleanupAfter(cleanupAge))
	laptop := e.device("laptop")
	b := put(t, laptop, bookmark("root", "Only here"))
	sync(t, laptop)
	time.Sleep(cleanupAge + 100*time.Millisecond)
	st := e.newServer()
	e.reopen(laptop)
	sync(t, laptop)
	if _, has := stored(t, st, "bookmarks", b.GUID); !has {
		t.Fatal("the wiped server didn't get the stale laptop's data back")
	}
}

// A crash during the start over leaves it to be done again.
func TestAStartOverSurvivesACrash(t *testing.T) {
	for failAt := 1; failAt <= 3; failAt++ {
		t.Run(fmt.Sprintf("crash at write %d", failAt), func(t *testing.T) {
			e := newEnv(t, cleanupAfter(cleanupAge))
			mac, laptop := e.device("mac"), e.device("laptop")
			gone := put(t, mac, bookmark("root", "Gone"))
			kept := put(t, mac, bookmark("root", "Kept"))
			sync(t, mac, laptop)
			mac.Delete("bookmarks", gone.GUID)
			sync(t, mac)
			time.Sleep(cleanupAge + 100*time.Millisecond)
			e.srv.Cleanup()

			crash := &crashing{Local: laptop.local, failAt: failAt}
			r, err := client.Open(client.Config{Client: &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}},
				Local: crash, Key: e.key})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Sync(t.Context()); !errors.Is(err, errCrash) {
				t.Fatalf("%v, want the crash", err)
			}
			e.reopen(laptop)
			sync(t, laptop)
			if holds(t, laptop, "bookmarks", gone.GUID) || !holds(t, laptop, "bookmarks", kept.GUID) {
				t.Fatal("the start over wasn't done again")
			}
		})
	}
}

// Inside the cleanup age a device keeps everything, and pulls on from its cursor.
func TestADeviceInsideTheAgeKeepsEverything(t *testing.T) {
	e := newEnv(t, cleanupAfter(time.Hour))
	mac, laptop := e.device("mac"), e.deviceWith("laptop", client.Config{KeepDeleted: true})
	b := put(t, mac, bookmark("root", "Deleted"))
	sync(t, mac, laptop)
	mac.Delete("bookmarks", b.GUID)
	sync(t, mac, laptop)
	put(t, mac, bookmark("root", "Next"))
	sync(t, mac, laptop)
	if !holds(t, laptop, "bookmarks", b.GUID) {
		t.Fatal("the laptop dropped a kept delete inside the cleanup age")
	}
	if got := cursor(t, laptop); got != last(t, e.store) {
		t.Fatalf("cursor %d, the server's last %d", got, last(t, e.store))
	}
}
