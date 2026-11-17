package synckosh

import (
	"errors"
	"fmt"
	"strings"
)

// Order keys sort items in a list. An item keeps its own order key in its data, so adding or
// moving one item changes only that item. Two devices may give two items the same key; a list
// shows its items by order key, then by guid, which sorts by when each was made. Any device may
// later give them keys of their own; that is an ordinary edit.

// digits is the order-key alphabet, in the order it sorts as bytes.
const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// ErrNoRoom is Between's answer when no key sorts strictly between the two it was given: they are
// equal or out of order, or b is a followed only by "0".
var ErrNoRoom = errors.New("no order key sorts between these two")

// Between returns an order key that sorts after a and before b. An empty a means the start of
// the list, an empty b its end. The keys it makes never end in "0", so there is always room
// between two of them; keys from elsewhere may not leave room, and then it returns ErrNoRoom.
// The app then gives one of the items a new key. Keys come from other devices, so a key with a
// character outside 0-9, A-Z and a-z is an error too.
func Between(a, b string) (string, error) {
	for _, k := range []string{a, b} {
		if i := strings.IndexFunc(k, func(r rune) bool { return !strings.ContainsRune(digits, r) }); i >= 0 {
			return "", fmt.Errorf("order key %q: %q is not an order-key digit", k, k[i:i+1])
		}
	}
	if b != "" && (a >= b || strings.HasPrefix(b, a) && strings.Trim(b[len(a):], "0") == "") {
		return "", ErrNoRoom
	}
	// While bounded, the key so far is b's prefix, so b limits the next digit. Once a digit is
	// below b's, the key is before b whatever follows. With room checked above, the loop ends:
	// bounded, it meets a digit with room below b's before b ends; unbounded, it meets one below
	// "z" within a, or just after it.
	var out []byte
	bounded := b != ""
	for i := 0; ; i++ {
		lo := 0
		if i < len(a) {
			lo = strings.IndexByte(digits, a[i])
		}
		hi := len(digits)
		if bounded {
			hi = strings.IndexByte(digits, b[i])
		}
		if hi-lo > 1 {
			return string(append(out, digits[(lo+hi)/2])), nil
		}
		out = append(out, digits[lo])
		if hi > lo {
			bounded = false
		}
	}
}

// Less is how a list sorts: by order key, then by guid.
func Less(orderA, guidA, orderB, guidB string) bool {
	if orderA != orderB {
		return orderA < orderB
	}
	return guidA < guidB
}
