package synckosh

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestItemKeepsFieldsItDoesNotKnow(t *testing.T) {
	in := `{"type":"bookmark","guid":"bm_1","ts":[5,1,"mac1",9],"deleted":false,"data":{"title":"Go"},"key":2,"seq":7}`
	var it Item
	if err := json.Unmarshal([]byte(in), &it); err != nil {
		t.Fatal(err)
	}
	if it.TS != (TS{5, 1, "mac1"}) || it.Seq != 7 || string(it.Extra["key"]) != "2" {
		t.Fatalf("read %+v", it)
	}
	out, err := json.Marshal(it)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"data":{"title":"Go"},"deleted":false,"guid":"bm_1","key":2,"seq":7,"ts":[5,1,"mac1"],"type":"bookmark"}`
	if string(out) != want {
		t.Fatalf("wrote\n%s\nwant\n%s", out, want)
	}
}

func TestTSOrder(t *testing.T) {
	cases := []struct {
		a, b TS
		want bool
	}{
		{TS{2, 0, "a"}, TS{1, 9, "z"}, true},
		{TS{1, 2, "a"}, TS{1, 1, "z"}, true},
		{TS{1, 1, "b"}, TS{1, 1, "a"}, true},
		{TS{1, 1, "a"}, TS{1, 1, "a"}, false},
	}
	for _, c := range cases {
		if got := c.a.After(c.b); got != c.want {
			t.Errorf("%v after %v = %v", c.a, c.b, got)
		}
	}
}

func TestClockNeverGoesBack(t *testing.T) {
	now := time.UnixMilli(1000)
	c := NewClock("phone")
	c.now = func() time.Time { return now }
	a := c.Now()
	b := c.Now()
	if !b.After(a) {
		t.Fatalf("%v then %v", a, b)
	}
	// The laptop's clock is a minute ahead; an edit here after seeing its item still wins.
	seen := TS{MS: 61000, N: 3, Dev: "laptop"}
	c.Observe(seen)
	if e := c.Now(); !e.After(seen) {
		t.Fatalf("edit %v after seeing %v", e, seen)
	}
	// A clock a year ahead doesn't drag this one along.
	c.Observe(TS{MS: now.Add(365 * 24 * time.Hour).UnixMilli(), Dev: "broken"})
	if e := c.Now(); e.MS > now.Add(MaxAhead).UnixMilli() {
		t.Fatalf("followed a clock a year ahead: %v", e)
	}
}

func TestSealBindsTheEnvelope(t *testing.T) {
	k, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	it := Item{Type: "login", GUID: "lg_1", TS: TS{1, 0, "mac1"}, Data: json.RawMessage(`{"password":"hunter2"}`)}
	s, err := k.Seal(it)
	if err != nil {
		t.Fatal(err)
	}
	if jsonEqual(s.Data, it.Data) {
		t.Fatal("data not sealed")
	}
	o, err := k.Open(s)
	if err != nil || !jsonEqual(o.Data, it.Data) {
		t.Fatalf("open: %v %s", err, o.Data)
	}
	moved := s
	moved.GUID = "lg_2"
	if _, err := k.Open(moved); err == nil {
		t.Fatal("sealed data opened under another guid")
	}
	newer := s
	newer.TS = TS{2, 0, "mac1"}
	if _, err := k.Open(newer); err == nil {
		t.Fatal("sealed data opened under a newer timestamp")
	}
	other, _ := NewKeys()
	if _, err := other.Open(s); err == nil {
		t.Fatal("opened with another key")
	}
}

func TestPassphrase(t *testing.T) {
	k, _ := NewKeys()
	item, err := k.Lock("correct horse battery staple", TS{1, 0, "mac1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unlock(item, "wrong"); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	k2, err := Unlock(item, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if k2.key != k.key {
		t.Fatal("unlocked a different key")
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
