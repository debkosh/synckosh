// Package synckosh syncs items through a sync server. Everything synced is an Item: a type, a
// guid, a timestamp, a deleted flag and the type's data. For each guid the newest item wins,
// unless the type has its own Merger. The server sees the envelope; the data is sealed.
// The protocol is in SPEC.md.
package synckosh

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// TS orders items: the wall clock in milliseconds, a counter for edits in the same
// millisecond or after a newer timestamp was seen, and the device, which breaks ties.
// On the wire it is [ms, n, "device"].
type TS struct {
	MS  int64
	N   int64
	Dev string
}

// After reports whether t is newer than u.
func (t TS) After(u TS) bool {
	if t.MS != u.MS {
		return t.MS > u.MS
	}
	if t.N != u.N {
		return t.N > u.N
	}
	return t.Dev > u.Dev
}

func (t TS) IsZero() bool { return t == TS{} }

func (t TS) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{t.MS, t.N, t.Dev})
}

// UnmarshalJSON reads [ms, n, "device"]. Elements a later version may add are ignored.
func (t *TS) UnmarshalJSON(b []byte) error {
	var a []json.RawMessage
	if err := json.Unmarshal(b, &a); err != nil {
		return fmt.Errorf("ts: %w", err)
	}
	if len(a) < 3 {
		return fmt.Errorf("ts: want [ms, n, device], got %s", b)
	}
	var v TS
	if err := json.Unmarshal(a[0], &v.MS); err != nil {
		return fmt.Errorf("ts ms: %w", err)
	}
	if err := json.Unmarshal(a[1], &v.N); err != nil {
		return fmt.Errorf("ts n: %w", err)
	}
	if err := json.Unmarshal(a[2], &v.Dev); err != nil {
		return fmt.Errorf("ts device: %w", err)
	}
	*t = v
	return nil
}

// Item is one synced thing. Fields a later version adds to the envelope are kept in Extra and
// written back unchanged, so an older device never strips them.
type Item struct {
	Type    string
	GUID    string
	TS      TS
	Deleted bool
	Data    json.RawMessage
	Seq     int64 // the server's change number; set on items the server returns
	Base    int64 // on a push: the change number of the server's item this edit was made on, 0 for none
	Extra   map[string]json.RawMessage
}

var envelope = map[string]bool{"type": true, "guid": true, "ts": true, "deleted": true, "data": true, "seq": true, "base": true}

func (it Item) MarshalJSON() ([]byte, error) {
	m := make(map[string]json.RawMessage, len(it.Extra)+6)
	for k, v := range it.Extra {
		m[k] = v
	}
	put := func(k string, v any) error {
		b, err := json.Marshal(v)
		m[k] = b
		return err
	}
	if err := put("type", it.Type); err != nil {
		return nil, err
	}
	if err := put("guid", it.GUID); err != nil {
		return nil, err
	}
	if err := put("ts", it.TS); err != nil {
		return nil, err
	}
	if err := put("deleted", it.Deleted); err != nil {
		return nil, err
	}
	if len(it.Data) > 0 {
		m["data"] = it.Data
	}
	if it.Seq != 0 {
		if err := put("seq", it.Seq); err != nil {
			return nil, err
		}
	}
	if it.Base != 0 {
		if err := put("base", it.Base); err != nil {
			return nil, err
		}
	}
	return marshalSorted(m)
}

func (it *Item) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	var v Item
	get := func(k string, dst any) error {
		raw, ok := m[k]
		if !ok {
			return nil
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("item %s: %w", k, err)
		}
		return nil
	}
	for k, dst := range map[string]any{"type": &v.Type, "guid": &v.GUID, "ts": &v.TS, "deleted": &v.Deleted, "seq": &v.Seq, "base": &v.Base} {
		if err := get(k, dst); err != nil {
			return err
		}
	}
	if d, ok := m["data"]; ok && !bytes.Equal(d, []byte("null")) {
		v.Data = d
	}
	for k, raw := range m {
		if !envelope[k] {
			if v.Extra == nil {
				v.Extra = map[string]json.RawMessage{}
			}
			v.Extra[k] = raw
		}
	}
	*it = v
	return nil
}

// marshalSorted writes a map with its keys in order, so the same item is always the same bytes.
func marshalSorted(m map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		var c bytes.Buffer
		if err := json.Compact(&c, m[k]); err != nil {
			return nil, fmt.Errorf("item %s: %w", k, err)
		}
		buf.Write(c.Bytes())
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// sameValue reports whether two items say the same thing, whatever their timestamps.
func sameValue(a, b Item) bool {
	if a.Deleted != b.Deleted {
		return false
	}
	if a.Deleted {
		return true
	}
	return jsonEqual(a.Data, b.Data)
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y bytes.Buffer
	if json.Compact(&x, a) != nil || json.Compact(&y, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(x.Bytes(), y.Bytes())
}
