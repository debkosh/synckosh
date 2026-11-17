package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sudiptadeb/synckosh"
	"github.com/sudiptadeb/synckosh/client"
)

func note(size int) synckosh.Item {
	return synckosh.Item{Collection: "notes", Datatype: "note",
		Private: json.RawMessage(fmt.Sprintf(`{"text":%q}`, strings.Repeat("x", size)))}
}

func putMany(t *testing.T, d *device, n, size int) {
	t.Helper()
	for range n {
		put(t, d, note(size))
	}
}

// Large data never stops sync: a push holds what fits in the bytes allowed, and a page too. The
// sizes are the defaults' divided by 16, which keeps the test quick under the race detector: 60
// items of 300 KB and 40 near 1 MB, against pushes and pages of 16 MB.
func TestSyncByBytes(t *testing.T) {
	limits := synckosh.Limits{ItemBytes: 64 << 10, PushBytes: 1 << 20, PageBytes: 1 << 20}
	e := newEnv(t, withLimits(limits))
	mac, macNet := e.flakyDevice("mac", client.Config{Limits: limits})
	phone, phoneNet := e.flakyDevice("phone", client.Config{Limits: limits})
	putMany(t, mac, 60, 300<<10/16)
	sync(t, mac)
	if macNet.pushes < 2 {
		t.Fatalf("60 items of 19 KB in %d push, want several", macNet.pushes)
	}
	putMany(t, mac, 40, 700<<10/16) // sealed, near 64 KB each
	sync(t, mac, phone)
	if got := len(items(t, phone, "notes")); got != 100 {
		t.Fatalf("the phone has %d notes, want 100", got)
	}
	if phoneNet.pulls < 3 {
		t.Fatalf("2.6 MB pulled in %d pages of 1 MB, want several", phoneNet.pulls)
	}
}

// Each side sets its own limits. A server with smaller ones refuses a push as too large, with
// its limits, and the device splits it and carries on; a device with a smaller page gets smaller
// pages.
func TestLimitsDifferOnEachSide(t *testing.T) {
	e := newEnv(t, withLimits(synckosh.Limits{PushItems: 7, PushBytes: 20 << 10}))
	mac, macNet := e.flakyDevice("mac", client.Config{})
	phone, phoneNet := e.flakyDevice("phone", client.Config{Limits: synckosh.Limits{PageBytes: 8 << 10}})
	putMany(t, mac, 50, 1000)
	sync(t, mac, phone)
	if got := len(items(t, phone, "notes")); got != 50 {
		t.Fatalf("the phone has %d notes, want 50", got)
	}
	if macNet.pushes < 8 {
		t.Fatalf("%d pushes to a server that takes 7 items at a time", macNet.pushes)
	}
	if phoneNet.pulls < 6 {
		t.Fatalf("%d pages of 8 KB for 50 KB", phoneNet.pulls)
	}
}

// An item over the server's item limit is refused, stays pending and is reported; the rest
// syncs.
func TestAnItemOverTheServersLimit(t *testing.T) {
	e := newEnv(t, withLimits(synckosh.Limits{ItemBytes: 4 << 10}))
	mac := e.device("mac")
	big := put(t, mac, note(8<<10))
	small := put(t, mac, note(10))
	err := mac.Sync(context.Background())
	var refused *client.RefusedError
	if !errors.As(err, &refused) || len(refused.Items) != 1 || refused.Items[0].GUID != big.GUID || refused.Items[0].Code != synckosh.RefusedTooBig {
		t.Fatalf("sync: %v", err)
	}
	if _, has := stored(t, e.store, "notes", small.GUID); !has {
		t.Fatal("the small item didn't go out")
	}
	if e, _, _ := mac.local.Get("notes", big.GUID); !e.Pending {
		t.Fatal("the refused item isn't pending")
	}
}
