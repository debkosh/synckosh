package synckosh

import (
	"sync"
	"time"
)

// MaxAhead is how far ahead of this device's clock a timestamp it reads may be and still move
// its clock forward. A device with its clock set a year ahead can't drag every other device's
// clock along, and the server refuses items that far ahead. An hour is generous for devices
// that keep network time, and bounds what one wrong clock can do: a delete-before stamped ahead
// deletes other devices' newer items for good.
const MaxAhead = time.Hour

// Clock makes timestamps, in milliseconds, that are always after every one this device has
// made or read, so an edit made after seeing another always wins over it, whatever the wall
// clock says.
type Clock struct {
	mu   sync.Mutex
	last int64
	now  func() time.Time
}

func NewClock() *Clock {
	return &Clock{now: time.Now}
}

// Now returns a timestamp after every one this clock has made or observed: the wall clock, or
// one millisecond past the newest it has seen when the wall clock is behind it.
func (c *Clock) Now() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = max(c.now().UnixMilli(), c.last+1)
	return c.last
}

// Observe moves the clock up to a timestamp read from another device, unless that timestamp
// is more than MaxAhead ahead of this device's clock.
func (c *Clock) Observe(t int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t <= c.now().Add(MaxAhead).UnixMilli() {
		c.last = max(c.last, t)
	}
}

// Reset moves the clock back to the server's time t, when the server refused a timestamp as
// too far ahead. Once this device's wall clock is right, its next timestamps are accepted.
func (c *Clock) Reset(t int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = min(c.last, t)
}

// Last returns the newest timestamp this clock has made or observed, for the device to keep
// so the clock never goes back after a restart.
func (c *Clock) Last() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}
