// Package synckosh holds what clients and servers share: the item, its timestamp, guids and the
// keys that seal an item's private part. The protocol is in SPEC.md; the client is in package
// client and the server in package server.
package synckosh

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Item is one synced thing. Public is plain: the server and a device that hasn't unlocked can
// read it. Private is plain on the device and sealed on the wire. Base is the sequence number of
// the version an edit was made from, 0 for none. Identity is for the server's index: a hash of
// what makes two items one thing, set by the device as it sends an item and never trusted by a
// device. Sequence and DeletedAt are set by the server. A field the item doesn't have is dropped
// when it is read.
type Item struct {
	Collection string
	Datatype   string
	GUID       string
	Timestamp  int64 // when the edit was made, the device's clock in milliseconds
	Deleted    bool
	Public     json.RawMessage
	Private    json.RawMessage
	Base       uint64 // the sequence number of the version this edit was made from
	Identity   string // what makes two items one thing, hashed: for the server's index
	Sequence   uint64 // the server's sequence number for this version
	DeletedAt  int64  // the server's clock, in milliseconds, when it stored the delete
}

func (it Item) MarshalJSON() ([]byte, error) {
	m := make(map[string]json.RawMessage, 10)
	set := func(k string, v any) {
		b, _ := json.Marshal(v)
		m[k] = b
	}
	set("collection", it.Collection)
	set("datatype", it.Datatype)
	set("guid", it.GUID)
	set("timestamp", it.Timestamp)
	set("deleted", it.Deleted)
	if len(it.Public) > 0 {
		m["public"] = it.Public
	}
	if len(it.Private) > 0 {
		m["private"] = it.Private
	}
	if it.Base != 0 {
		set("base", it.Base)
	}
	if it.Identity != "" {
		set("identity", it.Identity)
	}
	if it.Sequence != 0 {
		set("sequence", it.Sequence)
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
	fields := map[string]any{"collection": &v.Collection, "datatype": &v.Datatype, "guid": &v.GUID, "timestamp": &v.Timestamp,
		"deleted": &v.Deleted, "base": &v.Base, "identity": &v.Identity, "sequence": &v.Sequence, "deleted_at": &v.DeletedAt}
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
