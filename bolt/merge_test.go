package bolt

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sudiptadeb/synckosh"
	"github.com/sudiptadeb/synckosh/client"
)

// tagSet merges items whose private part is {"tags": [...]}, a set that only grows; an edit
// wins over a delete. It counts its calls.
type tagSet struct{ calls atomic.Int32 }

func tagsOf(it synckosh.Item) []string {
	var p struct{ Tags []string }
	json.Unmarshal(it.Private, &p)
	return p.Tags
}

func withTags(tags ...string) json.RawMessage {
	sort.Strings(tags)
	b, _ := json.Marshal(map[string][]string{"tags": tags})
	return b
}

func (m *tagSet) merge(local, remote synckosh.Item) synckosh.Item {
	m.calls.Add(1)
	set := map[string]bool{}
	for _, t := range append(tagsOf(local), tagsOf(remote)...) {
		set[t] = true
	}
	var tags []string
	for t := range set {
		tags = append(tags, t)
	}
	out := remote
	out.Private, out.Deleted = withTags(tags...), local.Deleted && remote.Deleted
	return out
}

func (m *tagSet) config() client.Config {
	return client.Config{Mergers: map[string]client.Merger{"tagged": m.merge}}
}

const tagged = "x"

func tag(t *testing.T, d *device, tags ...string) {
	t.Helper()
	put(t, d, synckosh.Item{Collection: "notes", Datatype: "tagged", GUID: tagged, Private: withTags(tags...)})
}

func tags(t *testing.T, d *device) string {
	t.Helper()
	it, has, err := d.Get("notes", tagged)
	if err != nil || !has {
		t.Fatalf("%s: %v %v", d.name, has, err)
	}
	if it.Deleted {
		return "(deleted)"
	}
	return strings.Join(tagsOf(it), ",")
}

// flaky is a device's connection: before runs once just before its next push, as if other
// devices synced then, and with drop the next push reaches the server but its answer is lost.
type flaky struct {
	signedIn
	before func()
	drop   bool
}

func (f *flaky) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost {
		if b := f.before; b != nil {
			f.before = nil
			b()
		}
		if f.drop {
			f.drop = false
			if resp, err := f.signedIn.RoundTrip(r); err == nil {
				resp.Body.Close()
			}
			return nil, errors.New("connection lost")
		}
	}
	return f.signedIn.RoundTrip(r)
}

// flakyDevice opens a device on a flaky connection.
func (e *env) flakyDevice(name string, cfg client.Config) (*device, *flaky) {
	e.t.Helper()
	f := &flaky{signedIn: signedIn{"me"}}
	d := e.deviceWith(name, cfg)
	cfg.Client = &client.Client{URL: e.url, HTTP: &http.Client{Transport: f}}
	cfg.Local, cfg.Key = d.local, e.key
	r, err := client.Open(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	d.Replica = r
	return d, f
}

// settled syncs every device twice more and checks no push stored anything.
func settled(t *testing.T, e *env, ds ...*device) {
	t.Helper()
	sync(t, ds...)
	before := last(t, e.store)
	sync(t, ds...)
	sync(t, ds...)
	if after := last(t, e.store); after != before {
		t.Fatalf("still pushing: the server went from %d to %d", before, after)
	}
}

func TestOfflineEditsMerge(t *testing.T) {
	for _, macFirst := range []bool{true, false} {
		e := newEnv(t)
		m := &tagSet{}
		mac, phone := e.deviceWith("mac", m.config()), e.deviceWith("phone", m.config())
		tag(t, mac)
		sync(t, mac, phone)
		tag(t, mac, "mac")
		tag(t, phone, "phone")
		if macFirst {
			sync(t, mac, phone, mac)
		} else {
			sync(t, phone, mac, phone)
		}
		settled(t, e, mac, phone)
		for _, d := range []*device{mac, phone} {
			if got := tags(t, d); got != "mac,phone" {
				t.Errorf("mac first %v: %s has %s", macFirst, d.name, got)
			}
		}
		if n := m.calls.Load(); n != 1 {
			t.Errorf("mac first %v: %d merges, want the second device's one", macFirst, n)
		}
	}
}

func TestThreeDevicesFromOneBase(t *testing.T) {
	e := newEnv(t)
	m := &tagSet{}
	a, b, c := e.deviceWith("a", m.config()), e.deviceWith("b", m.config()), e.deviceWith("c", m.config())
	tag(t, a)
	sync(t, a, b, c)
	tag(t, a, "a")
	tag(t, b, "b")
	tag(t, c, "c")
	sync(t, a, b, c)
	settled(t, e, a, b, c)
	for _, d := range []*device{a, b, c} {
		if got := tags(t, d); got != "a,b,c" {
			t.Errorf("%s has %s", d.name, got)
		}
	}
}

// Two idle devices that hold the same version both merge a concurrent one: they make the same
// content, and each then takes the other's instead of merging again.
func TestIdleDevicesMergeOnce(t *testing.T) {
	e := newEnv(t)
	m := &tagSet{}
	a, fa := e.flakyDevice("a", m.config())
	b, fb := e.flakyDevice("b", m.config())
	d := e.deviceWith("d", m.config())
	tag(t, a)
	sync(t, a, b, d)
	tag(t, a, "a")
	tag(t, b, "b")
	fb.before = func() { sync(t, a, d) } // a's edit lands, d takes it, then b's lands over it unmerged
	sync(t, b)
	fa.before = func() { sync(t, d) } // d merges, then a pushes its own merge of the same pair
	sync(t, a)
	settled(t, e, a, b, d)
	for _, x := range []*device{a, b, d} {
		if got := tags(t, x); got != "a,b" {
			t.Errorf("%s has %s", x.name, got)
		}
	}
	if n := m.calls.Load(); n < 2 {
		t.Errorf("%d merges: both idle devices should have merged", n)
	}
}

// A merge that loses a race to another edit is merged again.
func TestAMergeThatLosesIsMergedAgain(t *testing.T) {
	e := newEnv(t)
	m := &tagSet{}
	a, fa := e.flakyDevice("a", m.config())
	b, c := e.deviceWith("b", m.config()), e.deviceWith("c", m.config())
	tag(t, a)
	sync(t, a, b, c)
	tag(t, c, "c")
	sync(t, c)
	tag(t, a, "a")
	tag(t, b, "b")
	fa.before = func() { sync(t, b) } // b merges and pushes after a merged, before a pushes
	sync(t, a)
	settled(t, e, a, b, c)
	for _, x := range []*device{a, b, c} {
		if got := tags(t, x); got != "a,b,c" {
			t.Errorf("%s has %s", x.name, got)
		}
	}
	if n := m.calls.Load(); n < 3 {
		t.Errorf("%d merges: a's merge should have been merged again", n)
	}
}

func TestALaterEditIsTakenWithoutMerging(t *testing.T) {
	e := newEnv(t)
	m := &tagSet{}
	mac, phone := e.deviceWith("mac", m.config()), e.deviceWith("phone", m.config())
	tag(t, mac, "a")
	sync(t, mac, phone)
	tag(t, phone, "b") // made from what the Mac pushed
	sync(t, phone, mac)
	if got := tags(t, mac); got != "b" || m.calls.Load() != 0 {
		t.Fatalf("the Mac has %s after %d merges, want b and none", got, m.calls.Load())
	}
}

// A push whose answer is lost is still this device's own edit: when it comes back, the device
// doesn't merge it into its next edit, which would bring back what that edit removed.
func TestALostAnswerIsNotMerged(t *testing.T) {
	e := newEnv(t)
	m := &tagSet{}
	mac, f := e.flakyDevice("mac", m.config())
	phone := e.deviceWith("phone", m.config())
	tag(t, mac)
	sync(t, mac)
	tag(t, mac, "keep", "remove")
	f.drop = true
	if err := mac.Sync(t.Context()); err == nil {
		t.Fatal("the lost answer didn't fail the sync")
	}
	tag(t, mac, "keep")
	sync(t, mac, phone)
	if got := tags(t, mac) + " " + tags(t, phone); got != "keep keep" || m.calls.Load() != 0 {
		t.Fatalf("%s after %d merges, want keep on both and none", got, m.calls.Load())
	}
}

// After a store reset no device merges: a refill is newest wins.
func TestARefillNeverMerges(t *testing.T) {
	e := newEnv(t)
	m := &tagSet{}
	mac, phone := e.deviceWith("mac", m.config()), e.deviceWith("phone", m.config())
	tag(t, mac)
	sync(t, mac, phone)
	tag(t, mac, "1")
	sync(t, mac)
	tag(t, mac, "2")
	sync(t, mac) // the phone has been away all this time
	e.newServer()
	e.reopen(mac)
	e.reopen(phone)
	sync(t, mac, phone)
	settled(t, e, mac, phone)
	if got := tags(t, phone); got != "2" || m.calls.Load() != 0 {
		t.Fatalf("the phone has %s after %d merges", got, m.calls.Load())
	}
}

func TestNoMergerKeepsTheNewest(t *testing.T) {
	e := newEnv(t)
	mac, phone := e.device("mac"), e.device("phone")
	tag(t, mac)
	sync(t, mac, phone)
	tag(t, mac, "mac")
	tag(t, phone, "phone")
	sync(t, mac, phone, mac)
	for _, d := range []*device{mac, phone} {
		if got := tags(t, d); got != "phone" {
			t.Errorf("%s has %s, want the later edit", d.name, got)
		}
	}
}

func TestALockedVersionIsNotMerged(t *testing.T) {
	e := newEnv(t)
	m := &tagSet{}
	mac, phone := e.deviceWith("mac", m.config()), e.deviceWith("phone", m.config())
	tag(t, mac)
	sync(t, mac, phone)
	tag(t, phone, "phone")
	other, _ := synckosh.NewDataKey()
	it, _ := stored(t, e.store, "notes", tagged)
	it.Timestamp++
	it, _ = other.Seal(it, false)
	forge(t, e.store, it)
	sync(t, phone)
	if got := tags(t, phone); got != "phone" || m.calls.Load() != 0 {
		t.Fatalf("%s after %d merges", got, m.calls.Load())
	}
}

// The merger sees a delete against an edit, and decides: this one keeps the edit.
func TestADeleteAgainstAnEdit(t *testing.T) {
	e := newEnv(t)
	m := &tagSet{}
	mac, phone := e.deviceWith("mac", m.config()), e.deviceWith("phone", m.config())
	tag(t, mac, "a")
	sync(t, mac, phone)
	if err := mac.Delete("notes", tagged); err != nil {
		t.Fatal(err)
	}
	tag(t, phone, "a", "b")
	sync(t, mac, phone, mac)
	settled(t, e, mac, phone)
	for _, d := range []*device{mac, phone} {
		if got := tags(t, d); got != "a,b" {
			t.Errorf("%s has %s", d.name, got)
		}
	}
}

// What the claim says a server can do: replay an old version a key holder really wrote. A
// device with an edit from a later version merges it, and what that edit removed comes back.
func TestAReplayedVersionIsMerged(t *testing.T) {
	e := newEnv(t)
	m := &tagSet{}
	mac, phone := e.deviceWith("mac", m.config()), e.deviceWith("phone", m.config())
	tag(t, mac, "old")
	sync(t, mac)
	v0, _ := stored(t, e.store, "notes", tagged)
	tag(t, mac)
	sync(t, mac, phone)
	tag(t, phone, "phone")
	forge(t, e.store, v0)
	sync(t, phone)
	if got := tags(t, phone); got != "old,phone" {
		t.Fatalf("the phone has %s", got)
	}
}
