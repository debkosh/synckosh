package synckosh

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
	"golang.org/x/crypto/chacha20poly1305"
)

// KeysType is the type of the one item that holds the sync's key, sealed with the passphrase,
// as keykosh seals its identity. It is the only item not sealed with that key.
const (
	KeysType = "sync.keys"
	KeysGUID = "keys"
)

const sealPrefix = "seal1:"

// ErrPassphrase is returned when the passphrase doesn't open the keys item.
var ErrPassphrase = errors.New("wrong passphrase")

// Keys seals each item's data. The server sees the envelope, never the data, and can't move
// sealed data to another item or another timestamp: the envelope is bound into the seal.
type Keys struct {
	key [chacha20poly1305.KeySize]byte
}

// NewKeys makes the key for a new sync.
func NewKeys() (*Keys, error) {
	k := &Keys{}
	if _, err := rand.Read(k.key[:]); err != nil {
		return nil, err
	}
	return k, nil
}

// Lock seals the key with the passphrase into the keys item, with age's scrypt recipient.
func (k *Keys) Lock(passphrase string, ts TS) (Item, error) {
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return Item{}, err
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		return Item{}, err
	}
	if _, err := w.Write(k.key[:]); err != nil {
		return Item{}, err
	}
	if err := w.Close(); err != nil {
		return Item{}, err
	}
	data, _ := json.Marshal(base64.StdEncoding.EncodeToString(buf.Bytes()))
	return Item{Type: KeysType, GUID: KeysGUID, TS: ts, Data: data}, nil
}

// Unlock opens the keys item with the passphrase.
func Unlock(it Item, passphrase string) (*Keys, error) {
	var s string
	if err := json.Unmarshal(it.Data, &s); err != nil {
		return nil, fmt.Errorf("keys item: %w", err)
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("keys item: %w", err)
	}
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, err
	}
	r, err := age.Decrypt(bytes.NewReader(b), id)
	if err != nil {
		var wrong *age.NoIdentityMatchError
		if errors.As(err, &wrong) {
			return nil, ErrPassphrase
		}
		return nil, err
	}
	key, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	k := &Keys{}
	if len(key) != len(k.key) {
		return nil, fmt.Errorf("keys item: key is %d bytes", len(key))
	}
	copy(k.key[:], key)
	return k, nil
}

// bound is what the seal binds the data to.
func bound(it Item) []byte {
	ts, _ := it.TS.MarshalJSON()
	return []byte(strings.Join([]string{it.Type, it.GUID, string(ts), fmt.Sprint(it.Deleted)}, "\x00"))
}

// Seal returns the item with its data sealed. A deleted item has no data to seal.
func (k *Keys) Seal(it Item) (Item, error) {
	if len(it.Data) == 0 || it.Type == KeysType {
		return it, nil
	}
	aead, err := chacha20poly1305.NewX(k.key[:])
	if err != nil {
		return Item{}, err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(it.Data)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return Item{}, err
	}
	sealed := aead.Seal(nonce, nonce, it.Data, bound(it))
	it.Data, _ = json.Marshal(sealPrefix + base64.RawStdEncoding.EncodeToString(sealed))
	return it, nil
}

// Open returns the item with its data opened.
func (k *Keys) Open(it Item) (Item, error) {
	if len(it.Data) == 0 || it.Type == KeysType {
		return it, nil
	}
	var s string
	if err := json.Unmarshal(it.Data, &s); err != nil || !strings.HasPrefix(s, sealPrefix) {
		return Item{}, fmt.Errorf("%s %s: data is not sealed", it.Type, it.GUID)
	}
	b, err := base64.RawStdEncoding.DecodeString(s[len(sealPrefix):])
	if err != nil {
		return Item{}, fmt.Errorf("%s %s: %w", it.Type, it.GUID, err)
	}
	aead, err := chacha20poly1305.NewX(k.key[:])
	if err != nil {
		return Item{}, err
	}
	if len(b) < aead.NonceSize() {
		return Item{}, fmt.Errorf("%s %s: sealed data too short", it.Type, it.GUID)
	}
	data, err := aead.Open(nil, b[:aead.NonceSize()], b[aead.NonceSize():], bound(it))
	if err != nil {
		return Item{}, fmt.Errorf("%s %s: seal doesn't open: %w", it.Type, it.GUID, err)
	}
	it.Data = data
	return it, nil
}
