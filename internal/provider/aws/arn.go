package aws

import "fmt"

// The ARN shapes this package constructs locally, one pure format per
// resource shape. Every caller resolves Client.AccountID first, which is
// the one network call in the chain.

// functionARN builds a Lambda function's ARN from its derived name.
func functionARN(region, account, name string) string {
	return fmt.Sprintf("arn:aws:lambda:%s:%s:function:%s", region, account, name)
}

// roleARN builds an IAM role's ARN from its derived name. IAM ARNs carry no
// region segment.
func roleARN(account, name string) string {
	return fmt.Sprintf("arn:aws:iam::%s:role/%s", account, name)
}

// ruleARN builds an EventBridge rule's ARN on the default event bus, the
// only bus kraai creates rules on.
func ruleARN(region, account, name string) string {
	return fmt.Sprintf("arn:aws:events:%s:%s:rule/%s", region, account, name)
}

// executeAPIArn builds an API Gateway HTTP API's execute-api ARN for a
// Lambda invoke permission, scoped to every stage and route. apiID is
// AWS-assigned, so this always needs a live lookup.
//
// Two wildcards, not three: an HTTP API invokes with {apiID}/{stage}/{route},
// so the REST-style "/*/*/*" matches nothing and API Gateway is silently
// refused permission. The symptom is a bare 500 with no invocation in the
// function's log group. "*" spans "/", so two wildcards still cover the
// three-segment case.
func executeAPIArn(region, account, apiID string) string {
	return fmt.Sprintf("arn:aws:execute-api:%s:%s:%s/*/*", region, account, apiID)
}

// distributionARN builds a CloudFront distribution's ARN from its assigned
// id. CloudFront's ARNs carry no region segment, and the type's schema
// publishes no Arn attribute, so this is the one ARN here built from an id
// rather than a name.
func distributionARN(account, id string) string {
	return fmt.Sprintf("arn:aws:cloudfront::%s:distribution/%s", account, id)
}

func bucketARN(name string) string {
	return "arn:aws:s3:::" + name
}
