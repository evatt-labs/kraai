package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// fakeSTS is a hand-rolled stsAPI: no AWS account, no network needed.
type fakeSTS struct {
	account string
	err     error
	calls   int
}

func (f *fakeSTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &sts.GetCallerIdentityOutput{Account: aws.String(f.account)}, nil
}

func TestClientAccountID(t *testing.T) {
	t.Run("resolves and caches", func(t *testing.T) {
		fsts := &fakeSTS{account: "123456789012"}
		c := &Client{sts: fsts}

		id, err := c.AccountID(context.Background())
		if err != nil || id != "123456789012" {
			t.Fatalf("AccountID = %q, err %v", id, err)
		}
		if _, err := c.AccountID(context.Background()); err != nil {
			t.Fatalf("second AccountID call: %v", err)
		}
		if fsts.calls != 1 {
			t.Fatalf("STS called %d times, want exactly 1 (cached after first success)", fsts.calls)
		}
	})

	t.Run("a transient failure is not cached", func(t *testing.T) {
		fsts := &fakeSTS{err: errors.New("throttled")}
		c := &Client{sts: fsts}

		if _, err := c.AccountID(context.Background()); err == nil {
			t.Fatal("expected an error")
		}
		fsts.err = nil
		fsts.account = "999999999999"
		id, err := c.AccountID(context.Background())
		if err != nil || id != "999999999999" {
			t.Fatalf("retry after failure: id=%q err=%v", id, err)
		}
	})

	t.Run("no account id in the response is an error", func(t *testing.T) {
		c := &Client{sts: &fakeSTS{account: ""}}
		if _, err := c.AccountID(context.Background()); err == nil {
			t.Fatal("expected an error for an empty account id")
		}
	})
}
