package client

import (
	"context"
	"errors"

	"github.com/sudiptadeb/synckosh"
)

// What a bucket holds, for a device about to set sync up: Look tells which, so the app asks for a
// new passphrase twice on a new bucket and for the existing one once on a keyed bucket.
type BucketState int

const (
	// BucketNew holds no item at all: Create makes its key.
	BucketNew BucketState = iota
	// BucketKeyed holds a live sealed copy of a key: Join opens it with a passphrase.
	BucketKeyed
	// BucketPlain holds items and no live sealed copy: its devices sync without a key, or every
	// copy was removed and only the devices that hold the key can use it.
	BucketPlain
)

// Look tells what a bucket holds, without a key or a passphrase.
func Look(ctx context.Context, c *Client) (BucketState, error) {
	p, err := c.Pull(ctx, synckosh.Filter{}, 0, 0)
	if err != nil {
		return 0, err
	}
	if len(p.Items) == 0 {
		return BucketNew, nil
	}
	f := synckosh.Filter{Collections: []string{synckosh.LibraryCollection}, Datatypes: []string{synckosh.SealedKeyDatatype}}
	var after uint64
	for {
		p, err := c.Pull(ctx, f, after, 0)
		if err != nil {
			return 0, err
		}
		for _, it := range p.Items {
			if !it.Deleted && synckosh.Valid(it) == nil {
				return BucketKeyed, nil
			}
		}
		if !p.More {
			return BucketPlain, nil
		}
		after = p.Next
	}
}

// ErrNoKey is the passphrase calls' answer on a replica opened without a key.
var ErrNoKey = errors.New("this device syncs without a key")

// Passphrase is one of the bucket's sealed copies of this device's key: a passphrase's, or a
// backup code's. Removing one is deleting its item, LibraryCollection and GUID, with Delete.
type Passphrase struct {
	GUID       string
	Label      string
	BackupCode bool
}

// copies returns the live sealed copies of this device's key that it holds.
func (r *Replica) copies() ([]synckosh.Item, error) {
	if r.key == nil {
		return nil, ErrNoKey
	}
	var out []synckosh.Item
	err := r.local.Each(synckosh.LibraryCollection, func(e Entry) error {
		if e.Locked || e.Item.Deleted || e.Item.Datatype != synckosh.SealedKeyDatatype {
			return nil
		}
		if c, err := synckosh.ReadCopy(e.Item); err == nil && c.KeyID == r.key.ID {
			out = append(out, e.Item)
		}
		return nil
	})
	return out, err
}

// Passphrases lists the bucket's passphrases and backup codes for this device's key, as of its
// last Sync.
func (r *Replica) Passphrases() ([]Passphrase, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	its, err := r.copies()
	if err != nil {
		return nil, err
	}
	out := make([]Passphrase, 0, len(its))
	for _, it := range its {
		c, _ := synckosh.ReadCopy(it)
		out = append(out, Passphrase{GUID: it.GUID, Label: c.Label, BackupCode: c.BackupCode})
	}
	return out, nil
}

// CheckPassphrase returns nil if passphrase opens one of the bucket's copies of this device's key,
// as of its last Sync, and synckosh.ErrPassphrase if none. It reads no network, and takes the
// passphrase's work once per copy.
func (r *Replica) CheckPassphrase(passphrase string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	its, err := r.copies()
	if err != nil {
		return err
	}
	for _, it := range its {
		if r.key.Unlocks(it, passphrase) {
			return nil
		}
	}
	return synckosh.ErrPassphrase
}

// SetPassphrase makes passphrase the bucket's passphrase, in one write: a new sealed copy, and
// every other passphrase's copy of this device's key deleted. Backup codes stay. The data key
// doesn't change, and the next Sync sends the change.
func (r *Replica) SetPassphrase(passphrase, label string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	its, err := r.copies()
	if err != nil {
		return err
	}
	fresh, err := r.key.SealedCopy(passphrase, label)
	if err != nil {
		return err
	}
	out := []synckosh.Item{fresh}
	for _, it := range its {
		if c, _ := synckosh.ReadCopy(it); !c.BackupCode {
			it.Deleted, it.Sequence, it.DeletedAt = true, 0, 0
			out = append(out, it)
		}
	}
	return r.write(out...)
}

// AddBackupCode makes a backup code, a random passphrase for a person to write down or scan, with
// its own sealed copy, which the next Sync sends. It returns the code, to show once.
func (r *Replica) AddBackupCode(label string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.key == nil {
		return "", ErrNoKey
	}
	code, copy, err := r.key.BackupCode(label)
	if err != nil {
		return "", err
	}
	return code, r.write(copy)
}
