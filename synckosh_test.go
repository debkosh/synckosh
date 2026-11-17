package synckosh

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestItemKeepsFieldsItDoesNotKnow(t *testing.T) {
	in := `{"bucket":"bookmarks","type":"bookmark","guid":"g1","timestamp":5,"deleted":false,"public":{"parent":"f1"},"private":{"key":"k","box":"x"},"seen":7,"deleted_at":0,"later":2}`
	var it Item
	if err := json.Unmarshal([]byte(in), &it); err != nil {
		t.Fatal(err)
	}
	if it.Timestamp != 5 || it.Seen != 7 || string(it.Extra["later"]) != "2" || string(it.Public) != `{"parent":"f1"}` {
		t.Fatalf("read %+v", it)
	}
	out, err := json.Marshal(it)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"bucket":"bookmarks","deleted":false,"guid":"g1","later":2,"private":{"key":"k","box":"x"},"public":{"parent":"f1"},"seen":7,"timestamp":5,"type":"bookmark"}`
	if string(out) != want {
		t.Fatalf("wrote\n%s\nwant\n%s", out, want)
	}
}

func TestClockNeverGoesBack(t *testing.T) {
	now := time.UnixMilli(1000)
	c := NewClock()
	c.now = func() time.Time { return now }
	a := c.Now()
	b := c.Now()
	if b <= a {
		t.Fatalf("%v then %v", a, b)
	}
	// The laptop's clock is a minute ahead; an edit here after seeing its item still wins.
	seen := int64(61000)
	c.Observe(seen)
	if e := c.Now(); e <= seen {
		t.Fatalf("edit %v after seeing %v", e, seen)
	}
	// A clock a year ahead doesn't drag this one along.
	c.Observe(now.Add(365 * 24 * time.Hour).UnixMilli())
	if e := c.Now(); e > now.Add(MaxAhead).UnixMilli() {
		t.Fatalf("followed a clock a year ahead: %v", e)
	}
}

func TestSealBindsTheItem(t *testing.T) {
	k, err := NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	it := Item{Bucket: "logins", Type: "login-field", GUID: "g1", Timestamp: 1,
		Public: json.RawMessage(`{"parent":"l1","key":"password"}`), Private: json.RawMessage(`{"value":"hunter2"}`)}
	s, err := k.Seal(it)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(s.Private), "hunter2") {
		t.Fatal("private part not sealed")
	}
	o, err := k.Open(s)
	if err != nil || string(o.Private) != `{"value":"hunter2"}` {
		t.Fatalf("open: %v %s", err, o.Private)
	}
	for name, change := range map[string]func(*Item){
		"guid":      func(i *Item) { i.GUID = "g2" },
		"timestamp": func(i *Item) { i.Timestamp = 2 },
		"public":    func(i *Item) { i.Public = json.RawMessage(`{"parent":"l2","key":"password"}`) },
		"deleted":   func(i *Item) { i.Deleted = true },
	} {
		c := s
		change(&c)
		if _, err := k.Open(c); err == nil {
			t.Errorf("opened after the server changed its %s", name)
		}
	}
	other, _ := NewDataKey()
	if _, err := other.Open(s); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("another key: %v", err)
	}
}

func TestSealedCopies(t *testing.T) {
	k, _ := NewDataKey()
	a, err := k.SealedCopy("correct horse battery staple", "Main")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := k.SealedCopy("a second passphrase", "Spare")
	if strings.Contains(string(a.Public), "correct") || a.Bucket != KeysBucket {
		t.Fatalf("%s", a.Public)
	}
	if _, err := OpenSealedCopy(a, "wrong"); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	for _, c := range []struct {
		it   Item
		pass string
	}{{a, "correct horse battery staple"}, {b, "a second passphrase"}} {
		got, err := OpenSealedCopy(c.it, c.pass)
		if err != nil || got.ID != k.ID || string(got.Bytes()) != string(k.Bytes()) {
			t.Fatalf("%v %v", err, got)
		}
	}
}

func TestGUIDsSortByWhenMade(t *testing.T) {
	prev := NewGUID()
	for i := 0; i < 10000; i++ {
		g := NewGUID()
		if g <= prev {
			t.Fatalf("%s after %s", g, prev)
		}
		if len(g) != 36 || g[14] != '7' || !strings.ContainsRune("89ab", rune(g[19])) {
			t.Fatalf("not a UUIDv7: %s", g)
		}
		prev = g
	}
}

func TestBetween(t *testing.T) {
	keys := []string{Between("", "")}
	// Insert at the start, the end and in the middle, many times; the list stays in order.
	for i := 0; i < 2000; i++ {
		var k string
		switch i % 3 {
		case 0:
			k = Between("", keys[0])
			keys = append([]string{k}, keys...)
		case 1:
			k = Between(keys[len(keys)-1], "")
			keys = append(keys, k)
		default:
			j := len(keys) / 2
			k = Between(keys[j-1], keys[j])
			keys = append(keys[:j], append([]string{k}, keys[j:]...)...)
		}
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] >= keys[i] {
			t.Fatalf("keys %d and %d out of order: %q %q", i-1, i, keys[i-1], keys[i])
		}
	}
	if !Less("a", "2", "b", "1") || !Less("a", "1", "a", "2") {
		t.Fatal("Less")
	}
}
