package synckosh

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
)

// What a server answers, the same for both ends.

// Result is the server's answer for one pushed item: stored with its sequence number, or not
// stored because the server holds a version with a later timestamp, returned in Current, or
// refused with a Code (one of Refused...) and an Error to read.
type Result struct {
	OK       bool   `json:"ok"`
	Sequence uint64 `json:"sequence,omitempty"`
	Current  *Item  `json:"current,omitempty"`
	Code     string `json:"code,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Pushed is the server's answer to a push: one result per item, in the order sent.
type Pushed struct {
	Store   string   `json:"store"`
	Time    int64    `json:"time"` // the server's clock, in milliseconds
	Results []Result `json:"results"`
}

// Page is one page of a pull.
type Page struct {
	Store string `json:"store"`
	Items []Item `json:"items"`
	Next  uint64 `json:"next"`
	More  bool   `json:"more"`
}

// Why the server refused a pushed item, in a push result's "code".
const (
	// RefusedTimestampAhead: the item's timestamp is more than MaxAhead ahead of the server's
	// clock, so the device's clock is wrong.
	RefusedTimestampAhead = "timestamp-ahead"
	// RefusedTooBig: the item is over the server's item limit.
	RefusedTooBig = "too-big"
	// RefusedInvalid: a field is malformed, such as a collection name with capitals.
	RefusedInvalid = "invalid"
)

// Limits bound what one request carries. The server and each device set their own; a zero
// field means the default in DefaultLimits.
type Limits struct {
	ItemBytes int // one item as JSON on the wire, sealed
	PushItems int // items in one push
	PushBytes int // one push request's body
}

// DefaultLimits are the limits a side uses unless it sets its own.
var DefaultLimits = Limits{ItemBytes: 1 << 20, PushItems: 1000, PushBytes: 16 << 20}

// OrDefaults returns the limits with every zero field set to its default.
func (l Limits) OrDefaults() Limits {
	d := DefaultLimits
	if l.ItemBytes == 0 {
		l.ItemBytes = d.ItemBytes
	}
	if l.PushItems == 0 {
		l.PushItems = d.PushItems
	}
	if l.PushBytes == 0 {
		l.PushBytes = d.PushBytes
	}
	return l
}

// Names the library owns start with "_", which an app's names can't. The sealed copies of the
// data key are items of datatype SealedKeyDatatype in KeysCollection, and nothing else is.
const (
	KeysCollection    = "_sealedkeys"
	SealedKeyDatatype = "_sealedkey"
)

var (
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	guidRe = regexp.MustCompile(`^[A-Za-z0-9.:~-][A-Za-z0-9._:~-]{0,127}$`)
)

// Valid checks an item's names and timestamp. The server refuses an invalid item, a device
// won't put one, and a device takes none from the server.
func Valid(it Item) error {
	switch {
	case it.Collection == KeysCollection || it.Datatype == SealedKeyDatatype:
		if it.Collection != KeysCollection || it.Datatype != SealedKeyDatatype {
			return errors.New("the " + KeysCollection + " collection holds only " + SealedKeyDatatype + " items")
		}
	case !nameRe.MatchString(it.Collection):
		return errors.New("collection: lowercase letters, digits, dot, dash or underscore, up to 64, not starting with dot, dash or underscore")
	case !nameRe.MatchString(it.Datatype):
		return errors.New("datatype: lowercase letters, digits, dot, dash or underscore, up to 64, not starting with dot, dash or underscore")
	}
	switch {
	case !guidRe.MatchString(it.GUID):
		return errors.New("guid: letters, digits and ._:~- up to 128, not starting with underscore")
	case it.Timestamp <= 0:
		return errors.New("timestamp: the time of the edit, in milliseconds")
	}
	return nil
}

// ValidName checks a collection name, as a pull asks for one.
func ValidName(collection string) bool {
	return collection == KeysCollection || nameRe.MatchString(collection)
}

// Marshal writes JSON as the wire carries it: without Go's escaping of <, >, & and U+2028, so a
// public or private part keeps the bytes it was written with. json.Marshal escapes them.
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
