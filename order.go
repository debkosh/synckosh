package synckosh

import "strings"

// Order keys sort items in a list. An item keeps its own order key in its data, so adding or
// moving one item changes only that item. Two devices may give two items the same key; a list
// shows its items by order key, then by guid, which sorts by when each was made. Any device may
// later give them keys of their own; that is an ordinary edit.

const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Between returns an order key that sorts after a and before b. An empty a means the start of
// the list, an empty b its end. The keys it makes never end in "0", so there is always room
// between two of them. It panics if a doesn't sort before b.
func Between(a, b string) string {
	if b != "" && a >= b {
		panic("synckosh.Between: " + a + " is not before " + b)
	}
	var out []byte
	bounded := b != "" // still at b's prefix, so b limits the next digit
	for i := 0; ; i++ {
		lo := 0
		if i < len(a) {
			lo = strings.IndexByte(digits, a[i])
		}
		hi := len(digits)
		if bounded {
			hi = 0
			if i < len(b) {
				hi = strings.IndexByte(digits, b[i])
			}
		}
		if hi-lo > 1 {
			return string(append(out, digits[(lo+hi)/2]))
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
