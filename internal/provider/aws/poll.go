package aws

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Default polling bounds for asynchronous Cloud Control operations. A
// CloudFront distribution's propagation alone can run past fifteen minutes,
// while an S3 bucket create finishes in under a second, so the backoff
// starts small and grows. WithPollTimings overrides all three.
const (
	defaultPollInitialDelay = 2 * time.Second
	defaultPollMaxDelay     = 30 * time.Second
	defaultPollTimeout      = 40 * time.Minute
)

// pollTimings resolves the effective backoff and timeout, falling back to
// the defaults for a Client built by a struct literal, as tests do, so the
// poll loop never busy-spins on a zero delay.
func (c *Client) pollTimings() (initialDelay, maxDelay, timeout time.Duration) {
	initialDelay, maxDelay, timeout = c.pollInitialDelay, c.pollMaxDelay, c.pollTimeout
	if initialDelay <= 0 {
		initialDelay = defaultPollInitialDelay
	}
	if maxDelay <= 0 {
		maxDelay = defaultPollMaxDelay
	}
	if timeout <= 0 {
		timeout = defaultPollTimeout
	}
	return initialDelay, maxDelay, timeout
}

// pollToTerminal polls requestToken's status until it reaches a terminal
// OperationStatus, or until the status call fails, ctx is cancelled, or the
// poll timeout elapses. It returns the raw terminal event rather than
// deciding whether FAILED is an error: Delete treats NotFound as success
// and Create does not, and each verb keeps that decision.
//
// Backoff starts at initialDelay and doubles up to maxDelay, honouring Cloud
// Control's RetryAfter hint when it is later. Every iteration either
// returns or waits.
func (c *Client) pollToTerminal(ctx context.Context, requestToken, typeName, identifier string) (*cctypes.ProgressEvent, error) {
	initialDelay, maxDelay, timeout := c.pollTimings()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	delay := initialDelay
	var lastStatus cctypes.OperationStatus
	for {
		out, err := c.cc.GetResourceRequestStatus(ctx, &cloudcontrol.GetResourceRequestStatusInput{
			RequestToken: aws.String(requestToken),
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, kerrors.Wrap(ctx.Err(), kerrors.CodeUnexpected,
					"polling %s %q timed out or was cancelled (last known status %q)", typeName, identifier, lastStatus)
			}
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "polling status of %s %q", typeName, identifier)
		}
		if out.ProgressEvent == nil {
			return nil, kerrors.Validation("polling %s %q returned no progress event", typeName, identifier)
		}
		event := out.ProgressEvent
		lastStatus = event.OperationStatus

		switch event.OperationStatus {
		case cctypes.OperationStatusSuccess, cctypes.OperationStatusFailed, cctypes.OperationStatusCancelComplete:
			return event, nil
		}

		wait := delay
		if event.RetryAfter != nil {
			if until := time.Until(*event.RetryAfter); until > wait {
				wait = until
			}
		}
		select {
		case <-ctx.Done():
			return nil, kerrors.Wrap(ctx.Err(), kerrors.CodeUnexpected,
				"polling %s %q timed out or was cancelled (last known status %q)", typeName, identifier, lastStatus)
		case <-time.After(wait):
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// terminalFailureCodes are the HandlerErrorCodes that mean the caller did
// something the API will never accept as is: a validation problem the
// manifest can fix. Everything else is CodeUnexpected.
var terminalFailureCodes = map[cctypes.HandlerErrorCode]bool{
	cctypes.HandlerErrorCodeNotUpdatable:                 true,
	cctypes.HandlerErrorCodeInvalidRequest:               true,
	cctypes.HandlerErrorCodeAccessDenied:                 true,
	cctypes.HandlerErrorCodeUnauthorizedTaggingOperation: true,
	cctypes.HandlerErrorCodeInvalidCredentials:           true,
	cctypes.HandlerErrorCodeAlreadyExists:                true,
	cctypes.HandlerErrorCodeNotFound:                     true,
	cctypes.HandlerErrorCodeResourceConflict:             true,
	cctypes.HandlerErrorCodeServiceLimitExceeded:         true,
}

// translateFailure maps a terminal FAILED or CANCEL_COMPLETE event onto a
// kerrors bucket, naming the resource, the status and the handler's code.
func translateFailure(op, typeName, identifier string, event *cctypes.ProgressEvent) error {
	statusMessage := "no status message"
	if event.StatusMessage != nil && *event.StatusMessage != "" {
		statusMessage = *event.StatusMessage
	}

	code := kerrors.CodeUnexpected
	if terminalFailureCodes[event.ErrorCode] {
		code = kerrors.CodeValidation
	}
	return kerrors.Wrap(errors.New(statusMessage), code,
		"%s %s %q: operation ended %s (error code %q)", op, typeName, identifier, event.OperationStatus, event.ErrorCode)
}

// decodeResourceModel decodes a terminal ProgressEvent's ResourceModel into
// a properties map. Cloud Control promises the final model on SUCCESS, so no
// follow-up GetResource is needed.
func decodeResourceModel(model *string) (map[string]any, error) {
	if model == nil || *model == "" {
		return map[string]any{}, nil
	}
	var properties map[string]any
	if err := json.Unmarshal([]byte(*model), &properties); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding resource model")
	}
	return properties, nil
}
