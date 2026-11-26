// Package storetest checks that a server.Store keeps the contract the server relies on. A store's
// own tests call Run with a way to open an empty store.
package storetest

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/debkosh/synckosh"
	"github.com/debkosh/synckosh/server"
)

// Run runs every check, each on a new empty store from open.
func Run(t *testing.T, open func(t *testing.T) server.Store) {
	for _, c := range []struct {
		name string
		fn   func(*testing.T, server.Store)
	}{
		{"StoreID", storeID},
		{"UpdateAndSince", updateAndSince},
		{"FailedUpdateStoresNothing", failedUpdate},
		{"Cleanup", cleanup},
		{"RemoveBefore", removeBefore},
		{"Identity", identity},
	} {
		t.Run(c.name, func(t *testing.T) { c.fn(t, open(t)) })
	}
}

func item(collection, guid string, seq uint64) synckosh.Item {
	return synckosh.Item{Collection: collection, Datatype: "note", GUID: guid, Timestamp: 1, Sequence: seq}
}

func put(t *testing.T, st server.Store, bucket string, its ...synckosh.Item) {
	t.Helper()
	if err := st.Update(bucket, func(tx server.Tx) error { return tx.Put(its) }); err != nil {
		t.Fatal(err)
	}
}

// since returns "collection/guid@sequence" for each item Since gives, of one collection or of
// every collection if collection is empty.
func since(t *testing.T, st server.Store, bucket, collection string, after uint64, most int) []string {
	t.Helper()
	var f synckosh.Filter
	if collection != "" {
		f.Collections = []string{collection}
	}
	return sinceFiltered(t, st, bucket, f, after, most)
}

func sinceFiltered(t *testing.T, st server.Store, bucket string, f synckosh.Filter, after uint64, most int) []string {
	t.Helper()
	var out []string
	err := st.Since(bucket, f, after, func(it synckosh.Item) bool {
		out = append(out, fmt.Sprintf("%s/%s@%d", it.Collection, it.GUID, it.Sequence))
		return len(out) < most
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func last(t *testing.T, st server.Store, bucket string) uint64 {
	t.Helper()
	var n uint64
	err := st.Update(bucket, func(tx server.Tx) error {
		var err error
		n, err = tx.Last()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func same(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s: %v, want %v", what, got, want)
	}
}

func storeID(t *testing.T, st server.Store) {
	ids := make([]string, 10)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := st.StoreID("a")
			if err != nil {
				t.Error(err)
			}
			ids[i] = id
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("first use from ten callers: %v", ids)
		}
	}
	if err := st.SetStoreID("a", "s_new"); err != nil {
		t.Fatal(err)
	}
	if id, _ := st.StoreID("a"); id != "s_new" {
		t.Fatalf("after SetStoreID: %s", id)
	}
	b, _ := st.StoreID("b")
	if err := st.ResetAll(); err != nil {
		t.Fatal(err)
	}
	a2, _ := st.StoreID("a")
	b2, _ := st.StoreID("b")
	if a2 == "s_new" || b2 == b || a2 == "" || b2 == "" {
		t.Fatalf("ResetAll left a store id: %s %s", a2, b2)
	}
}

func updateAndSince(t *testing.T, st server.Store) {
	put(t, st, "a", item("notes", "n1", 1), item("tasks", "t1", 2), item("notes", "n2", 3))
	same(t, "every collection", since(t, st, "a", "", 0, 100), "notes/n1@1", "tasks/t1@2", "notes/n2@3")
	same(t, "one collection", since(t, st, "a", "notes", 0, 100), "notes/n1@1", "notes/n2@3")
	same(t, "after 1", since(t, st, "a", "", 1, 100), "tasks/t1@2", "notes/n2@3")
	same(t, "until fn says stop", since(t, st, "a", "", 0, 2), "notes/n1@1", "tasks/t1@2")
	same(t, "another bucket", since(t, st, "b", "", 0, 100))
	task := item("notes", "n3", 5)
	task.Datatype = "task"
	put(t, st, "a", task)
	same(t, "two collections", sinceFiltered(t, st, "a", synckosh.Filter{Collections: []string{"tasks", "notes"}}, 0, 100),
		"notes/n1@1", "tasks/t1@2", "notes/n2@3", "notes/n3@5")
	same(t, "one datatype", sinceFiltered(t, st, "a", synckosh.Filter{Datatypes: []string{"task"}}, 0, 100), "notes/n3@5")
	same(t, "a collection and a datatype", sinceFiltered(t, st, "a",
		synckosh.Filter{Collections: []string{"notes"}, Datatypes: []string{"note"}}, 1, 100), "notes/n2@3")
	same(t, "until fn says stop, filtered", sinceFiltered(t, st, "a", synckosh.Filter{Collections: []string{"notes"}}, 0, 1),
		"notes/n1@1")

	put(t, st, "a", item("notes", "n1", 6)) // a new version replaces the old one
	same(t, "after an edit", since(t, st, "a", "", 0, 100), "tasks/t1@2", "notes/n2@3", "notes/n3@5", "notes/n1@6")
	same(t, "one collection after an edit", since(t, st, "a", "notes", 0, 100), "notes/n2@3", "notes/n3@5", "notes/n1@6")
	if n := last(t, st, "a"); n != 6 {
		t.Fatalf("last %d, want 6", n)
	}
	var got synckosh.Item
	var has bool
	st.Update("a", func(tx server.Tx) error {
		var err error
		got, has, err = tx.Get("notes", "n1")
		return err
	})
	if !has || got.Sequence != 6 {
		t.Fatalf("Get: %v %+v", has, got)
	}
	if n := last(t, st, "b"); n != 0 {
		t.Fatalf("another bucket's last: %d", n)
	}
}

func failedUpdate(t *testing.T, st server.Store) {
	put(t, st, "a", item("notes", "n1", 1))
	boom := errors.New("boom")
	err := st.Update("a", func(tx server.Tx) error {
		if err := tx.Put([]synckosh.Item{item("notes", "n2", 2), item("notes", "n1", 3)}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update returned %v, want fn's error", err)
	}
	same(t, "after a failed Update", since(t, st, "a", "", 0, 100), "notes/n1@1")
	if n := last(t, st, "a"); n != 1 {
		t.Fatalf("last %d after a failed Update, want 1", n)
	}
}

func cleanup(t *testing.T, st server.Store) {
	old, recent := item("notes", "old", 2), item("notes", "recent", 3)
	old.Deleted, old.DeletedAt = true, 100
	recent.Deleted, recent.DeletedAt = true, 300
	put(t, st, "a", item("notes", "live", 1), recent, old)
	put(t, st, "b", old)
	n, err := st.Cleanup(200)
	if err != nil || n != 2 {
		t.Fatalf("Cleanup: %d %v, want 2 removed", n, err)
	}
	same(t, "after cleanup", since(t, st, "a", "", 0, 100), "notes/live@1", "notes/recent@3")
	same(t, "after cleanup, one collection", since(t, st, "a", "notes", 0, 100), "notes/live@1", "notes/recent@3")
	same(t, "the other bucket", since(t, st, "b", "", 0, 100))
	// The removed item had the bucket's highest number; the counter doesn't go back.
	old.Deleted, old.Sequence = false, 4
	put(t, st, "b", old)
	if n := last(t, st, "b"); n != 4 {
		t.Fatalf("last %d", n)
	}
	if n := last(t, st, "a"); n != 3 {
		t.Fatalf("last %d after cleanup, want 3", n)
	}
}

// removeBefore pins the delete-before rule: every item of the collection at or before the
// cutoff goes, and nothing elsewhere.
func removeBefore(t *testing.T, st server.Store) {
	at := func(collection, guid string, seq uint64, ts int64) synckosh.Item {
		it := item(collection, guid, seq)
		it.Timestamp = ts
		return it
	}
	put(t, st, "a", at("history", "old", 1, 100), at("history", "edge", 2, 200), at("history", "new", 3, 201),
		at("notes", "other", 4, 100))
	put(t, st, "b", at("history", "old", 1, 100))
	if err := st.Update("a", func(tx server.Tx) error { return tx.RemoveBefore("history", 200) }); err != nil {
		t.Fatal(err)
	}
	same(t, "after RemoveBefore", since(t, st, "a", "", 0, 100), "history/new@3", "notes/other@4")
	same(t, "one collection", since(t, st, "a", "history", 0, 100), "history/new@3")
	same(t, "another bucket", since(t, st, "b", "", 0, 100), "history/old@1")
	if n := last(t, st, "a"); n != 4 {
		t.Fatalf("last %d", n)
	}
}

// identity pins the identity index: a live item of the collection with that identity, never a
// deleted or removed one, and none from another collection or bucket.
func identity(t *testing.T, st server.Store) {
	find := func(bucket, collection, id string) string {
		t.Helper()
		var got string
		err := st.Update(bucket, func(tx server.Tx) error {
			it, has, err := tx.Identity(collection, id)
			if has {
				got = it.GUID
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	with := func(guid, id string, seq uint64) synckosh.Item {
		it := item("notes", guid, seq)
		it.Identity = id
		return it
	}
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	put(t, st, "x", with("n1", a, 1), with("n2", b, 2))
	if got := find("x", "notes", a); got != "n1" {
		t.Fatalf("identity a: %q, want n1", got)
	}
	if find("x", "tasks", a) != "" || find("y", "notes", a) != "" {
		t.Fatal("found in another collection or bucket")
	}
	put(t, st, "x", with("n1", b, 3)) // an edit with another identity
	if got := find("x", "notes", a); got != "" {
		t.Fatalf("identity a after n1 changed: %q", got)
	}
	if got := find("x", "notes", b); got != "n1" && got != "n2" {
		t.Fatalf("identity b: %q", got)
	}
	gone := with("n2", b, 4)
	gone.Deleted, gone.DeletedAt = true, 100
	put(t, st, "x", gone)
	if got := find("x", "notes", b); got != "n1" {
		t.Fatalf("identity b with n2 deleted: %q, want n1", got)
	}
	if err := st.Update("x", func(tx server.Tx) error { return tx.RemoveBefore("notes", 3) }); err != nil {
		t.Fatal(err)
	}
	if got := find("x", "notes", b); got != "" {
		t.Fatalf("identity b after RemoveBefore: %q", got)
	}
}
