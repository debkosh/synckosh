package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sudiptadeb/synckosh"
	"github.com/sudiptadeb/synckosh/bolt"
	"github.com/sudiptadeb/synckosh/client"
	"github.com/sudiptadeb/synckosh/server"
)

func auth(r *http.Request) (string, error) {
	if b := r.Header.Get("X-Bucket"); b != "" {
		return b, nil
	}
	return "", errors.New("no bucket")
}

type signedIn string

func (b signedIn) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Bucket", string(b))
	return http.DefaultTransport.RoundTrip(r)
}

func openStore(t *testing.T) *bolt.Store {
	t.Helper()
	st, err := bolt.OpenStore(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// serve runs srv and returns a client signed in to bucket "me".
func serve(t *testing.T, srv *server.Server) *client.Client {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &client.Client{URL: ts.URL, HTTP: &http.Client{Transport: signedIn("me")}}
}

func TestPushRefusesBadItems(t *testing.T) {
	srv := server.New(openStore(t), auth)
	srv.Limits.ItemBytes = 1000
	c := serve(t, srv)
	now := time.Now().UnixMilli()
	item := func(collection, datatype, guid string) synckosh.Item {
		return synckosh.Item{Collection: collection, Datatype: datatype, GUID: guid, Timestamp: now}
	}
	big := item("notes", "note", "big")
	big.Public = []byte(`"` + strings.Repeat("x", 1000) + `"`)
	for _, c2 := range []struct {
		name string
		it   synckosh.Item
		code string
	}{
		{"capital in a collection", item("Notes", "note", "g"), synckosh.RefusedInvalid},
		{"an app's name starting with an underscore", item("_notes", "note", "g"), synckosh.RefusedInvalid},
		{"a guid starting with an underscore", item("notes", "note", "_g"), synckosh.RefusedInvalid},
		{"another datatype in the keys collection", item(synckosh.KeysCollection, "note", "g"), synckosh.RefusedInvalid},
		{"no timestamp", synckosh.Item{Collection: "notes", Datatype: "note", GUID: "g"}, synckosh.RefusedInvalid},
		{"over the item limit", big, synckosh.RefusedTooBig},
	} {
		res, err := c.Push(context.Background(), []synckosh.Item{c2.it})
		if err != nil {
			t.Fatal(err)
		}
		if r := res.Results[0]; r.OK || r.Code != c2.code || r.Error == "" {
			t.Errorf("%s: %+v, want %s", c2.name, r, c2.code)
		}
	}
	res, err := c.Push(context.Background(), []synckosh.Item{item(synckosh.KeysCollection, synckosh.SealedKeyDatatype, "g")})
	if err != nil || !res.Results[0].OK {
		t.Fatalf("a sealed copy: %v %+v", err, res)
	}
}

func TestPushOverTheItemCountIsRefused(t *testing.T) {
	srv := server.New(openStore(t), auth)
	srv.Limits.PushItems = 2
	c := serve(t, srv)
	its := make([]synckosh.Item, 3)
	for i := range its {
		its[i] = synckosh.Item{Collection: "notes", Datatype: "note", GUID: synckosh.NewGUID(), Timestamp: time.Now().UnixMilli()}
	}
	if _, err := c.Push(context.Background(), its); err == nil || !strings.Contains(err.Error(), "413") {
		t.Fatalf("three items to a server that takes two: %v", err)
	}
}
