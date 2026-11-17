package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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

func note(guid string) synckosh.Item {
	return synckosh.Item{Collection: "notes", Datatype: "note", GUID: guid, Timestamp: time.Now().UnixMilli()}
}

// Two servers on one store, twenty devices pushing to one bucket at once: every push gets its
// own run of numbers, with no gaps, and together they use every number once.
func TestConcurrentPushesNumberInOrder(t *testing.T) {
	st := openStore(t)
	cs := []*client.Client{serve(t, server.New(st, auth)), serve(t, server.New(st, auth))}
	const pushes, each = 20, 5
	seqs := make([][]uint64, pushes)
	var wg sync.WaitGroup
	for p := range pushes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			its := make([]synckosh.Item, each)
			for i := range its {
				its[i] = note(synckosh.NewGUID())
			}
			res, err := cs[p%2].Push(context.Background(), its)
			if err != nil {
				t.Error(err)
				return
			}
			for _, r := range res.Results {
				seqs[p] = append(seqs[p], r.Sequence)
			}
		}()
	}
	wg.Wait()
	seen := map[uint64]bool{}
	for p, s := range seqs {
		for i, n := range s {
			if i > 0 && n != s[i-1]+1 {
				t.Fatalf("push %d numbered %v, with a gap", p, s)
			}
			if seen[n] {
				t.Fatalf("number %d given twice", n)
			}
			seen[n] = true
		}
	}
	for n := uint64(1); n <= pushes*each; n++ {
		if !seen[n] {
			t.Fatalf("number %d never given", n)
		}
	}
}

func TestSameGUIDTwiceInOnePush(t *testing.T) {
	c := serve(t, server.New(openStore(t), auth))
	a, b := note("g"), note("g")
	a.Public, b.Public = []byte(`{"v":1}`), []byte(`{"v":2}`)
	res, err := c.Push(context.Background(), []synckosh.Item{a, b})
	if err != nil || !res.Results[0].OK || !res.Results[1].OK || res.Results[1].Sequence != res.Results[0].Sequence+1 {
		t.Fatalf("%v %+v", err, res)
	}
	p, _ := c.Pull(context.Background(), "", 0)
	if len(p.Items) != 1 || string(p.Items[0].Public) != `{"v":2}` || p.Items[0].Sequence != 2 {
		t.Fatalf("want the second, as sequence 2: %+v", p.Items)
	}
}

// Cleanup can remove the item with the bucket's highest number; the next push still gets a
// higher one, so no device's cursor is passed over.
func TestCleanupNeverReusesANumber(t *testing.T) {
	srv := server.New(openStore(t), auth)
	c := serve(t, srv)
	gone := note("gone")
	gone.Deleted = true
	c.Push(context.Background(), []synckosh.Item{note("kept"), gone})
	if n, err := srv.Cleanup(-time.Minute); err != nil || n != 1 {
		t.Fatalf("cleanup: %d %v", n, err)
	}
	res, err := c.Push(context.Background(), []synckosh.Item{note("next")})
	if err != nil || res.Results[0].Sequence != 3 {
		t.Fatalf("after cleanup removed number 2: %v %+v", err, res)
	}
}
