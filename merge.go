package synckosh

// A Merger decides what an item becomes when this device changed it and another device
// changed it too. It gets the local item and the remote one for the same guid, and returns
// the result. It must be pure: the same answer on every device, whichever item it gets first.
//
// If the result says the same as remote, the device takes remote. If it says the same as local
// and local is newer, local is sent as it is. Anything else is a new edit: it gets a new
// timestamp, newer than both, and is sent.
//
// A merger that reads Data must keep the fields it doesn't know, so an older device never
// drops what a newer one wrote.
type Merger func(local, remote Item) Item

// Newest is the default: the item with the newer timestamp wins, whole.
func Newest(local, remote Item) Item {
	if remote.TS.After(local.TS) {
		return remote
	}
	return local
}
