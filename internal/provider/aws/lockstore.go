package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/lock"
)

// lockBucketPrefix starts the name of the bucket a lock store keeps its
// records in: kraai-lock-<account>-<region>, one per account and region,
// in the operator's own account. Named from the account rather than the
// environment so every environment in the account shares one bucket and
// nothing has to be provisioned per environment before it can be locked.
const lockBucketPrefix = "kraai-lock-"

// Keys within the bucket. A lock and a status record are separate objects
// so the lock can be conditionally created and deleted on its own.
const (
	lockKeyPrefix   = "locks/"
	statusKeyPrefix = "status/"
)

// lockStore is a lock.Store over an S3 bucket, using S3's conditional
// writes: a lock is an object created only if absent (If-None-Match: *)
// and deleted only if unchanged (If-Match on its ETag), which is what makes
// two concurrent acquirers see exactly one winner without a coordinator.
type lockStore struct {
	client *Client
	now    func() time.Time

	bucketMu sync.Mutex
	bucket   string
}

// NewLockStore returns a lock.Store backed by the account's lock bucket,
// which is created on first use.
func NewLockStore(client *Client) lock.Store {
	return &lockStore{client: client, now: time.Now}
}

// bucketName resolves the account's lock bucket and, when create is set,
// makes it on first use. A read (ReadStatus) never creates it: a bucket
// that does not exist holds no record, and a status query must not leave
// a bucket behind in an account it only looked at.
func (s *lockStore) bucketName(ctx context.Context, create bool) (string, bool, error) {
	s.bucketMu.Lock()
	defer s.bucketMu.Unlock()
	if s.bucket != "" {
		return s.bucket, true, nil
	}
	account, err := s.client.AccountID(ctx)
	if err != nil {
		return "", false, err
	}
	name := lockBucketPrefix + account + "-" + s.client.Region()
	exists, err := s.client.ensureBucket(ctx, name, create)
	if err != nil {
		return "", false, err
	}
	if exists {
		s.bucket = name
	}
	return name, exists, nil
}

// ensureBucket reports whether name exists in this client's region and,
// when create is set and it does not, creates it with public access
// blocked. A bucket this account already owns is success; one another
// account owns is an error naming it, since a lock in someone else's bucket
// is no lock.
func (c *Client) ensureBucket(ctx context.Context, name string, create bool) (bool, error) {
	_, err := c.s3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(name)})
	if err == nil {
		return true, nil
	}
	if !s3ErrorCodeIs(err, "NotFound", "NoSuchBucket") {
		return false, kerrors.Wrap(err, kerrors.CodeUnexpected, "checking for the lock bucket %s", name)
	}
	if !create {
		return false, nil
	}
	input := &s3.CreateBucketInput{Bucket: aws.String(name)}
	// us-east-1 refuses a location constraint naming itself; every other
	// region requires one.
	if c.region != "us-east-1" {
		input.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(c.region),
		}
	}
	if _, err := c.s3.CreateBucket(ctx, input); err != nil && !s3ErrorCodeIs(err, "BucketAlreadyOwnedByYou") {
		return false, kerrors.Wrap(err, kerrors.CodeUnexpected, "creating the lock bucket %s", name)
	}
	_, err = c.s3.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket: aws.String(name),
		PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
			BlockPublicAcls:       aws.Bool(true),
			BlockPublicPolicy:     aws.Bool(true),
			IgnorePublicAcls:      aws.Bool(true),
			RestrictPublicBuckets: aws.Bool(true),
		},
	})
	if err != nil {
		return false, kerrors.Wrap(err, kerrors.CodeUnexpected, "blocking public access on the lock bucket %s", name)
	}
	return true, nil
}

// s3ErrorCodeIs reports whether err is an S3 API error with one of codes.
func s3ErrorCodeIs(err error, codes ...string) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	for _, code := range codes {
		if apiErr.ErrorCode() == code {
			return true
		}
	}
	return false
}

// conditionFailed is S3 refusing a conditional write: the object already
// existed for an If-None-Match, changed for an If-Match, or was being
// written by someone else at the same moment.
func conditionFailed(err error) bool {
	return s3ErrorCodeIs(err, "PreconditionFailed", "ConditionalRequestConflict")
}

// Acquire implements lock.Store. Two attempts at most: the first create,
// and one more after breaking a lock found expired, so a lock that keeps
// being retaken by someone else is reported held rather than fought over.
func (s *lockStore) Acquire(ctx context.Context, environment, holder string, lease time.Duration) (lock.Lease, error) {
	bucket, _, err := s.bucketName(ctx, true)
	if err != nil {
		return nil, err
	}
	key := lockKeyPrefix + environment + ".json"
	for attempt := range 2 {
		now := s.now()
		record := lock.Record{Environment: environment, Holder: holder, AcquiredAt: now, ExpiresAt: now.Add(lease)}
		body, err := json.Marshal(record)
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "encoding the lock for %s", environment)
		}
		out, err := s.client.s3.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(bucket),
			Key:         aws.String(key),
			Body:        bytes.NewReader(body),
			ContentType: aws.String("application/json"),
			IfNoneMatch: aws.String("*"),
		})
		if err == nil {
			return &s3Lease{store: s, bucket: bucket, key: key, etag: aws.ToString(out.ETag)}, nil
		}
		if !conditionFailed(err) {
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "acquiring the lock for %s", environment)
		}
		current, etag, found, err := s.readRecord(ctx, bucket, key)
		if err != nil {
			return nil, err
		}
		if !found {
			// Released between the refused create and the read; the next
			// attempt takes it.
			continue
		}
		if s.now().Before(current.ExpiresAt) || attempt > 0 {
			return nil, &lock.HeldError{Record: current}
		}
		// Expired: break it, but only the very object read, so a release or
		// a fresh acquire that landed since is not deleted from under them.
		_, err = s.client.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key), IfMatch: aws.String(etag),
		})
		if err != nil && !conditionFailed(err) {
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "breaking the expired lock for %s", environment)
		}
	}
	return nil, kerrors.New("acquiring the lock for %s: gave up after two attempts", environment)
}

// readRecord fetches and decodes the lock object, with its ETag.
func (s *lockStore) readRecord(ctx context.Context, bucket, key string) (lock.Record, string, bool, error) {
	out, err := s.client.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		if s3ErrorCodeIs(err, "NoSuchKey", "NotFound") {
			return lock.Record{}, "", false, nil
		}
		return lock.Record{}, "", false, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading the lock at %s", key)
	}
	defer func() { _ = out.Body.Close() }()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		return lock.Record{}, "", false, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading the lock at %s", key)
	}
	var record lock.Record
	if err := json.Unmarshal(body, &record); err != nil {
		return lock.Record{}, "", false, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding the lock at %s", key)
	}
	return record, aws.ToString(out.ETag), true, nil
}

type s3Lease struct {
	store  *lockStore
	bucket string
	key    string
	etag   string
}

// Release deletes the lock, only if it is still the object this lease
// created: a lock broken after this lease expired and retaken by another
// run is theirs, and the refused delete is success here.
func (l *s3Lease) Release(ctx context.Context) error {
	_, err := l.store.client.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(l.bucket), Key: aws.String(l.key), IfMatch: aws.String(l.etag),
	})
	if err != nil && !conditionFailed(err) {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "releasing the lock at %s", l.key)
	}
	return nil
}

// ReadStatus implements lock.Store.
func (s *lockStore) ReadStatus(ctx context.Context, environment string) (lock.Status, bool, error) {
	bucket, exists, err := s.bucketName(ctx, false)
	if err != nil || !exists {
		return lock.Status{}, false, err
	}
	key := statusKeyPrefix + environment + ".json"
	out, err := s.client.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		if s3ErrorCodeIs(err, "NoSuchKey", "NotFound") {
			return lock.Status{}, false, nil
		}
		return lock.Status{}, false, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading the status of %s", environment)
	}
	defer func() { _ = out.Body.Close() }()
	var status lock.Status
	if err := json.NewDecoder(out.Body).Decode(&status); err != nil {
		return lock.Status{}, false, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding the status of %s", environment)
	}
	return status, true, nil
}

// WriteStatus implements lock.Store.
func (s *lockStore) WriteStatus(ctx context.Context, status lock.Status) error {
	bucket, _, err := s.bucketName(ctx, true)
	if err != nil {
		return err
	}
	body, err := json.Marshal(status)
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "encoding the status of %s", status.Environment)
	}
	_, err = s.client.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(statusKeyPrefix + status.Environment + ".json"),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "writing the status of %s", status.Environment)
	}
	return nil
}

// DeleteStatus implements lock.Store.
func (s *lockStore) DeleteStatus(ctx context.Context, environment string) error {
	bucket, exists, err := s.bucketName(ctx, false)
	if err != nil || !exists {
		return err
	}
	_, err = s.client.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(statusKeyPrefix + environment + ".json"),
	})
	if err != nil && !s3ErrorCodeIs(err, "NoSuchKey", "NotFound") {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "deleting the status of %s", environment)
	}
	return nil
}
