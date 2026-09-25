package direct

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const (
	subnets      = "AWS::EC2::Subnet"
	targetGroups = "AWS::ElasticLoadBalancingV2::TargetGroup"
)

// xmlServer answers every request with status and body, recording each
// request's form.
func xmlServer(t *testing.T, status int, body string) (*Client, *[]url.Values) {
	t.Helper()
	var forms []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		if r.Method != http.MethodPost || r.URL.Path != "/" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			t.Errorf("request = %s %s %s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		forms = append(forms, form)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Client{
		HTTP:        srv.Client(),
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region:      "us-east-1",
		Endpoint:    func(string) string { return srv.URL },
		Now:         func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) },
	}, &forms
}

const subnetXML = `<?xml version="1.0" encoding="UTF-8"?>
<DescribeSubnetsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <requestId>r</requestId>
  <subnetSet>
    <item>
      <subnetId>subnet-1</subnetId>
      <cidrBlock>10.0.0.0/24</cidrBlock>
      <mapPublicIpOnLaunch>true</mapPublicIpOnLaunch>
      <enableDns64>false</enableDns64>
      <tagSet>
        <item><key>Name</key><value>a b</value></item>
        <item><key>env</key><value></value></item>
      </tagSet>
      <privateDnsNameOptionsOnLaunch><hostnameType>ip-name</hostnameType></privateDnsNameOptionsOnLaunch>
    </item>
  </subnetSet>
</DescribeSubnetsResponse>`

// ec2Query: the identifier as a flattened list, the resource inside the
// one item of the list, booleans typed, a list of structures, a nested
// structure, and text kept exactly.
// xmlServerBy answers each request by its Action, recording every form.
func xmlServerBy(t *testing.T, byAction map[string]string) (*Client, *[]url.Values) {
	t.Helper()
	var (
		mu    sync.Mutex
		forms []url.Values
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		mu.Lock()
		forms = append(forms, form)
		mu.Unlock()
		_, _ = io.WriteString(w, byAction[form.Get("Action")])
	}))
	t.Cleanup(srv.Close)
	return &Client{
		HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL },
	}, &forms
}

func networkACLsXML(assocs ...[2]string) string {
	var b strings.Builder
	for _, a := range assocs {
		fmt.Fprintf(&b, "<item><networkAclAssociationId>%s</networkAclAssociationId><subnetId>%s</subnetId></item>", a[0], a[1])
	}
	return `<DescribeNetworkAclsResponse><networkAclSet><item><networkAclId>acl-1</networkAclId><associationSet>` +
		b.String() + `</associationSet></item></networkAclSet></DescribeNetworkAclsResponse>`
}

// ec2Query: the identifier as a flattened list, the resource inside the
// one item of the list, booleans typed, a list of structures, a nested
// structure, and text kept exactly; then the network ACL call, filtered
// by the subnet, with its association selected by subnet id.
func TestReadEC2Query(t *testing.T) {
	client, forms := xmlServerBy(t, map[string]string{
		"DescribeSubnets":     subnetXML,
		"DescribeNetworkAcls": networkACLsXML([2]string{"aclassoc-2", "subnet-2"}, [2]string{"aclassoc-1", "subnet-1"}),
	})
	got, err := client.Read(context.Background(), subnets, map[string]string{"SubnetId": "subnet-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"SubnetId": "subnet-1", "CidrBlock": "10.0.0.0/24", "MapPublicIpOnLaunch": true, "EnableDns64": false,
		"Tags":                          []any{map[string]any{"Key": "Name", "Value": "a b"}, map[string]any{"Key": "env", "Value": ""}},
		"PrivateDnsNameOptionsOnLaunch": map[string]any{"HostnameType": "ip-name"},
		"NetworkAclAssociationId":       "aclassoc-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant   %#v", got, want)
	}
	wantForms := []url.Values{
		{"Action": {"DescribeSubnets"}, "Version": {"2016-11-15"}, "SubnetId.1": {"subnet-1"}},
		{"Action": {"DescribeNetworkAcls"}, "Version": {"2016-11-15"}, "Filter.1.Name": {"association.subnet-id"}, "Filter.1.Value.1": {"subnet-1"}},
	}
	if !reflect.DeepEqual(*forms, wantForms) {
		t.Fatalf("forms = %v\nwant    %v", *forms, wantForms)
	}
}

// An ec2Query read answered with a nextToken is incomplete, in the read and
// in a further call alike.
func TestReadEC2QueryPageTokenIsIncomplete(t *testing.T) {
	emptySubnets := `<DescribeSubnetsResponse><subnetSet/><nextToken>t</nextToken></DescribeSubnetsResponse>`
	pagedACLs := strings.Replace(networkACLsXML([2]string{"aclassoc-1", "subnet-1"}), "</networkAclSet>", "</networkAclSet><nextToken>t</nextToken>", 1)
	for name, byAction := range map[string]map[string]string{
		"read":    {"DescribeSubnets": emptySubnets, "DescribeNetworkAcls": networkACLsXML([2]string{"aclassoc-1", "subnet-1"})},
		"further": {"DescribeSubnets": subnetXML, "DescribeNetworkAcls": pagedACLs},
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := xmlServerBy(t, byAction)
			_, err := client.Read(context.Background(), subnets, map[string]string{"SubnetId": "subnet-1"})
			if err == nil || errors.Is(err, ErrAbsent) || !strings.Contains(err.Error(), "page token") {
				t.Fatalf("Read = %v, want an incomplete-response error", err)
			}
		})
	}
}

// A selection that finds two elements is an error, never the first of
// them: the read must not guess which one Cloud Control would report.
func TestReadAmbiguousSelectionFails(t *testing.T) {
	client, _ := xmlServerBy(t, map[string]string{
		"DescribeSubnets":     subnetXML,
		"DescribeNetworkAcls": networkACLsXML([2]string{"aclassoc-1", "subnet-1"}, [2]string{"aclassoc-9", "subnet-1"}),
	})
	if _, err := client.Read(context.Background(), subnets, map[string]string{"SubnetId": "subnet-1"}); err == nil || !strings.Contains(err.Error(), "selects 2 elements") {
		t.Fatalf("Read = %v, want an ambiguous-selection error", err)
	}
}

const targetGroupXML = `<DescribeTargetGroupsResponse xmlns="http://elasticloadbalancing.amazonaws.com/doc/2015-12-01/">
  <DescribeTargetGroupsResult>
    <TargetGroups>
      <member>
        <TargetGroupArn>arn:aws:elasticloadbalancing:us-east-1:1:targetgroup/tg/abc</TargetGroupArn>
        <TargetGroupName>tg</TargetGroupName>
        <Port>80</Port>
        <HealthCheckIntervalSeconds>30</HealthCheckIntervalSeconds>
        <LoadBalancerArns><member>arn:lb1</member><member>arn:lb2</member></LoadBalancerArns>
        <Matcher><HttpCode>200</HttpCode></Matcher>
      </member>
    </TargetGroups>
  </DescribeTargetGroupsResult>
  <ResponseMetadata><RequestId>r</RequestId></ResponseMetadata>
</DescribeTargetGroupsResponse>`

// awsQuery: the identifier inside the list's member element, the output
// inside its Result wrapper, numbers typed, a list of scalars, and one
// member mapped to two properties.
func TestReadAWSQuery(t *testing.T) {
	byAction := map[string]string{
		"DescribeTargetGroups": targetGroupXML,
		"DescribeTags": `<DescribeTagsResponse><DescribeTagsResult><TagDescriptions><member>
			<ResourceArn>arn:tg</ResourceArn><Tags><member><Key>k</Key><Value>v</Value></member></Tags>
		</member></TagDescriptions></DescribeTagsResult></DescribeTagsResponse>`,
		"DescribeTargetGroupAttributes": `<DescribeTargetGroupAttributesResponse><DescribeTargetGroupAttributesResult><Attributes>
			<member><Key>stickiness.enabled</Key><Value>false</Value></member>
		</Attributes></DescribeTargetGroupAttributesResult></DescribeTargetGroupAttributesResponse>`,
		"DescribeTargetHealth": `<DescribeTargetHealthResponse><DescribeTargetHealthResult><TargetHealthDescriptions>
			<member><Target><Id>i-1</Id><Port>80</Port></Target><TargetHealth><State>healthy</State></TargetHealth></member>
			<member><Target><Id>i-2</Id><Port>81</Port></Target></member>
		</TargetHealthDescriptions></DescribeTargetHealthResult></DescribeTargetHealthResponse>`,
	}
	var (
		mu    sync.Mutex
		forms []url.Values
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		mu.Lock()
		forms = append(forms, form)
		mu.Unlock()
		_, _ = io.WriteString(w, byAction[form.Get("Action")])
	}))
	t.Cleanup(srv.Close)
	client := &Client{
		HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL },
	}
	got, err := client.Read(context.Background(), targetGroups, map[string]string{"TargetGroupArn": "arn:aws:elasticloadbalancing:us-east-1:1:targetgroup/tg/abc"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"TargetGroupArn": "arn:aws:elasticloadbalancing:us-east-1:1:targetgroup/tg/abc", "TargetGroupName": "tg", "Name": "tg",
		"TargetGroupFullName": "targetgroup/tg/abc",
		"Port":                json.Number("80"), "HealthCheckIntervalSeconds": json.Number("30"),
		"LoadBalancerArns":      []any{"arn:lb1", "arn:lb2"},
		"Matcher":               map[string]any{"HttpCode": "200"},
		"Tags":                  []any{map[string]any{"Key": "k", "Value": "v"}},
		"TargetGroupAttributes": []any{map[string]any{"Key": "stickiness.enabled", "Value": "false"}},
		"Targets": []any{
			map[string]any{"Id": "i-1", "Port": json.Number("80")},
			map[string]any{"Id": "i-2", "Port": json.Number("81")},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant   %#v", got, want)
	}
	id := "arn:aws:elasticloadbalancing:us-east-1:1:targetgroup/tg/abc"
	wantForms := []url.Values{
		{"Action": {"DescribeTargetGroups"}, "Version": {"2015-12-01"}, "TargetGroupArns.member.1": {id}},
		{"Action": {"DescribeTags"}, "Version": {"2015-12-01"}, "ResourceArns.member.1": {id}},
		{"Action": {"DescribeTargetGroupAttributes"}, "Version": {"2015-12-01"}, "TargetGroupArn": {id}},
		{"Action": {"DescribeTargetHealth"}, "Version": {"2015-12-01"}, "TargetGroupArn": {id}},
	}
	// The read comes first; the further calls are made together, in no
	// particular order.
	sort.Slice(forms[1:], func(i, j int) bool { return forms[1+i].Get("Action") < forms[1+j].Get("Action") })
	if !reflect.DeepEqual(forms, wantForms) {
		t.Fatalf("forms = %v\nwant    %v", forms, wantForms)
	}
}

// A read answered with anything but exactly one instance fails: none is
// absent, and two would be one read standing in for another.
func TestReadXMLWantsExactlyOne(t *testing.T) {
	for name, body := range map[string]string{
		"none":    `<DescribeSubnetsResponse><subnetSet/></DescribeSubnetsResponse>`,
		"two":     `<DescribeSubnetsResponse><subnetSet><item><subnetId>a</subnetId></item><item><subnetId>b</subnetId></item></subnetSet></DescribeSubnetsResponse>`,
		"no list": `<DescribeSubnetsResponse/>`,
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := xmlServer(t, 200, body)
			if got, err := client.Read(context.Background(), subnets, map[string]string{"SubnetId": "a"}); err == nil {
				t.Fatalf("Read = %v, want an error", got)
			}
		})
	}
	client, _ := xmlServer(t, 200, `<DescribeTargetGroupsResponse><Other/></DescribeTargetGroupsResponse>`)
	if _, err := client.Read(context.Background(), targetGroups, map[string]string{"TargetGroupArn": "a"}); err == nil || !strings.Contains(err.Error(), "no DescribeTargetGroupsResult") {
		t.Fatalf("Read without the wrapper = %v", err)
	}
}

func TestReadXMLErrors(t *testing.T) {
	cases := map[string]struct {
		typeName, body, code string
	}{
		"ec2Query": {subnets, `<Response><Errors><Error><Code>InvalidSubnetID.NotFound</Code><Message>gone</Message></Error></Errors><RequestID>r</RequestID></Response>`, "InvalidSubnetID.NotFound"},
		"awsQuery": {targetGroups, `<ErrorResponse><Error><Type>Sender</Type><Code>TargetGroupNotFound</Code><Message>gone</Message></Error></ErrorResponse>`, "TargetGroupNotFound"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			client, _ := xmlServer(t, 400, c.body)
			_, err := client.Read(context.Background(), c.typeName, map[string]string{"SubnetId": "a", "TargetGroupArn": "a"})
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Code != c.code || apiErr.Message != "gone" || apiErr.Status != 400 {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestTranslateXML(t *testing.T) {
	root, err := parseXML([]byte(`<r>
		<flat>a</flat><flat>b</flat>
		<attrs><entry><key>k</key><value>7</value></entry></attrs>
		<n>not-a-number</n><b>yes</b>
	</r>`))
	if err != nil {
		t.Fatal(err)
	}
	got := translateXML(&walk{}, root, []Field{
		{Property: "Flat", Kind: "list", XMLName: "flat", Scalar: "string"},
		{Property: "Attrs", Kind: "map", XMLName: "attrs", Scalar: "number"},
		{Property: "N", Kind: "scalar", XMLName: "n", Scalar: "number"},
		{Property: "B", Kind: "scalar", XMLName: "b", Scalar: "boolean"},
		{Property: "Missing", Kind: "scalar", XMLName: "missing", Scalar: "string"},
		{Property: "MissingList", Kind: "list", XMLName: "missingList", Item: "member", Scalar: "string"},
	})
	want := map[string]any{
		"Flat":  []any{"a", "b"},
		"Attrs": map[string]any{"k": json.Number("7")},
		// Text that is not what the model says stays text, for the
		// comparison to report.
		"N": "not-a-number", "B": "yes",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("translateXML = %#v\nwant           %#v", got, want)
	}
}

func TestCompileQueryKeys(t *testing.T) {
	str := func(traits map[string]any) map[string]any {
		return map[string]any{"target": "com.example#Ids", "traits": traits}
	}
	cases := map[string]struct {
		protocol string
		member   map[string]any
		list     map[string]any
		want     string
	}{
		"awsQuery list":               {"awsQuery", str(nil), nil, "Ids.member.1"},
		"awsQuery list with xmlName":  {"awsQuery", str(map[string]any{"smithy.api#xmlName": "IdList"}), map[string]any{"smithy.api#xmlName": "Id"}, "IdList.Id.1"},
		"awsQuery flattened list":     {"awsQuery", str(map[string]any{"smithy.api#xmlFlattened": map[string]any{}}), nil, "Ids.1"},
		"ec2Query list":               {"ec2Query", str(nil), nil, "Ids.1"},
		"ec2Query list, xmlName":      {"ec2Query", str(map[string]any{"smithy.api#xmlName": "id"}), nil, "Id.1"},
		"ec2Query list, ec2QueryName": {"ec2Query", str(map[string]any{"smithy.api#xmlName": "id", "aws.protocols#ec2QueryName": "IdSet"}), nil, "IdSet.1"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			el := map[string]any{"target": "smithy.api#String"}
			if c.list != nil {
				el["traits"] = c.list
			}
			raw, _ := json.Marshal(map[string]any{"shapes": map[string]any{"com.example#Ids": map[string]any{"type": "list", "member": el}}})
			var model smithyModel
			if err := json.Unmarshal(raw, &model); err != nil {
				t.Fatal(err)
			}
			rawMember, _ := json.Marshal(c.member)
			var m smithyMember
			if err := json.Unmarshal(rawMember, &m); err != nil {
				t.Fatal(err)
			}
			if got := queryKey(&model, c.protocol, "Ids", m); got != c.want {
				t.Fatalf("queryKey = %q, want %q", got, c.want)
			}
		})
	}
}

func TestCompileRefusesUnderXML(t *testing.T) {
	const subnet = "AWS--EC2--Subnet.yaml"
	cases := map[string]struct {
		old, replacement, want string
	}{
		"a list step on a structure": {"response: Subnets[]", "response: Subnets[].Tags[].Key[]", "is not a list"},
		"a list":                     {"response: Subnets[]", "response: Subnets[]\nlist:\n  operation: DescribeSubnets", "a list under ec2Query is not supported yet"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := compileAll(edit(t, subnet, c.old, c.replacement))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("compile = %v\nwant an error containing %q", err, c.want)
			}
		})
	}
}

const cachePolicies = "AWS::CloudFront::CachePolicy"

const cachePolicyXML = `<?xml version="1.0"?>
<CachePolicy xmlns="http://cloudfront.amazonaws.com/doc/2020-05-31/">
  <Id>cp-1</Id>
  <LastModifiedTime>1970-01-01T00:00:00Z</LastModifiedTime>
  <CachePolicyConfig>
    <Name>Managed-CachingOptimized</Name>
    <MinTTL>1</MinTTL>
    <ParametersInCacheKeyAndForwardedToOrigin>
      <EnableAcceptEncodingGzip>true</EnableAcceptEncodingGzip>
      <HeadersConfig>
        <HeaderBehavior>whitelist</HeaderBehavior>
        <Headers><Quantity>2</Quantity><Items><Name>Host</Name><Name>Origin</Name></Items></Headers>
      </HeadersConfig>
      <CookiesConfig><CookieBehavior>none</CookieBehavior><Cookies><Quantity>0</Quantity></Cookies></CookiesConfig>
    </ParametersInCacheKeyAndForwardedToOrigin>
  </CachePolicyConfig>
</CachePolicy>`

// restXml: a GET with the identifier in the path, the body the payload
// itself, and lists read through their Quantity and Items wrapper.
func TestReadRestXML(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.EscapedPath()+" body="+string(raw))
		_, _ = io.WriteString(w, cachePolicyXML)
	}))
	t.Cleanup(srv.Close)
	client := &Client{
		HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL },
		Now: func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) },
	}
	read, err := client.Read(context.Background(), cachePolicies, map[string]string{"Id": "cp/1"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"Id": "cp-1", "LastModifiedTime": "1970-01-01T00:00:00Z",
		"CachePolicyConfig": map[string]any{
			"Name": "Managed-CachingOptimized", "MinTTL": json.Number("1"),
			"ParametersInCacheKeyAndForwardedToOrigin": map[string]any{
				"EnableAcceptEncodingGzip": true,
				"HeadersConfig":            map[string]any{"HeaderBehavior": "whitelist", "Headers": []any{"Host", "Origin"}},
				"CookiesConfig":            map[string]any{"CookieBehavior": "none"},
			},
		},
	}
	if !reflect.DeepEqual(read, want) {
		t.Fatalf("Read = %#v\nwant   %#v", read, want)
	}
	if want := []string{"GET /2020-05-31/cache-policy/cp%2F1 body="}; !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

func TestReadRestXMLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `<ErrorResponse><Error><Type>Sender</Type><Code>NoSuchCachePolicy</Code><Message>gone</Message></Error></ErrorResponse>`)
	}))
	t.Cleanup(srv.Close)
	client := &Client{
		HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL },
	}
	_, err := client.Read(context.Background(), cachePolicies, map[string]string{"Id": "x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "NoSuchCachePolicy" || apiErr.Status != 404 {
		t.Fatalf("error = %#v", err)
	}
}

func TestCompileRefusesAMemberPath(t *testing.T) {
	const file = "AWS--CloudFront--CachePolicy.yaml"
	cases := map[string]struct {
		old, replacement, want string
	}{
		"a step that is not a member":     {"Headers: Headers.Items", "Headers: Nope.Items", "Nope is not a structure member"},
		"a step that is not a structure":  {"Headers: Headers.Items", "Headers: HeaderBehavior.Items", "HeaderBehavior is not a structure member"},
		"a last step the structure lacks": {"Headers: Headers.Items", "Headers: Headers.Names", "maps to Headers.Names, which"},
		"a path to the wrong type":        {"Headers: Headers.Items", "Headers: Headers.Quantity", "Headers is [array] in the schema, but Headers.Quantity is integer"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := compileAll(edit(t, file, c.old, c.replacement))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("compile = %v\nwant an error containing %q", err, c.want)
			}
		})
	}
}

// Under a JSON protocol a member path is walked through the objects too.
func TestTranslateAMemberPath(t *testing.T) {
	r := Reader{Protocol: "restJson1"}
	got := r.translate(&walk{}, map[string]any{"Headers": map[string]any{"Quantity": 1, "Items": []any{"Host"}}}, []Field{
		{Property: "Headers", Member: "Items", Kind: "list", Via: []Step{{Name: "Headers"}}},
		{Property: "Absent", Member: "Items", Kind: "list", Via: []Step{{Name: "Nothing"}}},
	})
	if !reflect.DeepEqual(got, map[string]any{"Headers": []any{"Host"}}) {
		t.Fatalf("translate = %v", got)
	}
}

// ec2Query: a security group's rules come from one list, split by
// direction, with a referenced group read through its nested structure.
func TestReadSecurityGroupSplitsRulesByDirection(t *testing.T) {
	client, forms := xmlServerBy(t, map[string]string{
		"DescribeSecurityGroups": `<DescribeSecurityGroupsResponse><securityGroupInfo><item>
<groupId>sg-1</groupId><groupName>web</groupName><groupDescription>d</groupDescription><vpcId>vpc-1</vpcId>
</item></securityGroupInfo></DescribeSecurityGroupsResponse>`,
		"DescribeSecurityGroupRules": `<DescribeSecurityGroupRulesResponse><securityGroupRuleSet>
<item><isEgress>false</isEgress><ipProtocol>tcp</ipProtocol><fromPort>443</fromPort><toPort>443</toPort><cidrIpv6>::/0</cidrIpv6><description>https</description></item>
<item><isEgress>true</isEgress><ipProtocol>-1</ipProtocol><fromPort>-1</fromPort><toPort>-1</toPort><cidrIpv4>0.0.0.0/0</cidrIpv4></item>
<item><isEgress>false</isEgress><ipProtocol>-1</ipProtocol><fromPort>-1</fromPort><toPort>-1</toPort><referencedGroupInfo><groupId>sg-2</groupId><userId>123</userId></referencedGroupInfo></item>
</securityGroupRuleSet></DescribeSecurityGroupRulesResponse>`,
	})
	got, err := client.Read(context.Background(), "AWS::EC2::SecurityGroup", map[string]string{"Id": "sg-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"Id": "sg-1", "GroupId": "sg-1", "GroupName": "web", "GroupDescription": "d", "VpcId": "vpc-1",
		"SecurityGroupIngress": []any{
			map[string]any{"IpProtocol": "tcp", "FromPort": json.Number("443"), "ToPort": json.Number("443"), "CidrIpv6": "::/0", "Description": "https"},
			map[string]any{"IpProtocol": "-1", "FromPort": json.Number("-1"), "ToPort": json.Number("-1"), "SourceSecurityGroupId": "sg-2", "SourceSecurityGroupOwnerId": "123"},
		},
		"SecurityGroupEgress": []any{
			map[string]any{"IpProtocol": "-1", "FromPort": json.Number("-1"), "ToPort": json.Number("-1"), "CidrIp": "0.0.0.0/0"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant   %#v", got, want)
	}
	if f := (*forms)[1]; f.Get("Filter.1.Name") != "group-id" || f.Get("Filter.1.Value.1") != "sg-1" {
		t.Fatalf("rules request = %v", f)
	}
}
