package synckosh

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

var (
	ErrPassphrase = errors.New("wrong passphrase")
	ErrUnknownKey = errors.New("sealed with a key this device doesn't have")
	ErrNotSealed  = errors.New("not sealed")
)

// DataKey seals every item in a bucket.
type DataKey struct {
	ID  string
	key [chacha20poly1305.KeySize]byte
}

// NewDataKey makes the key for a new bucket.
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

// Bytes returns the key, only for the device to keep in its own key store, such as the OS
// keychain. It is never shown, exported or put in a QR code: a person only ever sees a
// passphrase that opens a sealed copy.
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

func seal(key []byte, plain, ad []byte) (string, error) {
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return sealWith(key, nonce, plain, ad)
}

func sealWith(key, nonce, plain, ad []byte) (string, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(aead.Seal(append([]byte(nil), nonce...), nonce, plain, ad)), nil
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

// sealedPart is an item's private part on the wire.
type sealedPart struct {
	Key    string `json:"key"`
	Box    string `json:"box"`
	Public bool   `json:"public,omitempty"` // the box holds a copy of the public part too
}

// additional is what an item's seal binds outside the box: every field the device wrote, one
// per line. Valid names hold no newline, so the text can't be read two ways.
func additional(it Item, public bool) []byte {
	base := ""
	if it.Base != 0 {
		base = strconv.FormatUint(it.Base, 10)
	}
	return []byte(strings.Join([]string{it.Collection, it.Datatype, it.GUID, strconv.FormatInt(it.Timestamp, 10),
		bit(it.Deleted), base, bit(public)}, "\n"))
}

func bit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// part is a part as it goes in a box: its JSON, or null when there is none.
func part(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("null"), nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("not JSON")
	}
	return raw, nil
}

func unpart(raw []byte) json.RawMessage {
	if bytes.Equal(raw, []byte("null")) {
		return nil
	}
	return raw
}

// Seal returns the item with its private part sealed, an empty one included, bound to every
// other field the device wrote, so the server can't alter, forge or move it unnoticed. With
// public, the box also holds a copy of the public part, and a device that opens it uses that
// copy, so the server can't change the public part either. The library's own items, those of
// LibraryCollection, are always sealed with their public part.
func (k *DataKey) Seal(it Item, public bool) (Item, error) {
	public = public || it.Collection == LibraryCollection
	priv, err := part(it.Private)
	if err != nil {
		return Item{}, fmt.Errorf("%s %s: private part: %w", it.Collection, it.GUID, err)
	}
	plain := priv
	if public {
		pub, err := part(it.Public)
		if err != nil {
			return Item{}, fmt.Errorf("%s %s: public part: %w", it.Collection, it.GUID, err)
		}
		plain = bytes.Join([][]byte{[]byte("["), pub, []byte(","), priv, []byte("]")}, nil)
	}
	s, err := seal(k.key[:], plain, additional(it, public))
	if err != nil {
		return Item{}, err
	}
	it.Private, err = json.Marshal(sealedPart{Key: k.ID, Box: s, Public: public})
	return it, err
}

// Open returns the item with its private part opened, and its public part taken from the box
// when the box holds one. It returns ErrNotSealed for an item that isn't sealed, ErrUnknownKey
// for one sealed under another key, and an error for a seal that doesn't open, which means the
// item was changed after it was sealed.
func (k *DataKey) Open(it Item) (Item, error) {
	var p sealedPart
	if len(it.Private) == 0 || json.Unmarshal(it.Private, &p) != nil || p.Key == "" || p.Box == "" {
		return Item{}, fmt.Errorf("%s %s: %w", it.Collection, it.GUID, ErrNotSealed)
	}
	if p.Key != k.ID {
		return Item{}, fmt.Errorf("%s %s: %w (%s)", it.Collection, it.GUID, ErrUnknownKey, p.Key)
	}
	if !p.Public && it.Collection == LibraryCollection {
		return Item{}, fmt.Errorf("%s %s: the library's items are sealed with their public part", it.Collection, it.GUID)
	}
	plain, err := open(k.key[:], p.Box, additional(it, p.Public))
	if err != nil {
		return Item{}, fmt.Errorf("%s %s: seal doesn't open: %w", it.Collection, it.GUID, err)
	}
	if !p.Public {
		it.Private = unpart(plain)
		return it, nil
	}
	var both []json.RawMessage
	if err := json.Unmarshal(plain, &both); err != nil || len(both) != 2 {
		return Item{}, fmt.Errorf("%s %s: box doesn't hold the public and private parts", it.Collection, it.GUID)
	}
	it.Public, it.Private = unpart(both[0]), unpart(both[1])
	return it, nil
}

// sealedKey is the public part of a sealed copy: what a device that hasn't unlocked needs to
// turn the passphrase into the key that opens box, which holds the data key.
type sealedKey struct {
	Label string `json:"label"`
	Code  bool   `json:"code,omitempty"` // a backup code's copy, not a passphrase's
	Key   string `json:"key"`
	KDF   string `json:"kdf"`
	Time  uint32 `json:"t"`
	MemKB uint32 `json:"m"`
	Par   uint32 `json:"p"`
	Salt  string `json:"salt"`
	Box   string `json:"box"`
}

// Argon2id settings for new sealed copies: RFC 9106's second recommended option.
const (
	argonTime  = 3
	argonMemKB = 64 * 1024
	argonPar   = 4
)

// check refuses settings no honest writer makes, before a joining device spends time or memory
// on them: it reads them before it can verify anything.
func (p sealedKey) check() ([]byte, error) {
	switch {
	case p.KDF != "argon2id":
		return nil, fmt.Errorf("sealed key: kdf %q, want argon2id", p.KDF)
	case p.Time < 1 || p.Time > 10:
		return nil, fmt.Errorf("sealed key: t %d, want 1 to 10", p.Time)
	case p.Par < 1 || p.Par > 16:
		return nil, fmt.Errorf("sealed key: p %d, want 1 to 16", p.Par)
	case p.MemKB < 8*p.Par || p.MemKB > 256*1024:
		return nil, fmt.Errorf("sealed key: m %d KiB, want 8p to 262144", p.MemKB)
	}
	salt, err := base64.RawStdEncoding.DecodeString(p.Salt)
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return nil, errors.New("sealed key: salt, want 8 to 64 bytes")
	}
	return salt, nil
}

func (p sealedKey) wrapKey(passphrase string, salt []byte) []byte {
	return argon2.IDKey([]byte(passphrase), salt, p.Time, p.MemKB, uint8(p.Par), chacha20poly1305.KeySize)
}

// SealedCopy seals the data key with a passphrase, as a new item for LibraryCollection with its
// own guid. A bucket can have several, one per passphrase. The device puts it as any item: it is
// sealed with the data key too, so the server can't alter or forge one.
func (k *DataKey) SealedCopy(passphrase, label string) (Item, error) {
	return k.sealedCopy(passphrase, label, false)
}

func (k *DataKey) sealedCopy(passphrase, label string, code bool) (Item, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return Item{}, err
	}
	p := sealedKey{Label: label, Code: code, Key: k.ID, KDF: "argon2id", Time: argonTime, MemKB: argonMemKB, Par: argonPar,
		Salt: base64.RawStdEncoding.EncodeToString(salt)}
	var err error
	if p.Box, err = seal(p.wrapKey(passphrase, salt), k.key[:], []byte(k.ID)); err != nil {
		return Item{}, err
	}
	pub, err := json.Marshal(p)
	return Item{Collection: LibraryCollection, Datatype: SealedKeyDatatype, GUID: NewGUID(), Public: pub}, err
}

// BackupCode makes a random passphrase of 128 bits, for a person to write down or scan, with
// its sealed copy. Deleting the copy removes the code without changing the data key.
func (k *DataKey) BackupCode(label string) (code string, copy Item, err error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", Item{}, err
	}
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	var groups []string
	for len(s) > 4 {
		groups, s = append(groups, s[:4]), s[4:]
	}
	code = strings.Join(append(groups, s), "-")
	copy, err = k.sealedCopy(code, label, true)
	return code, copy, err
}

// CopyInfo is what a sealed copy says of itself, readable without its passphrase.
type CopyInfo struct {
	Label      string
	KeyID      string // the id of the data key it holds
	BackupCode bool   // a backup code's copy, not a passphrase's
}

// ReadCopy reads a sealed copy's label, key id and kind.
func ReadCopy(it Item) (CopyInfo, error) {
	var p sealedKey
	if it.Collection != LibraryCollection || it.Datatype != SealedKeyDatatype || json.Unmarshal(it.Public, &p) != nil {
		return CopyInfo{}, fmt.Errorf("%s %s: not a sealed copy", it.Collection, it.GUID)
	}
	return CopyInfo{Label: p.Label, KeyID: p.Key, BackupCode: p.Code}, nil
}

// Unlocks says whether a passphrase opens a sealed copy, as a device holds it, to this key.
func (k *DataKey) Unlocks(it Item, passphrase string) bool {
	var p sealedKey
	if it.Deleted || json.Unmarshal(it.Public, &p) != nil || p.Key != k.ID {
		return false
	}
	salt, err := p.check()
	if err != nil {
		return false
	}
	raw, err := open(p.wrapKey(passphrase, salt), p.Box, []byte(p.Key))
	return err == nil && subtle.ConstantTimeCompare(raw, k.key[:]) == 1
}

// OpenSealedCopy opens a sealed copy, as the server sent it, with its passphrase. The key it
// opens must also open the item's own seal, which proves a holder of that key wrote the copy.
func OpenSealedCopy(it Item, passphrase string) (*DataKey, error) {
	if it.Collection != LibraryCollection || it.Datatype != SealedKeyDatatype {
		return nil, fmt.Errorf("%s %s: not a sealed copy", it.Collection, it.GUID)
	}
	if it.Deleted {
		return nil, fmt.Errorf("%s: the sealed copy was removed", it.GUID)
	}
	var p sealedKey
	if err := json.Unmarshal(it.Public, &p); err != nil {
		return nil, fmt.Errorf("sealed key: %w", err)
	}
	salt, err := p.check()
	if err != nil {
		return nil, err
	}
	raw, err := open(p.wrapKey(passphrase, salt), p.Box, []byte(p.Key))
	if err != nil {
		return nil, ErrPassphrase
	}
	key, err := DataKeyFrom(p.Key, raw)
	if err != nil {
		return nil, err
	}
	if _, err := key.Open(it); err != nil {
		return nil, fmt.Errorf("sealed key: not sealed by its own key: %w", err)
	}
	return key, nil
}

// Identity hashes what makes two items one thing into an item's identity, for the server's
// index: HMAC-SHA256 under a key made from the data key, so the server learns only that two items
// match, never what they are. A bucket without a key is plain to the server, and there it is
// SHA-256.
func Identity(key *DataKey, what []byte) string {
	if key == nil {
		sum := sha256.Sum256(what)
		return hex.EncodeToString(sum[:])
	}
	sub := hmac.New(sha256.New, key.key[:])
	sub.Write([]byte("synckosh identity"))
	h := hmac.New(sha256.New, sub.Sum(nil))
	h.Write(what)
	return hex.EncodeToString(h.Sum(nil))
}

// SameContent is the identity an item has unless its app says otherwise: its public and private
// parts, in canonical JSON, as the array [public, private], null for a part it lacks. Two items
// that hold the same are one thing.
func SameContent(it Item) []byte {
	pub, priv := it.Public, it.Private
	if len(pub) == 0 {
		pub = json.RawMessage("null")
	}
	if len(priv) == 0 {
		priv = json.RawMessage("null")
	}
	b, err := Canonical(bytes.Join([][]byte{[]byte("["), pub, []byte(","), priv, []byte("]")}, nil))
	if err != nil {
		return nil
	}
	return b
}
