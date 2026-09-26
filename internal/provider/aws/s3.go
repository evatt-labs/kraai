package aws

import (
	"bytes"
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// maxEmptyBucketPages bounds EmptyBucket's page walk over ListObjectsV2.
const maxEmptyBucketPages = 100

// maxOwnsBucketPages bounds OwnsBucket's page walk over ListBuckets. Its
// Prefix filter narrows a real account's response to a handful of buckets,
// so this exists only so a broken pagination contract cannot spin forever.
const maxOwnsBucketPages = 100

// PutObject uploads body to bucket/key, replacing any existing object. No
// existence check first: PutObject is itself an overwrite, and the artifact
// key is a content hash, so a repeat write for unchanged input writes back
// bytes S3 already holds.
func (c *Client) PutObject(ctx context.Context, bucket, key string, body []byte) error {
	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "uploading s3://%s/%s", bucket, key)
	}
	return nil
}

// EmptyBucket deletes every object in bucket, so a subsequent bucket delete
// through Cloud Control can succeed; S3 refuses to delete a non-empty
// bucket, and a destroy once left every artifact bucket behind.
//
// Each ListObjectsV2 page is deleted as its own DeleteObjects batch, since
// both are capped at 1000 keys. No version or delete-marker handling: this
// package never enables versioning on a bucket it creates, so current
// objects are everything such a bucket can hold. A NoSuchBucket from the
// listing means the bucket is already gone, which is success; every other
// failure, including a per-object error inside a successful DeleteObjects
// response, is returned naming the bucket.
func (c *Client) EmptyBucket(ctx context.Context, bucket string) error {
	var token *string
	for range maxEmptyBucketPages {
		out, err := c.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			ContinuationToken: token,
		})
		if err != nil {
			var notFound *s3types.NoSuchBucket
			if errors.As(err, &notFound) {
				return nil
			}
			return kerrors.Wrap(err, kerrors.CodeUnexpected, "listing objects in bucket %q", bucket)
		}

		if len(out.Contents) > 0 {
			ids := make([]s3types.ObjectIdentifier, len(out.Contents))
			for i, obj := range out.Contents {
				ids[i] = s3types.ObjectIdentifier{Key: obj.Key}
			}
			delOut, err := c.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(bucket),
				Delete: &s3types.Delete{Objects: ids},
			})
			if err != nil {
				return kerrors.Wrap(err, kerrors.CodeUnexpected, "deleting objects from bucket %q", bucket)
			}
			// A DeleteObjects call can succeed overall while individual
			// keys failed; S3 reports those per object. The first is enough
			// to act on.
			if len(delOut.Errors) > 0 {
				first := delOut.Errors[0]
				key, code, msg := "<unknown key>", "<unknown code>", "<no message>"
				if first.Key != nil {
					key = *first.Key
				}
				if first.Code != nil {
					code = *first.Code
				}
				if first.Message != nil {
					msg = *first.Message
				}
				return kerrors.Wrap(errors.New(msg), kerrors.CodeUnexpected,
					"deleting %d of %d object(s) from bucket %q failed (e.g. key %q: %s)",
					len(delOut.Errors), len(ids), bucket, key, code)
			}
		}

		if out.IsTruncated == nil || !*out.IsTruncated || out.NextContinuationToken == nil {
			return nil
		}
		token = out.NextContinuationToken
	}
	return kerrors.Validation("emptying bucket %q did not terminate within %d pages", bucket, maxEmptyBucketPages)
}

// OwnsBucket reports whether bucket belongs to this AWS account.
//
// Bucket names are global, and Cloud Control's GetResource for a bucket
// resolves that namespace without checking ownership, so a stranger's
// bucket that collides with a derived name would read as "exists, no
// change" and then be written into or deleted. HeadBucket cannot answer
// this: with or without ExpectedBucketOwner it returns the same 403 for a
// foreign bucket as for one of ours the caller's IAM cannot see, and AWS
// documents that ambiguity as intentional. ListBuckets returns only the
// buckets the authenticated account owns, so it can never be fooled by a
// bucket policy and never has to compare an account id. A failure of the
// call itself, most likely a missing s3:ListAllMyBuckets, is returned as an
// error and must never be read as "not owned".
//
// Filtered by Prefix: an unpaginated ListBuckets is rejected once an
// account exceeds 10,000 buckets. Prefix is "begins with", so each
// candidate's name is still checked for equality.
func (c *Client) OwnsBucket(ctx context.Context, bucket string) (bool, error) {
	var token *string
	for range maxOwnsBucketPages {
		out, err := c.s3.ListBuckets(ctx, &s3.ListBucketsInput{
			Prefix:            aws.String(bucket),
			ContinuationToken: token,
		})
		if err != nil {
			return false, kerrors.Wrap(err, kerrors.CodeUnexpected,
				"listing this account's own S3 buckets to check ownership of %q", bucket)
		}
		for _, b := range out.Buckets {
			if b.Name != nil && *b.Name == bucket {
				return true, nil
			}
		}
		if out.ContinuationToken == nil || *out.ContinuationToken == "" {
			return false, nil
		}
		token = out.ContinuationToken
	}
	return false, kerrors.Validation("checking ownership of bucket %q did not terminate within %d pages", bucket, maxOwnsBucketPages)
}

// bucketPolicyAbsent reports whether err is S3 saying a bucket policy, or
// the bucket itself, is not there. NoSuchBucketPolicy has no typed Go error
// the way NoSuchBucket does, so one ErrorCode check recognizes either.
func bucketPolicyAbsent(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case "NoSuchBucketPolicy", "NoSuchBucket":
		return true
	default:
		return false
	}
}

// PutBucketPolicy replaces bucket's entire policy with document.
func (c *Client) PutBucketPolicy(ctx context.Context, bucket, document string) error {
	_, err := c.s3.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String(bucket),
		Policy: aws.String(document),
	})
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "putting bucket policy on %q", bucket)
	}
	return nil
}

// GetBucketPolicy returns bucket's current policy document, or found=false
// when the bucket has no policy or does not exist. Every other error is
// returned, never folded into absence.
func (c *Client) GetBucketPolicy(ctx context.Context, bucket string) (string, bool, error) {
	out, err := c.s3.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(bucket)})
	if err != nil {
		if bucketPolicyAbsent(err) {
			return "", false, nil
		}
		return "", false, kerrors.Wrap(err, kerrors.CodeUnexpected, "getting bucket policy for %q", bucket)
	}
	if out.Policy == nil {
		return "", false, nil
	}
	return *out.Policy, true, nil
}

// DeleteBucketPolicy removes bucket's policy, tolerating one already absent
// or a bucket already gone.
func (c *Client) DeleteBucketPolicy(ctx context.Context, bucket string) error {
	_, err := c.s3.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: aws.String(bucket)})
	if err != nil && !bucketPolicyAbsent(err) {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "deleting bucket policy for %q", bucket)
	}
	return nil
}
