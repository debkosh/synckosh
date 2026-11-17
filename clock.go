package synckosh

import (
	"sync"
	"time"
)

// MaxAhead is how far ahead of this device's clock a timestamp it reads may be and still move
// its clock forward. A device with its clock set a year ahead can't drag every other device's
// clock along, and the server refuses items that far ahead.
const MaxAhead = 24 * time.Hour

// Clock makes timestamps that never go below one this device has written or read, so an edit
// made after seeing another always sorts after it, whatever the wall clock says.
type Clock struct {
	mu   sync.Mutex
	dev  string
	last TS
	now  func() time.Time
}

func NewClock(device string) *Clock {
	return &Clock{dev: device, now: time.Now}
}

// Now returns a timestamp newer than every one this clock has made or observed.
func (c *Clock) Now() TS {
	c.mu.Lock()
	defer c.mu.Unlock()
	ms := c.now().UnixMilli()
	if ms > c.last.MS {
		c.last = TS{MS: ms, Dev: c.dev}
	} else {
		c.last = TS{MS: c.last.MS, N: c.last.N + 1, Dev: c.dev}
	}
	return c.last
}

// Observe moves the clock past a timestamp read from another device.
func (c *Clock) Observe(t TS) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.MS > c.now().Add(MaxAhead).UnixMilli() {
		return
	}
	if t.MS > c.last.MS || (t.MS == c.last.MS && t.N > c.last.N) {
		c.last = TS{MS: t.MS, N: t.N, Dev: c.dev}
	}
}
