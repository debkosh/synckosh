package synckosh

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

// testdata/vectors.json pins the wire for other implementations: a data key, items with the
// additional data their seal binds, what each box opens to, a sealed copy of the key with its
// passphrase, and items' public and private parts with their canonical JSON and identity.
type vectors struct {
	DataKey struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	} `json:"data_key"`
	Items []struct {
		Item        Item   `json:"item"`
		PublicInBox bool   `json:"public_in_box"`
		Additional  string `json:"additional_data"`
		Opens       string `json:"box_opens_to"`
		Sealed      Item   `json:"sealed"`
	} `json:"items"`
	SealedCopy struct {
		Passphrase string `json:"passphrase"`
		Item       Item   `json:"item"`
	} `json:"sealed_copy"`
	Identities []struct {
		Public     json.RawMessage `json:"public"`
		Private    json.RawMessage `json:"private"`
		Canonical  string          `json:"canonical"`
		Identity   string          `json:"identity"`
		WithoutKey string          `json:"identity_without_key"`
	} `json:"identities"`
}

func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if raw == nil {
		return "null"
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	kb, err := base64.RawStdEncoding.DecodeString(v.DataKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	key, err := DataKeyFrom(v.DataKey.ID, kb)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Items {
		it := c.Item
		if got := string(additional(it, c.PublicInBox)); got != c.Additional {
			t.Errorf("%s: additional data %q, want %q", it.GUID, got, c.Additional)
		}
		var p sealedPart
		if err := json.Unmarshal(c.Sealed.Private, &p); err != nil || p.Key != key.ID || p.Public != c.PublicInBox {
			t.Fatalf("%s: sealed private part %s: %v", it.GUID, c.Sealed.Private, err)
		}
		box, _ := base64.RawStdEncoding.DecodeString(p.Box)
		// Sealing what the box opens to, with the box's nonce, makes the same box.
		again, err := sealWith(key.key[:], box[:24], []byte(c.Opens), []byte(c.Additional))
		if err != nil || again != p.Box {
			t.Errorf("%s: sealing again with the same nonce gives another box: %v", it.GUID, err)
		}
		opened, err := key.Open(c.Sealed)
		if err != nil {
			t.Fatalf("%s: %v", it.GUID, err)
		}
		if compact(t, opened.Public) != compact(t, it.Public) || compact(t, opened.Private) != compact(t, it.Private) {
			t.Errorf("%s: opened to %s %s", it.GUID, opened.Public, opened.Private)
		}
	}
	got, err := OpenSealedCopy(v.SealedCopy.Item, v.SealedCopy.Passphrase)
	if err != nil || got.ID != key.ID || !bytes.Equal(got.Bytes(), key.Bytes()) {
		t.Fatalf("the sealed copy: %v", err)
	}
	if len(v.Identities) == 0 {
		t.Fatal("no identities")
	}
	for _, c := range v.Identities {
		can := SameContent(Item{Public: c.Public, Private: c.Private})
		if string(can) != c.Canonical {
			t.Errorf("canonical %s, want %s", can, c.Canonical)
		}
		if id := Identity(key, can); id != c.Identity {
			t.Errorf("%s: identity %s, want %s", c.Canonical, id, c.Identity)
		}
		if id := Identity(nil, can); id != c.WithoutKey {
			t.Errorf("%s: identity without a key %s, want %s", c.Canonical, id, c.WithoutKey)
		}
	}
}
