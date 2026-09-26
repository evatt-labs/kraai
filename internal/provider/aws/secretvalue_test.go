package aws

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestClientSecretValue(t *testing.T) {
	c := &Client{sm: &fakeSecretsManager{value: `{"username":"u","password":"p"}`}}
	value, err := c.SecretValue(context.Background(), "arn:secret")
	if err != nil || value != `{"username":"u","password":"p"}` {
		t.Fatalf("SecretValue = %q, %v", value, err)
	}
	empty := &Client{sm: &fakeSecretsManager{value: ""}}
	if _, err := empty.SecretValue(context.Background(), "arn:secret"); err == nil {
		t.Fatal("SecretValue(empty) succeeded, want an error")
	}
	failing := &Client{sm: &fakeSecretsManager{err: errors.New("denied")}}
	if _, err := failing.SecretValue(context.Background(), "arn:secret"); err == nil || !strings.Contains(err.Error(), "arn:secret") {
		t.Fatalf("SecretValue(failure): err = %v, want an error carrying no value", err)
	}
}
