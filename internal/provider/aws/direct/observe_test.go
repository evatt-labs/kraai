package direct

import (
	"slices"
	"sync"
)

// observing has c report each of want it observes, until the returned
// function stops it and returns them, sorted. Further calls run
// concurrently, so the codes are gathered under a lock.
func observing(c *Client, want []string) func() []string {
	var mu sync.Mutex
	seen := map[string]bool{}
	c.Observe = func(code string) {
		if slices.Contains(want, code) {
			mu.Lock()
			seen[code] = true
			mu.Unlock()
		}
	}
	return func() []string {
		c.Observe = nil
		mu.Lock()
		defer mu.Unlock()
		if len(seen) == 0 {
			return nil
		}
		return sortedKeys(seen)
	}
}
