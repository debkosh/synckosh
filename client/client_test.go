package client_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudiptadeb/synckosh"
	"github.com/sudiptadeb/synckosh/bolt"
	"github.com/sudiptadeb/synckosh/client"
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
	good := synckosh.Item{Collection: "notes", Datatype: "note", Private: json.RawMessage(`{"text":"hi"}`)}
	for name, bad := range map[string]synckosh.Item{
		"capital in a collection": {Collection: "Notes", Datatype: "note"},
		"reserved collection":     {Collection: "_notes", Datatype: "note"},
		"reserved datatype":       {Collection: "notes", Datatype: synckosh.SealedKeyDatatype},
		"over the item limit":     {Collection: "notes", Datatype: "note", Private: json.RawMessage(`"` + strings.Repeat("x", 1<<20) + `"`)},
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
	in := []synckosh.Item{{Collection: "notes", Datatype: "note", Private: json.RawMessage(`{}`)}}
	out, err := r.Put(in...)
	if err != nil {
		t.Fatal(err)
	}
	if in[0].GUID != "" || in[0].Timestamp != 0 {
		t.Fatalf("Put changed the caller's item: %+v", in[0])
	}
	if out[0].GUID == "" || out[0].Timestamp == 0 {
		t.Fatalf("stored without a guid or timestamp: %+v", out[0])
	}
}
