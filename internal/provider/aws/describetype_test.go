package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

func TestClientDescribeType(t *testing.T) {
	t.Run("decodes the schema fields this package uses", func(t *testing.T) {
		cf := &fakeCF{out: &cloudformation.DescribeTypeOutput{
			Schema: aws.String(`{"primaryIdentifier":["/properties/Id"],"createOnlyProperties":["/properties/DistributionConfig"]}`),
		}}
		c := &Client{cf: cf}

		schema, err := c.DescribeType(context.Background(), TypeCloudFrontDistribution)
		if err != nil {
			t.Fatalf("DescribeType: %v", err)
		}
		if len(schema.PrimaryIdentifier) != 1 || schema.PrimaryIdentifier[0] != "/properties/Id" {
			t.Fatalf("PrimaryIdentifier = %v", schema.PrimaryIdentifier)
		}
		if len(schema.CreateOnly) != 1 {
			t.Fatalf("CreateOnlyProperties = %v", schema.CreateOnly)
		}
	})

	t.Run("TypeNotFoundException is a validation error", func(t *testing.T) {
		cf := &fakeCF{err: &cftypes.TypeNotFoundException{Message: aws.String("no such type")}}
		c := &Client{cf: cf}

		_, err := c.DescribeType(context.Background(), "AWS::Bogus::Type")
		if err == nil {
			t.Fatal("expected an error")
		}
		var kerr *kerrors.KError
		if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeValidation {
			t.Fatalf("err = %v, want a CodeValidation KError", err)
		}
	})

	t.Run("another error is wrapped, not swallowed", func(t *testing.T) {
		cf := &fakeCF{err: errors.New("boom")}
		c := &Client{cf: cf}

		if _, err := c.DescribeType(context.Background(), TypeS3Bucket); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("a nil schema is an error", func(t *testing.T) {
		cf := &fakeCF{out: &cloudformation.DescribeTypeOutput{}}
		c := &Client{cf: cf}

		if _, err := c.DescribeType(context.Background(), TypeS3Bucket); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("malformed schema JSON is an error", func(t *testing.T) {
		cf := &fakeCF{out: &cloudformation.DescribeTypeOutput{Schema: aws.String(`{not json`)}}
		c := &Client{cf: cf}

		if _, err := c.DescribeType(context.Background(), TypeS3Bucket); err == nil {
			t.Fatal("expected an error")
		}
	})
}
