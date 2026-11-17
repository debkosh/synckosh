package synckosh

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// The keys bucket holds one sealed copy of the data key per passphrase. Its items are sealed
// by their passphrase, not by the data key.
const (
	KeysBucket    = "keys"
	SealedKeyType = "sealed-key"
)

var (
	ErrPassphrase = errors.New("wrong passphrase")
	ErrUnknownKey = errors.New("sealed with a key this device doesn't have")
)

// DataKey seals the private part of every item in an account.
type DataKey struct {
	ID  string
	key [chacha20poly1305.KeySize]byte
}

// NewDataKey makes the key for a new account.
func NewDataKey() (*DataKey, error) {
	k := &DataKey{}
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	k.ID = "k_" + base64.RawURLEncoding.EncodeToString(id)
	if _, err := rand.Read(k.key[:]); err != nil {
		return nil, err
	}
	return k, nil
}

// Bytes returns the key, for the device to keep in its keychain.
func (k *DataKey) Bytes() []byte { return append([]byte(nil), k.key[:]...) }

// DataKeyFrom rebuilds a key the device kept.
func DataKeyFrom(id string, b []byte) (*DataKey, error) {
	k := &DataKey{ID: id}
	if len(b) != len(k.key) {
		return nil, fmt.Errorf("data key is %d bytes, want %d", len(b), len(k.key))
	}
	copy(k.key[:], b)
	return k, nil
}

type box struct {
	Key string `json:"key,omitempty"`
	Box string `json:"box"`
}

func seal(key []byte, plain, ad []byte) (string, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, ad)), nil
}

func open(key []byte, sealed string, ad []byte) ([]byte, error) {
	b, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	if len(b) < aead.NonceSize() {
		return nil, errors.New("sealed data too short")
	}
	return aead.Open(nil, b[:aead.NonceSize()], b[aead.NonceSize():], ad)
}

// Seal returns the item with its private part sealed, bound to the rest of the item, so the
// server can't change the public part or move the private part to another item unnoticed.
func (k *DataKey) Seal(it Item) (Item, error) {
	if len(it.Private) == 0 || it.Bucket == KeysBucket {
		return it, nil
	}
	s, err := seal(k.key[:], it.Private, bound(it))
	if err != nil {
		return Item{}, err
	}
	it.Private, _ = json.Marshal(box{Key: k.ID, Box: s})
	return it, nil
}

// Open returns the item with its private part opened.
func (k *DataKey) Open(it Item) (Item, error) {
	if len(it.Private) == 0 || it.Bucket == KeysBucket {
		return it, nil
	}
	var b box
	if err := json.Unmarshal(it.Private, &b); err != nil || b.Box == "" {
		return Item{}, fmt.Errorf("%s %s: private part is not sealed", it.Bucket, it.GUID)
	}
	if b.Key != k.ID {
		return Item{}, fmt.Errorf("%s %s: %w (%s)", it.Bucket, it.GUID, ErrUnknownKey, b.Key)
	}
	plain, err := open(k.key[:], b.Box, bound(it))
	if err != nil {
		return Item{}, fmt.Errorf("%s %s: seal doesn't open: %w", it.Bucket, it.GUID, err)
	}
	it.Private = plain
	return it, nil
}

// kdf is the public part of a sealed copy: what a new device needs to turn the passphrase
// into the key that opens it.
type kdf struct {
	Label string `json:"label"`
	Key   string `json:"key"`
	KDF   string `json:"kdf"`
	Time  uint32 `json:"t"`
	MemKB uint32 `json:"m"`
	Par   uint8  `json:"p"`
	Salt  string `json:"salt"`
}

// Argon2id settings for new sealed copies: RFC 9106's second recommended option.
const (
	argonTime  = 3
	argonMemKB = 64 * 1024
	argonPar   = 4
)

// SealedCopy seals the data key with a passphrase, as an item for the keys bucket. An account
// can have several, one per passphrase. The caller gives it a guid and stores it as any item.
func (k *DataKey) SealedCopy(passphrase, label string) (Item, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return Item{}, err
	}
	p := kdf{Label: label, Key: k.ID, KDF: "argon2id", Time: argonTime, MemKB: argonMemKB, Par: argonPar,
		Salt: base64.RawStdEncoding.EncodeToString(salt)}
	wrap := argon2.IDKey([]byte(passphrase), salt, p.Time, p.MemKB, p.Par, chacha20poly1305.KeySize)
	s, err := seal(wrap, k.key[:], []byte(k.ID))
	if err != nil {
		return Item{}, err
	}
	pub, _ := json.Marshal(p)
	priv, _ := json.Marshal(box{Box: s})
	return Item{Bucket: KeysBucket, Type: SealedKeyType, Public: pub, Private: priv}, nil
}

// OpenSealedCopy opens a sealed copy with its passphrase.
func OpenSealedCopy(it Item, passphrase string) (*DataKey, error) {
	var p kdf
	if err := json.Unmarshal(it.Public, &p); err != nil {
		return nil, fmt.Errorf("sealed key: %w", err)
	}
	if p.KDF != "argon2id" {
		return nil, fmt.Errorf("sealed key: kdf %q", p.KDF)
	}
	salt, err := base64.RawStdEncoding.DecodeString(p.Salt)
	if err != nil {
		return nil, fmt.Errorf("sealed key salt: %w", err)
	}
	var b box
	if err := json.Unmarshal(it.Private, &b); err != nil {
		return nil, fmt.Errorf("sealed key: %w", err)
	}
	wrap := argon2.IDKey([]byte(passphrase), salt, p.Time, p.MemKB, p.Par, chacha20poly1305.KeySize)
	key, err := open(wrap, b.Box, []byte(p.Key))
	if err != nil {
		return nil, ErrPassphrase
	}
	return DataKeyFrom(p.Key, key)
}
