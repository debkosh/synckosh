package synckosh

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

var guids struct {
	mu    sync.Mutex
	ms    int64
	count uint16
}

// NewGUID returns a new item's guid: a UUIDv7 (RFC 9562), which sorts by when it was made.
// Guids made on one device in the same millisecond still sort in the order they were made:
// the 12 bits after the time count up, as RFC 9562 section 6.2 method 1 allows.
func NewGUID() string {
	guids.mu.Lock()
	ms := time.Now().UnixMilli()
	if ms <= guids.ms {
		guids.count++
		if guids.count > 0xfff {
			guids.ms++
			guids.count = 0
		}
		ms = guids.ms
	} else {
		guids.ms, guids.count = ms, 0
	}
	count := guids.count
	guids.mu.Unlock()

	var u [16]byte
	if _, err := rand.Read(u[6:]); err != nil {
		panic(err)
	}
	u[0], u[1], u[2], u[3], u[4], u[5] = byte(ms>>40), byte(ms>>32), byte(ms>>24), byte(ms>>16), byte(ms>>8), byte(ms)
	u[6] = 0x70 | byte(count>>8) // version 7
	u[7] = byte(count)
	u[8] = 0x80 | u[8]&0x3f // variant 10

	var s [36]byte
	hex.Encode(s[0:8], u[0:4])
	hex.Encode(s[9:13], u[4:6])
	hex.Encode(s[14:18], u[6:8])
	hex.Encode(s[19:23], u[8:10])
	hex.Encode(s[24:], u[10:])
	s[8], s[13], s[18], s[23] = '-', '-', '-', '-'
	return string(s[:])
}
