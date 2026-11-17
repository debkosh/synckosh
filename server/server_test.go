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

// A push over the server's limits is refused whole, with the limits, so the device can split it.
func TestAPushOverTheLimitsIsRefusedWithThem(t *testing.T) {
	srv := server.New(openStore(t), auth)
	srv.Limits.PushItems, srv.Limits.PushBytes = 2, 1000
	c := serve(t, srv)
	its := make([]synckosh.Item, 3)
	for i := range its {
		its[i] = synckosh.Item{Collection: "notes", Datatype: "note", GUID: synckosh.NewGUID(), Timestamp: time.Now().UnixMilli()}
	}
	var tooLarge *client.TooLargeError
	if _, err := c.Push(context.Background(), its); !errors.As(err, &tooLarge) || tooLarge.Limits.PushItems != 2 {
		t.Fatalf("three items to a server that takes two: %v", err)
	}
	big := its[:1]
	big[0].Public = []byte(`"` + strings.Repeat("x", 1000) + `"`)
	if _, err := c.Push(context.Background(), big); !errors.As(err, &tooLarge) || tooLarge.Limits.PushBytes != 1000 {
		t.Fatalf("a push over the server's bytes: %v", err)
	}
}

// A page stops at the bytes the device asks for, or the server's limit, but holds at least one
// item, and the pages together hold everything once.
func TestPagesStopAtTheirBytes(t *testing.T) {
	srv := server.New(openStore(t), auth)
	srv.Limits.PageBytes = 3000
	c := serve(t, srv)
	its := make([]synckosh.Item, 10)
	for i := range its {
		its[i] = note(synckosh.NewGUID())
		its[i].Public = []byte(`"` + strings.Repeat("x", 900) + `"`)
	}
	if _, err := c.Push(context.Background(), its); err != nil {
		t.Fatal(err)
	}
	for _, c2 := range []struct{ ask, perPage int }{{0, 3}, {2000, 2}, {10, 1}} {
		var after uint64
		got := 0
		for {
			p, err := c.Pull(context.Background(), "", after, c2.ask)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Items) > c2.perPage || len(p.Items) == 0 {
				t.Fatalf("asking for %d bytes: a page of %d items", c2.ask, len(p.Items))
			}
			got += len(p.Items)
			after = p.Next
			if !p.More {
				break
			}
		}
		if got != len(its) {
			t.Fatalf("asking for %d bytes: %d items in all, want %d", c2.ask, got, len(its))
		}
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
	p, _ := c.Pull(context.Background(), "", 0, 0)
	if len(p.Items) != 1 || string(p.Items[0].Public) != `{"v":2}` || p.Items[0].Sequence != 2 {
		t.Fatalf("want the second, as sequence 2: %+v", p.Items)
	}
}

// Cleanup can remove the item with the bucket's highest number; the next push still gets a
// higher one, so no device's cursor is passed over.
func TestCleanupNeverReusesANumber(t *testing.T) {
	srv := server.New(openStore(t), auth)
	srv.CleanupAfter = time.Millisecond
	c := serve(t, srv)
	gone := note("gone")
	gone.Deleted = true
	c.Push(context.Background(), []synckosh.Item{note("kept"), gone})
	time.Sleep(5 * time.Millisecond)
	if n, err := srv.Cleanup(); err != nil || n != 1 {
		t.Fatalf("cleanup: %d %v", n, err)
	}
	res, err := c.Push(context.Background(), []synckosh.Item{note("next")})
	if err != nil || res.Results[0].Sequence != 3 {
		t.Fatalf("after cleanup removed number 2: %v %+v", err, res)
	}
}

func TestBearer(t *testing.T) {
	buckets := map[string]string{"t-alice": "alice", "t-bob": "bob"}
	srv := server.New(openStore(t), server.Bearer(func(token string) (string, error) {
		if b, ok := buckets[token]; ok {
			return b, nil
		}
		return "", errors.New("unknown token")
	}))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	ctx := context.Background()
	alice, bob := &client.Client{URL: ts.URL, Token: "t-alice"}, &client.Client{URL: ts.URL, Token: "t-bob"}
	if _, err := alice.Push(ctx, []synckosh.Item{note("a")}); err != nil {
		t.Fatal(err)
	}
	if p, err := alice.Pull(ctx, "", 0, 0); err != nil || len(p.Items) != 1 {
		t.Fatalf("alice: %v %+v", err, p)
	}
	if p, err := bob.Pull(ctx, "", 0, 0); err != nil || len(p.Items) != 0 {
		t.Fatalf("bob sees alice's bucket: %v %+v", err, p)
	}
	local, err := bolt.OpenLocal(filepath.Join(t.TempDir(), "bob.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	r, _ := client.Open(client.Config{Client: bob, Local: local})
	r.Put(synckosh.Item{Collection: "notes", Datatype: "note"})
	if err := r.Sync(ctx); err != nil {
		t.Fatalf("a device signed in with a token: %v", err)
	}
	if p, _ := bob.Pull(ctx, "", 0, 0); len(p.Items) != 1 {
		t.Fatalf("bob's bucket: %+v", p)
	}
	for _, c := range []*client.Client{{URL: ts.URL, Token: "t-eve"}, {URL: ts.URL}} {
		if _, err := c.Pull(ctx, "", 0, 0); err == nil || !strings.Contains(err.Error(), "401") {
			t.Errorf("token %q: %v, want 401", c.Token, err)
		}
	}
}
