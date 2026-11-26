package client_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/debkosh/synckosh"
	"github.com/debkosh/synckosh/bolt"
	"github.com/debkosh/synckosh/client"
)

func open(t *testing.T) (*client.Replica, *bolt.Local) {
	t.Helper()
	local, err := bolt.OpenLocal(filepath.Join(t.TempDir(), "device.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close() })
	key, err := synckosh.NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	r, err := client.Open(client.Config{Client: &client.Client{URL: "http://unused"}, Local: local, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	return r, local
}

func pending(t *testing.T, local *bolt.Local) int {
	t.Helper()
	es, err := local.Pending()
	if err != nil {
		t.Fatal(err)
	}
	return len(es)
}

// An edit the server would refuse for good is refused at once, and nothing of that write is kept.
func TestPutRefusesWhatTheServerWould(t *testing.T) {
	r, local := open(t)
	good := synckosh.Item{Collection: "notes", Datatype: "note", GUID: synckosh.NewGUID(), Private: json.RawMessage(`{"text":"hi"}`)}
	for name, bad := range map[string]synckosh.Item{
		"capital in a collection":  {Collection: "Notes", Datatype: "note", GUID: synckosh.NewGUID()},
		"the library's collection": {Collection: synckosh.LibraryCollection, Datatype: "note", GUID: synckosh.NewGUID()},
		"no guid":                  {Collection: "notes", Datatype: "note"},
		"over the item limit": {Collection: "notes", Datatype: "note", GUID: synckosh.NewGUID(),
			Private: json.RawMessage(`"` + strings.Repeat("x", 1<<20) + `"`)},
	} {
		if _, err := r.Put(good, bad); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if n := pending(t, local); n != 0 {
		t.Fatalf("%d entries kept from refused writes", n)
	}
}

func TestPutLeavesTheCallersItems(t *testing.T) {
	r, _ := open(t)
	in := []synckosh.Item{{Collection: "notes", Datatype: "note", GUID: synckosh.NewGUID(), Private: json.RawMessage(`{}`)}}
	out, err := r.Put(in...)
	if err != nil {
		t.Fatal(err)
	}
	if in[0].Timestamp != 0 {
		t.Fatalf("Put changed the caller's item: %+v", in[0])
	}
	if out[0].GUID != in[0].GUID || out[0].Timestamp == 0 {
		t.Fatalf("stored without its guid or a timestamp: %+v", out[0])
	}
}

// A Put with bad items stores none of them, and its error names every one, by its place in
// the call.
func TestPutNamesEveryBadItem(t *testing.T) {
	r, local := open(t)
	good := synckosh.Item{Collection: "notes", Datatype: "note", GUID: synckosh.NewGUID()}
	noGUID := synckosh.Item{Collection: "notes", Datatype: "note"}
	badGUID := synckosh.Item{Collection: "notes", Datatype: "note", GUID: "note 1"}
	capital := synckosh.Item{Collection: "Notes", Datatype: "note", GUID: synckosh.NewGUID()}
	_, err := r.Put(good, noGUID, badGUID, good, capital)
	var pe *client.PutError
	if !errors.As(err, &pe) {
		t.Fatalf("%v, want a *client.PutError", err)
	}
	var got []string
	for _, it := range pe.Items {
		got = append(got, fmt.Sprintf("%d %s %s", it.Index, it.Collection, it.GUID))
		if it.Reason == "" {
			t.Errorf("item %d: no reason", it.Index)
		}
	}
	want := []string{"1 notes ", "2 notes note 1", "4 Notes " + capital.GUID}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("named %q, want %q", got, want)
	}
	for _, it := range want {
		if !strings.Contains(err.Error(), "item "+it[:1]) {
			t.Errorf("the message doesn't name item %s: %v", it[:1], err)
		}
	}
	if n := pending(t, local); n != 0 {
		t.Fatalf("%d entries kept from a refused Put", n)
	}
}
