package bolt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sudiptadeb/synckosh"
	"github.com/sudiptadeb/synckosh/client"
)

func visit(title string) synckosh.Item {
	return synckosh.Item{Collection: "history", Datatype: "visit", Private: json.RawMessage(fmt.Sprintf(`{"title":%q}`, title))}
}

// titles lists a collection's live items by title, sorted.
func titles(t *testing.T, d *device, collection string) string {
	t.Helper()
	var out []string
	for _, it := range items(t, d, collection) {
		out = append(out, private(t, it)["title"].(string))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// holds says whether a device's own file holds an entry.
func holds(t *testing.T, d *device, collection, guid string) bool {
	t.Helper()
	_, has, err := d.local.Get(collection, guid)
	if err != nil {
		t.Fatal(err)
	}
	return has
}

// A device removes an item once the server holds its delete, whichever device deleted it. Until
// then the delete stays pending, through a restart, so it still goes out.
func TestADeletedItemLeavesTheDevice(t *testing.T) {
	e := newEnv(t)
	mac, phone := e.device("mac"), e.device("phone")
	b := put(t, mac, bookmark("root", "Go"))
	sync(t, mac, phone)
	if err := mac.Delete("bookmarks", b.GUID); err != nil {
		t.Fatal(err)
	}
	if !holds(t, mac, "bookmarks", b.GUID) {
		t.Fatal("the pending delete is gone before it was pushed")
	}
	e.reopen(mac)
	sync(t, mac)
	if holds(t, mac, "bookmarks", b.GUID) {
		t.Fatal("the Mac still holds what it deleted, after the server took the delete")
	}
	sync(t, phone)
	if holds(t, phone, "bookmarks", b.GUID) {
		t.Fatal("the phone still holds what the Mac deleted")
	}
	if it, has := stored(t, e.store, "bookmarks", b.GUID); !has || !it.Deleted {
		t.Fatal("the server should keep the delete until its cleanup")
	}
}

// A device with KeepDeleted keeps deleted items, with their data, until the app cleans them up,
// and putting one again restores it everywhere.
func TestKeepDeletedRestores(t *testing.T) {
	e := newEnv(t)
	mac, phone := e.deviceWith("mac", client.Config{KeepDeleted: true}), e.device("phone")
	b := put(t, mac, bookmark("root", "Go"))
	sync(t, mac, phone)
	if err := phone.Delete("bookmarks", b.GUID); err != nil {
		t.Fatal(err)
	}
	sync(t, phone, mac)
	if got := items(t, mac, "bookmarks"); len(got) != 0 {
		t.Fatalf("a deleted item shown: %+v", got)
	}
	it, has, _ := mac.Get("bookmarks", b.GUID)
	if !has || !it.Deleted || private(t, it)["title"] != "Go" {
		t.Fatalf("KeepDeleted should keep the delete with its data: %v %+v", has, it)
	}
	if n, err := mac.Cleanup(time.Hour); err != nil || n != 0 {
		t.Fatalf("cleaned up a fresh delete: %d %v", n, err)
	}
	put(t, mac, it)
	sync(t, mac, phone)
	if got := titles(t, phone, "bookmarks"); got != "Go" {
		t.Fatalf("restored on the Mac; the phone shows %q", got)
	}
	if err := mac.Delete("bookmarks", b.GUID); err != nil {
		t.Fatal(err)
	}
	sync(t, mac)
	if n, err := mac.Cleanup(-time.Minute); err != nil || n != 1 || holds(t, mac, "bookmarks", b.GUID) {
		t.Fatalf("cleanup: %d %v", n, err)
	}
}

// DeleteBefore deletes a collection's older items everywhere. The server forgets them at once
// and never stores them again; a device removes them, or with KeepDeleted keeps them hidden.
// Either way the app sees the same.
func TestDeleteBefore(t *testing.T) {
	e := newEnv(t)
	mac, phone := e.device("mac"), e.device("phone")
	tablet := e.deviceWith("tablet", client.Config{KeepDeleted: true})
	old := put(t, mac, visit("old"))
	older := put(t, mac, visit("older"))
	sync(t, mac, phone, tablet)
	time.Sleep(5 * time.Millisecond)
	cut := time.Now()
	time.Sleep(5 * time.Millisecond)
	put(t, mac, visit("new"))
	if err := mac.DeleteBefore("history", cut); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, mac, "history"); got != "new" || holds(t, mac, "history", old.GUID) {
		t.Fatalf("on the Mac at once: %q", got)
	}
	sync(t, mac, phone, tablet)

	if _, has := stored(t, e.store, "history", old.GUID); has {
		t.Fatal("the server still holds an item before the cutoff")
	}
	for _, d := range []*device{mac, phone, tablet} {
		if got := titles(t, d, "history"); got != "new" {
			t.Errorf("%s shows %q, want only the item after the cutoff", d.name, got)
		}
	}
	if holds(t, phone, "history", old.GUID) {
		t.Fatal("the phone still holds an item before the cutoff")
	}
	if !holds(t, tablet, "history", old.GUID) {
		t.Fatal("the tablet keeps deleted items, and dropped one")
	}
	if it, _, _ := tablet.Get("history", old.GUID); !it.Deleted {
		t.Fatal("an item before the cutoff isn't reported deleted")
	}
	fresh := e.device("fresh")
	sync(t, fresh)
	if got := titles(t, fresh, "history"); got != "new" || holds(t, fresh, "history", old.GUID) {
		t.Fatalf("a new device downloaded %q", got)
	}

	// An earlier cutoff changes nothing: it hides nothing new and brings nothing back.
	if err := tablet.DeleteBefore("history", cut.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	sync(t, tablet, phone)
	cutItem, _ := stored(t, e.store, "history", synckosh.DeleteBeforeItem)
	if c, _ := synckosh.Cutoff(cutItem); c != cut.UnixMilli() {
		t.Fatalf("the cutoff moved back to %d from %d", c, cut.UnixMilli())
	}
	for _, d := range []*device{phone, tablet} {
		if got := titles(t, d, "history"); got != "new" {
			t.Errorf("%s after an earlier cutoff: %q", d.name, got)
		}
	}

	// A refill pushes the tablet's kept old items; the server answers OK and doesn't store them.
	if err := e.srv.ResetStore("me"); err != nil {
		t.Fatal(err)
	}
	sync(t, tablet)
	if _, has := stored(t, e.store, "history", older.GUID); has {
		t.Fatal("a refill brought back an item before the cutoff")
	}

	// An edit after the cutoff brings a hidden item back to life.
	it, _, _ := tablet.Get("history", old.GUID)
	put(t, tablet, it)
	sync(t, tablet, phone)
	if got := titles(t, phone, "history"); got != "new,old" {
		t.Fatalf("after editing a hidden item: %q", got)
	}

	// The tablet's app cleans up what it kept.
	if n, err := tablet.Cleanup(-time.Minute); err != nil || n != 1 || holds(t, tablet, "history", older.GUID) {
		t.Fatalf("cleanup: %d %v", n, err)
	}
}

func TestDeleteBeforeTooFarAheadIsRefused(t *testing.T) {
	e := newEnv(t)
	mac := e.device("mac")
	if err := mac.DeleteBefore("history", time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("a cutoff two hours ahead was written")
	}
	c := &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}}
	now := time.Now()
	cut := func(ts, before time.Time) synckosh.Item {
		it, _ := e.key.Seal(synckosh.Item{Collection: "history", Datatype: synckosh.DeleteBeforeItem, GUID: synckosh.DeleteBeforeItem,
			Timestamp: ts.UnixMilli(), Public: json.RawMessage(fmt.Sprintf(`{"before":%d}`, before.UnixMilli()))}, true)
		return it
	}
	res, err := c.Push(context.Background(), []synckosh.Item{cut(now, now.Add(2*time.Hour))})
	if err != nil || res.Results[0].Code != synckosh.RefusedTimestampAhead {
		t.Fatalf("a cutoff two hours ahead: %v %+v", err, res)
	}
	// One from a device that hadn't seen the later cutoff: the server answers with the one it has.
	c.Push(context.Background(), []synckosh.Item{cut(now, now)})
	res, err = c.Push(context.Background(), []synckosh.Item{cut(now.Add(time.Second), now.Add(-time.Hour))})
	if err != nil || res.Results[0].OK || res.Results[0].Current == nil {
		t.Fatalf("an earlier cutoff with a later timestamp: %v %+v", err, res)
	}
}
