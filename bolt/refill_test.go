package bolt

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/debkosh/synckosh"
	"github.com/debkosh/synckosh/client"
)

var errCrash = errors.New("crash")

// crashing is a device's store that fails its failAt'th write, as if the app were killed then.
type crashing struct {
	client.Local
	writes, failAt int
}

func (c *crashing) write() error {
	c.writes++
	if c.writes == c.failAt {
		return errCrash
	}
	return nil
}

func (c *crashing) Put(es ...client.Entry) error {
	if err := c.write(); err != nil {
		return err
	}
	return c.Local.Put(es...)
}

func (c *crashing) Remove(collection string, guids ...string) error {
	if err := c.write(); err != nil {
		return err
	}
	return c.Local.Remove(collection, guids...)
}

func (c *crashing) SetMeta(key string, value []byte) error {
	if err := c.write(); err != nil {
		return err
	}
	return c.Local.SetMeta(key, value)
}

// A crash at any point of a refill leaves it to be done again: the next sync still pushes
// everything back, and reads the new store from the start.
func TestARefillSurvivesACrash(t *testing.T) {
	for failAt := 1; failAt <= 3; failAt++ {
		t.Run(fmt.Sprintf("crash at write %d", failAt), func(t *testing.T) {
			e := newEnv(t)
			mac := e.device("mac")
			var mine []synckosh.Item
			for _, name := range []string{"A", "B", "C"} {
				mine = append(mine, put(t, mac, bookmark("root", name)))
			}
			sync(t, mac)

			st := e.newServer()
			tablet := e.device("tablet")
			theirs := put(t, tablet, bookmark("root", "Tablet's"))
			sync(t, tablet) // the new store's first item, below the Mac's old cursor

			crash := &crashing{Local: mac.local, failAt: failAt}
			r, err := client.Open(client.Config{Client: &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}},
				Local: crash, Key: e.key})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Sync(t.Context()); !errors.Is(err, errCrash) {
				t.Fatalf("write %d: %v, want the crash", failAt, err)
			}

			e.reopen(mac)
			sync(t, mac)
			for _, it := range mine {
				if _, has := stored(t, st, "bookmarks", it.GUID); !has {
					t.Fatalf("crash at write %d: the refill lost %s", failAt, it.GUID)
				}
			}
			if !holds(t, mac, "bookmarks", theirs.GUID) {
				t.Fatalf("crash at write %d: the Mac didn't read the new store from the start", failAt)
			}
		})
	}
}
