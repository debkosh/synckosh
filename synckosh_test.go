package synckosh

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestItemKeepsFieldsItDoesNotKnow(t *testing.T) {
	in := `{"collection":"bookmarks","datatype":"bookmark","guid":"g1","timestamp":5,"deleted":false,"public":{"parent":"f1"},"private":{"key":"k","box":"x"},"sequence":7,"deleted_at":0,"later":2}`
	var it Item
	if err := json.Unmarshal([]byte(in), &it); err != nil {
		t.Fatal(err)
	}
	if it.Timestamp != 5 || it.Sequence != 7 || string(it.Extra["later"]) != "2" || string(it.Public) != `{"parent":"f1"}` {
		t.Fatalf("read %+v", it)
	}
	out, err := json.Marshal(it)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"collection":"bookmarks","datatype":"bookmark","deleted":false,"guid":"g1","later":2,"private":{"key":"k","box":"x"},"public":{"parent":"f1"},"sequence":7,"timestamp":5}`
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
	it := Item{Collection: "logins", Datatype: "login-field", GUID: "g1", Timestamp: 1, Base: 3,
		Public: json.RawMessage(`{"parent":"l1","key":"password"}`), Private: json.RawMessage(`{"value":"hunter2"}`)}
	s, err := k.Seal(it, false)
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
		"collection": func(i *Item) { i.Collection = "notes" },
		"datatype":   func(i *Item) { i.Datatype = "login" },
		"guid":       func(i *Item) { i.GUID = "g2" },
		"timestamp":  func(i *Item) { i.Timestamp = 2 },
		"deleted":    func(i *Item) { i.Deleted = true },
		"base":       func(i *Item) { i.Base = 0 },
		"public flag": func(i *Item) {
			i.Private = json.RawMessage(strings.Replace(string(i.Private), `"}`, `","public":true}`, 1))
		},
	} {
		c := s
		change(&c)
		if _, err := k.Open(c); err == nil {
			t.Errorf("opened after the server changed its %s", name)
		}
	}
	// Without the public copy the outer public part isn't checked.
	c := s
	c.Public = json.RawMessage(`{"parent":"l2"}`)
	if o, err := k.Open(c); err != nil || string(o.Public) != `{"parent":"l2"}` {
		t.Fatalf("public part outside the seal: %v %s", err, o.Public)
	}
	other, _ := NewDataKey()
	if _, err := other.Open(s); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("another key: %v", err)
	}
	for _, plain := range []Item{{Collection: "logins"}, {Collection: "logins", Private: json.RawMessage(`{"value":"x"}`)}} {
		if _, err := k.Open(plain); !errors.Is(err, ErrNotSealed) {
			t.Fatalf("an unsealed item: %v", err)
		}
	}
}

func TestSealWithThePublicPart(t *testing.T) {
	k, _ := NewDataKey()
	pub := "{\"t\":\"a<b&c\u2028\"}"
	it := Item{Collection: "notes", Datatype: "note", GUID: "g", Timestamp: 1, Public: json.RawMessage(pub)}
	s, err := k.Seal(it, true)
	if err != nil {
		t.Fatal(err)
	}
	s.Public = json.RawMessage(`{"t":"changed"}`)
	o, err := k.Open(s)
	if err != nil || string(o.Public) != pub || o.Private != nil {
		t.Fatalf("want the public part from the box: %v %s %s", err, o.Public, o.Private)
	}
	// The library's own items are always sealed with their public part, and refused without.
	own := Item{Collection: KeysCollection, Datatype: SealedKeyDatatype, GUID: "g", Timestamp: 1, Public: json.RawMessage(`{}`)}
	if s, _ := k.Seal(own, false); !strings.Contains(string(s.Private), `"public":true`) {
		t.Fatalf("a library item sealed without its public part: %s", s.Private)
	}
}

func TestSealedCopies(t *testing.T) {
	k, _ := NewDataKey()
	sealedCopy := func(pass, label string) Item {
		c, err := k.SealedCopy(pass, label)
		if err != nil {
			t.Fatal(err)
		}
		c.GUID, c.Timestamp = NewGUID(), 1
		if c, err = k.Seal(c, false); err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := sealedCopy("correct horse battery staple", "Main"), sealedCopy("a second passphrase", "Spare")
	if strings.Contains(string(a.Public), "correct") || a.Collection != KeysCollection {
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

	// A copy that opens with the passphrase but isn't sealed by the key inside: forged.
	other, _ := NewDataKey()
	forged, _ := other.SealedCopy("correct horse battery staple", "Main")
	forged.GUID, forged.Timestamp = NewGUID(), 1
	forged, _ = k.Seal(forged, false)
	if _, err := OpenSealedCopy(forged, "correct horse battery staple"); err == nil {
		t.Fatal("opened a copy not sealed by its own key")
	}
	removed := a
	removed.Deleted = true
	if _, err := OpenSealedCopy(removed, "correct horse battery staple"); err == nil {
		t.Fatal("opened a removed copy")
	}
}

// A joining device reads the Argon2 settings before it can verify them, so it refuses any no
// honest writer makes rather than panic or allocate a gigabyte.
func TestSealedCopyChecksSettings(t *testing.T) {
	k, _ := NewDataKey()
	c, _ := k.SealedCopy("pass", "Main")
	for name, change := range map[string]func(m map[string]any){
		"p=0":         func(m map[string]any) { m["p"] = 0 },
		"t=0":         func(m map[string]any) { m["t"] = 0 },
		"t=11":        func(m map[string]any) { m["t"] = 11 },
		"m of 1 GiB":  func(m map[string]any) { m["m"] = 1 << 20 },
		"m below 8p":  func(m map[string]any) { m["m"] = 31 },
		"short salt":  func(m map[string]any) { m["salt"] = "AAAA" },
		"another kdf": func(m map[string]any) { m["kdf"] = "scrypt" },
	} {
		var m map[string]any
		json.Unmarshal(c.Public, &m)
		change(m)
		bad := c
		bad.Public, _ = json.Marshal(m)
		bad.GUID, bad.Timestamp = "g", 1
		if _, err := OpenSealedCopy(bad, "pass"); err == nil || errors.Is(err, ErrPassphrase) {
			t.Errorf("%s: %v, want the settings refused", name, err)
		}
	}
}

func TestBackupCode(t *testing.T) {
	k, _ := NewDataKey()
	code, c, err := k.BackupCode("Printed")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.ReplaceAll(code, "-", "")); n != 26 {
		t.Fatalf("code %q: %d characters, want 26 (128 bits)", code, n)
	}
	c.GUID, c.Timestamp = NewGUID(), 1
	c, _ = k.Seal(c, false)
	got, err := OpenSealedCopy(c, code)
	if err != nil || got.ID != k.ID {
		t.Fatalf("%v", err)
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

func between(t *testing.T, a, b string) string {
	t.Helper()
	k, err := Between(a, b)
	if err != nil {
		t.Fatalf("Between(%q, %q): %v", a, b, err)
	}
	return k
}

func TestBetween(t *testing.T) {
	keys := []string{between(t, "", "")}
	// Insert at the start, the end and in the middle, many times; the list stays in order.
	for i := 0; i < 2000; i++ {
		switch i % 3 {
		case 0:
			keys = append([]string{between(t, "", keys[0])}, keys...)
		case 1:
			keys = append(keys, between(t, keys[len(keys)-1], ""))
		default:
			j := len(keys) / 2
			k := between(t, keys[j-1], keys[j])
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

// Keys come from other devices, so Between answers every input, never hanging or panicking.
func TestBetweenWithoutRoom(t *testing.T) {
	for _, c := range [][2]string{{"V", "V0"}, {"", "0"}, {"", "000"}, {"V", "V"}, {"W", "V"}, {"Vz", "V"}} {
		if k, err := Between(c[0], c[1]); !errors.Is(err, ErrNoRoom) {
			t.Errorf("Between(%q, %q) = %q, %v, want ErrNoRoom", c[0], c[1], k, err)
		}
	}
	for _, c := range [][2]string{{"", "!"}, {"a b", ""}, {"V", "é"}} {
		if _, err := Between(c[0], c[1]); err == nil || errors.Is(err, ErrNoRoom) {
			t.Errorf("Between(%q, %q): %v, want an error for the character", c[0], c[1], err)
		}
	}
	for _, c := range [][2]string{{"V0", "V1"}, {"zz", ""}, {"V", "V01"}, {"0", "1"}} {
		between(t, c[0], c[1])
	}
}

func FuzzBetween(f *testing.F) {
	for _, c := range [][2]string{{"", ""}, {"V", "V0"}, {"", "0"}, {"V", "W"}, {"zzz", ""}, {"0", "01"}, {"A0", "A01"}} {
		f.Add(c[0], c[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		k, err := Between(a, b)
		if err != nil {
			return
		}
		if k <= a || b != "" && k >= b || strings.HasSuffix(k, "0") {
			t.Fatalf("Between(%q, %q) = %q", a, b, k)
		}
	})
}

func TestValid(t *testing.T) {
	ok := Item{Collection: "bookmarks", Datatype: "bookmark", GUID: NewGUID(), Timestamp: 1}
	if err := Valid(ok); err != nil {
		t.Fatal(err)
	}
	if err := Valid(Item{Collection: KeysCollection, Datatype: SealedKeyDatatype, GUID: "g", Timestamp: 1}); err != nil {
		t.Fatalf("a sealed copy: %v", err)
	}
	if err := Valid(Item{Collection: "history", Datatype: DeleteBeforeItem, GUID: DeleteBeforeItem, Timestamp: 1,
		Public: json.RawMessage(`{"before":5}`)}); err != nil {
		t.Fatalf("a delete-before: %v", err)
	}
	for name, change := range map[string]func(*Item){
		"capital":                 func(i *Item) { i.Collection = "Bookmarks" },
		"newline":                 func(i *Item) { i.Datatype = "a\nb" },
		"reserved collection":     func(i *Item) { i.Collection = "_bookmarks" },
		"reserved datatype":       func(i *Item) { i.Datatype = "_bookmark" },
		"sealed key elsewhere":    func(i *Item) { i.Datatype = SealedKeyDatatype },
		"other type in keys":      func(i *Item) { i.Collection = KeysCollection },
		"guid with underscore":    func(i *Item) { i.GUID = "_g" },
		"guid with a space":       func(i *Item) { i.GUID = "a b" },
		"guid too long":           func(i *Item) { i.GUID = strings.Repeat("a", 129) },
		"collection too long":     func(i *Item) { i.Collection = strings.Repeat("a", 65) },
		"no timestamp":            func(i *Item) { i.Timestamp = 0 },
		"delete-before guid only": func(i *Item) { i.GUID = DeleteBeforeItem },
		"delete-before type only": func(i *Item) { i.Datatype = DeleteBeforeItem; i.Public = json.RawMessage(`{"before":1}`) },
		"delete-before deleted": func(i *Item) {
			i.Datatype, i.GUID, i.Deleted, i.Public = DeleteBeforeItem, DeleteBeforeItem, true, json.RawMessage(`{"before":1}`)
		},
		"delete-before without a cutoff": func(i *Item) { i.Datatype, i.GUID = DeleteBeforeItem, DeleteBeforeItem },
	} {
		it := ok
		change(&it)
		if Valid(it) == nil {
			t.Errorf("%s: valid", name)
		}
	}
}
