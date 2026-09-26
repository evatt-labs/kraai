package direct

import (
	"context"
	"strings"
	"testing"
)

// A read whose response lacks a captured value cannot address its further
// calls, and says so rather than sending an empty ARN.
func TestReadWithoutACapturedValueFails(t *testing.T) {
	noARN := strings.Replace(dbSubnetGroupXML, "<DBSubnetGroupArn>arn:aws:rds:us-east-1:1:subgrp:my-group</DBSubnetGroupArn>", "", 1)
	client, forms := xmlServerBy(t, map[string]string{"DescribeDBSubnetGroups": noARN, "ListTagsForResource": tagsXML})
	_, err := client.Read(context.Background(), dbSubnetGroups, map[string]string{"DBSubnetGroupName": "my-group"})
	if err == nil || !strings.Contains(err.Error(), "did not return Arn") {
		t.Fatalf("Read = %v, want the missing capture named", err)
	}
	if len(*forms) != 1 {
		t.Fatalf("%d requests, want only the read", len(*forms))
	}
}

func TestCompileRefusesABadCapture(t *testing.T) {
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	var base Override
	for _, o := range all {
		if o.Type == dbSubnetGroups {
			base = o
		}
	}
	cases := map[string]struct {
		edit func(*Override)
		want string
	}{
		"a member the resource lacks": {func(o *Override) { o.Read.Capture = map[string]string{"Arn": "Nope"} }, "capture Arn: Nope is not a string member"},
		"a structure":                 {func(o *Override) { o.Read.Capture = map[string]string{"Arn": "Subnets"} }, "capture Arn: Subnets is not a string member"},
		"the primary identifier": {func(o *Override) {
			o.Read.Capture = map[string]string{"Arn": "DBSubnetGroupArn", "DBSubnetGroupName": "DBSubnetGroupName"}
		}, "capture DBSubnetGroupName is the primary identifier"},
		"a capture no call names": {func(o *Override) {
			o.Read.Capture = map[string]string{"Arn": "DBSubnetGroupArn", "Vpc": "VpcId"}
		}, "capture Vpc is named by no further call"},
		"a placeholder never captured": {func(o *Override) { o.Read.Capture = nil }, "read input ResourceName names {Arn}, which is not the primary identifier"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			o := base
			o.Read.Capture = map[string]string{}
			for k, v := range base.Read.Capture {
				o.Read.Capture[k] = v
			}
			c.edit(&o)
			if _, errs := compileOne(files, lock, o); !containsErr(errs, c.want) {
				t.Fatalf("errors = %v\nwant one containing %q", errs, c.want)
			}
		})
	}
}
