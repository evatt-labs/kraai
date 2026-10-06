package direct

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
)

// redirectError is S3's answer to a bucket addressed at another region's
// endpoint: 301 PermanentRedirect, with no Location, and the bucket's
// region in X-Amz-Bucket-Region.
type redirectError struct {
	Region string
	err    error
}

func (e *redirectError) Error() string {
	return fmt.Sprintf("%v; the bucket is in %s", e.err, e.Region)
}

func (e *redirectError) Unwrap() error { return e.err }

// redirected is err as a redirectError when resp moved the request to
// another region, else err.
func redirected(resp *http.Response, err error) error {
	if region := resp.Header.Get("X-Amz-Bucket-Region"); resp.StatusCode == http.StatusMovedPermanently && region != "" {
		return &redirectError{Region: region, err: err}
	}
	return err
}

// regionShape is what a region name looks like, so a redirect cannot name
// anything else into the host a request is sent to.
var regionShape = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)

// movedTo returns a copy of c in the region err redirected a call to, or
// false when err is no redirect this client can follow: one to its own
// region, or to a region outside its partition or not shaped as one.
func (c *Client) movedTo(err error) (*Client, bool) {
	var moved *redirectError
	if !errors.As(err, &moved) || moved.Region == c.Region || !regionShape.MatchString(moved.Region) {
		return nil, false
	}
	from, ferr := awsPartition(c.Region)
	to, terr := awsPartition(moved.Region)
	if ferr != nil || terr != nil || from["name"] != to["name"] {
		return nil, false
	}
	there := *c
	there.Region = moved.Region
	return &there, true
}

// readFollowing is readCall, made again in the bucket's region when the
// first answer is a redirect there. It returns the client the read was
// made through, for the further calls and the properties built from the
// region.
func (c *Client) readFollowing(ctx context.Context, r Reader, identifier map[string]string) (*Client, map[string]any, map[string]any, error) {
	props, captured, _, err := c.readCall(ctx, r, identifier)
	if there, ok := c.movedTo(err); ok {
		c = there
		props, captured, _, err = c.readCall(ctx, r, identifier)
	}
	return c, props, captured, err
}

// following runs do through c, and again through the bucket's region when
// a call was redirected there. Every call of one instance goes to the same
// bucket, so the first is the one redirected, and a redirect is answered
// without anything being done.
func (c *Client) following(do func(*Client) error) error {
	err := do(c)
	if there, ok := c.movedTo(err); ok {
		err = do(there)
	}
	return err
}
