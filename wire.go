package synckosh

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
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

// Page is one page of a pull. Time is the server's clock, and Cleanup the age after which it
// removes deleted items, both in milliseconds; Cleanup is 0 if it never does.
type Page struct {
	Store   string `json:"store"`
	Items   []Item `json:"items"`
	Next    uint64 `json:"next"`
	More    bool   `json:"more"`
	Time    int64  `json:"time"`
	Cleanup int64  `json:"cleanup,omitempty"`
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
	// RefusedIdentityTaken: the item is new to the server, and a live item of its collection has
	// its identity. The result's Current is that item, which the device checks and takes.
	RefusedIdentityTaken = "identity-taken"
)

// Limits bound what one request carries. The server and each device set their own; a zero
// field means the default in DefaultLimits. A server that refuses a push as too large answers
// with its limits, and the device splits the push to fit.
type Limits struct {
	ItemBytes int `json:"item_bytes"` // one item as JSON on the wire, sealed
	PushItems int `json:"push_items"` // items in one push
	PushBytes int `json:"push_bytes"` // one push request's body
	PageBytes int `json:"page_bytes"` // the items of one pull page, but always at least one item
}

// DefaultLimits are the limits a side uses unless it sets its own.
var DefaultLimits = Limits{ItemBytes: 1 << 20, PushItems: 1000, PushBytes: 16 << 20, PageBytes: 16 << 20}

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
	if l.PageBytes == 0 {
		l.PageBytes = d.PageBytes
	}
	return l
}

// LibraryCollection is the one name the protocol keeps for itself: the collection of the
// library's own items. It holds the data key's sealed copies, of datatype SealedKeyDatatype, and
// each collection's delete-before, of datatype DeleteBeforeDatatype, and nothing else. An app's
// collections are its own, and so is every datatype outside this collection.
const (
	LibraryCollection    = "_synckosh"
	SealedKeyDatatype    = "sealedkey"
	DeleteBeforeDatatype = "deletebefore"
)

// DeleteBeforeGUID is the guid of a collection's delete-before, the same on every device, so a
// collection has one.
func DeleteBeforeGUID(collection string) string { return "deletebefore:" + collection }

var (
	nameRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	guidRe     = regexp.MustCompile(`^[A-Za-z0-9._:~-]{1,128}$`)
	identityRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Valid checks an item's names and timestamp, and for the library's own items what the protocol
// reads in them. The server refuses an invalid item, a device won't put one, and a device takes
// none from the server.
func Valid(it Item) error {
	lib := it.Collection == LibraryCollection
	switch {
	case !lib && !nameRe.MatchString(it.Collection):
		return errors.New("collection: lowercase letters, digits, dot, dash or underscore, up to 64, starting with a letter or digit")
	case !nameRe.MatchString(it.Datatype):
		return errors.New("datatype: lowercase letters, digits, dot, dash or underscore, up to 64, starting with a letter or digit")
	case !guidRe.MatchString(it.GUID):
		return errors.New("guid: letters, digits and ._:~- up to 128")
	case it.Timestamp <= 0:
		return errors.New("timestamp: the time of the edit, in milliseconds")
	case it.Identity != "" && !identityRe.MatchString(it.Identity):
		return errors.New("identity: 64 lowercase hex digits, a SHA-256")
	case !lib:
		return nil
	}
	switch it.Datatype {
	case SealedKeyDatatype:
		return nil
	case DeleteBeforeDatatype:
		collection, _, err := Cutoff(it)
		switch {
		case err != nil:
			return err
		case it.GUID != DeleteBeforeGUID(collection):
			return errors.New("a delete-before's guid is DeleteBeforeGUID of its collection")
		case it.Deleted:
			return errors.New("a delete-before is never deleted")
		}
		return nil
	}
	return errors.New("the " + LibraryCollection + " collection holds only " + SealedKeyDatatype + " and " +
		DeleteBeforeDatatype + " items")
}

// Cutoff reads a delete-before: the collection it deletes from, and its cutoff, from its public
// part {"collection": name, "before": milliseconds}.
func Cutoff(it Item) (collection string, before int64, err error) {
	var p struct {
		Collection string `json:"collection"`
		Before     int64  `json:"before"`
	}
	if it.Collection != LibraryCollection || it.Datatype != DeleteBeforeDatatype {
		return "", 0, errors.New("not a delete-before")
	}
	if json.Unmarshal(it.Public, &p) != nil || !nameRe.MatchString(p.Collection) || p.Before <= 0 {
		return "", 0, errors.New(`a delete-before's public part is {"collection": an app's collection, "before": milliseconds}`)
	}
	return p.Collection, p.Before, nil
}

// IsDeleteBefore says whether an item is a delete-before.
func IsDeleteBefore(it Item) bool {
	return it.Collection == LibraryCollection && it.Datatype == DeleteBeforeDatatype
}

// ValidName checks a collection name, as a pull asks for one.
func ValidName(collection string) bool {
	return collection == LibraryCollection || nameRe.MatchString(collection)
}

// ValidDatatype checks a datatype name, as a pull asks for one.
func ValidDatatype(datatype string) bool { return nameRe.MatchString(datatype) }

// Filter limits a pull to some collections and some datatypes. An empty list allows all: the zero
// Filter is every item.
type Filter struct {
	Collections []string
	Datatypes   []string
}

// Match says whether an item passes the filter.
func (f Filter) Match(it Item) bool {
	return (len(f.Collections) == 0 || slices.Contains(f.Collections, it.Collection)) &&
		(len(f.Datatypes) == 0 || slices.Contains(f.Datatypes, it.Datatype))
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
