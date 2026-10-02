package direct

import (
	"context"
	"fmt"
	"time"
)

// defaultWait is the ceiling for a mutation's retries and for a read to
// show it, when neither the client nor the type sets one.
const defaultWait = 2 * time.Minute

// ceiling is how long r's mutations may be retried and waited on.
func (r Reader) ceiling() time.Duration {
	if r.Wait > 0 {
		return r.Wait
	}
	return defaultWait
}

// wait is the ceiling for r's mutations: the client's when set, else the
// type's.
func (c *Client) wait(r Reader) time.Duration {
	if c.Wait != 0 {
		return c.Wait
	}
	return r.ceiling()
}

// compileWait parses an override's wait.
func compileWait(o Override) (time.Duration, error) {
	if o.Wait == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(o.Wait)
	if err != nil {
		return 0, fmt.Errorf("wait %q is not a duration such as 20m", o.Wait)
	}
	if d <= 0 {
		return 0, fmt.Errorf("wait %q must be positive", o.Wait)
	}
	return d, nil
}

// settle waits, within the type's ceiling, until the instance address
// names is past every Busy condition, so a call the service refuses for an
// instance mid-change is not sent. A type with no Busy is not read.
func (c *Client) settle(ctx context.Context, r Reader, address map[string]any) error {
	if len(r.Busy) == 0 {
		return nil
	}
	parts := r.identifierOf(address)
	id := r.identifierString(parts)
	wait, poll := c.wait(r), c.poll()
	deadline := time.Now().Add(wait)
	for {
		_, _, busy, err := c.readCall(ctx, r, parts)
		if err == nil && !busy {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("the %s %s did not settle within %s: %w", r.Type, id, wait, err)
			}
			return fmt.Errorf("the %s %s was still busy after %s", r.Type, id, wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}
