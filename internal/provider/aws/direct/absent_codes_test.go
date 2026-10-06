package direct

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// notFoundShaped names an error shape that says what was asked for is not
// there.
var notFoundShaped = regexp.MustCompile(`(?i)NotFound|NoSuch|DoesNotExist|NonExistent`)

// notAbsence is each read's not-found-shaped code that does not mean the
// instance is gone, and why.
var notAbsence = map[string]map[string]string{
	"AWS::ElasticLoadBalancingV2::TargetGroup": {"LoadBalancerNotFound": "the load balancer a filter names, not the target group"},
	"AWS::SSM::Parameter":                      {"ParameterVersionNotFound": "a version the read does not ask for"},
}

// TestReadsListTheirNotFoundCodes holds every read to listing each error
// its operation declares for something not there: left out, a read of a
// gone instance fails instead of reading absent, and falls back to Cloud
// Control for an answer the direct read had (#466).
func TestReadsListTheirNotFoundCodes(t *testing.T) {
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := loadLock(files)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range all {
		m, err := loadModel(files, lock.Models[o.Read.Model].File)
		if err != nil {
			t.Fatal(err)
		}
		var service, namespace, protocol string
		for id, s := range m.Shapes {
			if s.Type == "service" {
				service, namespace = id, id[:strings.Index(id, "#")+1]
			}
		}
		for _, p := range protocolPreference {
			if m.Shapes[service].Traits["aws.protocols#"+p] != nil {
				protocol = p
				break
			}
		}
		op, ok := m.Shapes[namespace+o.Read.Operation]
		if !ok {
			t.Fatalf("%s: no operation %s", o.Type, o.Read.Operation)
		}
		codes := wireCodesOf(&m, protocol, op.Errors)
		for target, code := range codes {
			if !notFoundShaped.MatchString(target) || slices.Contains(o.Read.AbsentErrors, code) {
				continue
			}
			if _, exempt := notAbsence[o.Type][code]; exempt {
				continue
			}
			t.Errorf("%s: %s declares %s, which the read does not list as absence", o.Type, o.Read.Operation, code)
		}
	}
}

// wireCodesOf is each error target's wire code under protocol, keyed by
// the target's shape name.
func wireCodesOf(m *smithyModel, protocol string, errs []smithyMember) map[string]string {
	out := map[string]string{}
	for _, e := range errs {
		name := e.Target[strings.Index(e.Target, "#")+1:]
		code := name
		if protocol == "awsQuery" || protocol == "ec2Query" {
			var q struct{ Code string }
			if json.Unmarshal(m.Shapes[e.Target].Traits["aws.protocols#awsQueryError"], &q) == nil && q.Code != "" {
				code = q.Code
			}
		}
		out[name] = code
	}
	return out
}
