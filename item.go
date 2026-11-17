// Package synckosh holds what clients and servers share: the item, its timestamp, guids,
// order keys and the keys that seal an item's private part. The protocol is in SPEC.md; the
// client is in package client and the server in package server.
package synckosh

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// TS is when an edit was made: the device's clock in milliseconds, a counter, and the device.
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

// UnmarshalJSON reads [ms, n, "device"]. Elements a later version adds are ignored.
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

// Item is one synced thing. Public is plain: the server and a device that hasn't unlocked can
// read it. Private is plain on the device and sealed on the wire. Seen and DeletedAt are set
// by the server. Fields a later version adds are kept in Extra and written back unchanged.
type Item struct {
	Bucket    string
	Type      string
	GUID      string
	TS        TS
	Deleted   bool
	Public    json.RawMessage
	Private   json.RawMessage
	Seen      uint64 // the server's change number for this version
	DeletedAt int64  // the server's clock, in milliseconds, when it stored the delete
	Extra     map[string]json.RawMessage
}

var known = map[string]bool{"bucket": true, "type": true, "guid": true, "ts": true, "deleted": true,
	"public": true, "private": true, "seen": true, "deleted_at": true}

func (it Item) MarshalJSON() ([]byte, error) {
	m := make(map[string]json.RawMessage, len(it.Extra)+9)
	for k, v := range it.Extra {
		m[k] = v
	}
	set := func(k string, v any) {
		b, _ := json.Marshal(v)
		m[k] = b
	}
	set("bucket", it.Bucket)
	set("type", it.Type)
	set("guid", it.GUID)
	set("ts", it.TS)
	set("deleted", it.Deleted)
	if len(it.Public) > 0 {
		m["public"] = it.Public
	}
	if len(it.Private) > 0 {
		m["private"] = it.Private
	}
	if it.Seen != 0 {
		set("seen", it.Seen)
	}
	if it.DeletedAt != 0 {
		set("deleted_at", it.DeletedAt)
	}
	return marshalSorted(m)
}

func (it *Item) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	var v Item
	fields := map[string]any{"bucket": &v.Bucket, "type": &v.Type, "guid": &v.GUID, "ts": &v.TS,
		"deleted": &v.Deleted, "seen": &v.Seen, "deleted_at": &v.DeletedAt}
	for k, dst := range fields {
		if raw, ok := m[k]; ok {
			if err := json.Unmarshal(raw, dst); err != nil {
				return fmt.Errorf("item %s: %w", k, err)
			}
		}
	}
	for _, k := range []string{"public", "private"} {
		if raw, ok := m[k]; ok && !bytes.Equal(raw, []byte("null")) {
			if k == "public" {
				v.Public = raw
			} else {
				v.Private = raw
			}
		}
	}
	for k, raw := range m {
		if !known[k] {
			if v.Extra == nil {
				v.Extra = map[string]json.RawMessage{}
			}
			v.Extra[k] = raw
		}
	}
	*it = v
	return nil
}

// marshalSorted writes a map with its keys in order, so one item is always the same bytes.
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
		if err := json.Compact(&buf, m[k]); err != nil {
			return nil, fmt.Errorf("item %s: %w", k, err)
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// bound is what an item's seal binds its private part to: everything the device wrote but
// the private part itself.
func bound(it Item) []byte {
	var pub bytes.Buffer
	if len(it.Public) > 0 {
		if json.Compact(&pub, it.Public) != nil {
			pub.Reset()
			pub.Write(it.Public)
		}
	} else {
		pub.WriteString("null")
	}
	b, _ := json.Marshal([]any{it.Bucket, it.Type, it.GUID, it.TS, it.Deleted, json.RawMessage(pub.Bytes())})
	return b
}
