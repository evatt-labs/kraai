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
	subnets = "AWS::EC2::Subnet"
	roles   = "AWS::IAM::Role"
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
		RetryDelay:  time.Millisecond,
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
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, RetryDelay: time.Millisecond,
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

const roleXML = `<GetRoleResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/">
  <GetRoleResult>
    <Role>
      <Path>/</Path>
      <RoleName>kraai-role</RoleName>
      <RoleId>AROAEXAMPLE</RoleId>
      <Arn>arn:aws:iam::123456789012:role/kraai-role</Arn>
      <MaxSessionDuration>3600</MaxSessionDuration>
      <Description>a &amp; b</Description>
      <AssumeRolePolicyDocument>%7B%22Version%22%3A%222012-10-17%22%7D</AssumeRolePolicyDocument>
      <PermissionsBoundary><PermissionsBoundaryType>Policy</PermissionsBoundaryType><PermissionsBoundaryArn>arn:aws:iam::123456789012:policy/pb</PermissionsBoundaryArn></PermissionsBoundary>
      <RoleLastUsed><Region>us-east-1</Region></RoleLastUsed>
    </Role>
  </GetRoleResult>
  <ResponseMetadata><RequestId>r</RequestId></ResponseMetadata>
</GetRoleResponse>`

// awsQuery: the output inside its Result wrapper, numbers typed, a list of
// scalars, a path through a nested structure, text decoded, and a call made
// once for each element a list read returned.
func TestReadAWSQuery(t *testing.T) {
	byAction := map[string]string{
		"GetRole": roleXML,
		"ListAttachedRolePolicies": `<ListAttachedRolePoliciesResponse><ListAttachedRolePoliciesResult><AttachedPolicies>
			<member><PolicyName>a</PolicyName><PolicyArn>arn:aws:iam::aws:policy/A</PolicyArn></member>
			<member><PolicyName>b</PolicyName><PolicyArn>arn:aws:iam::aws:policy/B</PolicyArn></member>
		</AttachedPolicies><IsTruncated>false</IsTruncated></ListAttachedRolePoliciesResult></ListAttachedRolePoliciesResponse>`,
		"ListRolePolicies": `<ListRolePoliciesResponse><ListRolePoliciesResult><PolicyNames>
			<member>inline-1</member><member>inline-2</member>
		</PolicyNames><IsTruncated>false</IsTruncated></ListRolePoliciesResult></ListRolePoliciesResponse>`,
		"GetRolePolicy": `<GetRolePolicyResponse><GetRolePolicyResult><RoleName>kraai-role</RoleName><PolicyName>x</PolicyName>
			<PolicyDocument>%7B%22Version%22%3A%222012-10-17%22%7D</PolicyDocument></GetRolePolicyResult></GetRolePolicyResponse>`,
	}
	client, forms := xmlServerBy(t, byAction)
	got, err := client.Read(context.Background(), roles, map[string]string{"RoleName": "kraai-role"})
	if err != nil {
		t.Fatal(err)
	}
	policy := map[string]any{"Version": "2012-10-17"}
	want := map[string]any{
		"Arn": "arn:aws:iam::123456789012:role/kraai-role", "RoleName": "kraai-role", "RoleId": "AROAEXAMPLE", "Path": "/",
		"Description": "a & b", "MaxSessionDuration": json.Number("3600"), "AssumeRolePolicyDocument": policy,
		"PermissionsBoundary": "arn:aws:iam::123456789012:policy/pb",
		"ManagedPolicyArns":   []any{"arn:aws:iam::aws:policy/A", "arn:aws:iam::aws:policy/B"},
		"Policies": []any{
			map[string]any{"PolicyName": "inline-1", "PolicyDocument": policy},
			map[string]any{"PolicyName": "inline-2", "PolicyDocument": policy},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant   %#v", got, want)
	}
	const v = "2010-05-08"
	wantForms := []url.Values{
		{"Action": {"GetRole"}, "Version": {v}, "RoleName": {"kraai-role"}},
		{"Action": {"GetRolePolicy"}, "Version": {v}, "RoleName": {"kraai-role"}, "PolicyName": {"inline-1"}},
		{"Action": {"GetRolePolicy"}, "Version": {v}, "RoleName": {"kraai-role"}, "PolicyName": {"inline-2"}},
		{"Action": {"ListAttachedRolePolicies"}, "Version": {v}, "RoleName": {"kraai-role"}},
		{"Action": {"ListRolePolicies"}, "Version": {v}, "RoleName": {"kraai-role"}},
	}
	// The read comes first; the further calls are made together, in no
	// particular order.
	rest := (*forms)[1:]
	sort.Slice(rest, func(i, j int) bool {
		if a, b := rest[i].Get("Action"), rest[j].Get("Action"); a != b {
			return a < b
		}
		return rest[i].Get("PolicyName") < rest[j].Get("PolicyName")
	})
	if !reflect.DeepEqual(*forms, wantForms) {
		t.Fatalf("forms = %v\nwant    %v", *forms, wantForms)
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
	client, _ := xmlServer(t, 200, `<GetRoleResponse><Other/></GetRoleResponse>`)
	if _, err := client.Read(context.Background(), roles, map[string]string{"RoleName": "a"}); err == nil || !strings.Contains(err.Error(), "no GetRoleResult") {
		t.Fatalf("Read without the wrapper = %v", err)
	}
}

func TestReadXMLErrors(t *testing.T) {
	cases := map[string]struct {
		typeName, body, code string
	}{
		"ec2Query": {subnets, `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>gone</Message></Error></Errors><RequestID>r</RequestID></Response>`, "UnauthorizedOperation"},
		"awsQuery": {roles, `<ErrorResponse><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>gone</Message></Error></ErrorResponse>`, "AccessDenied"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			client, _ := xmlServer(t, 400, c.body)
			_, err := client.Read(context.Background(), c.typeName, map[string]string{"SubnetId": "a", "RoleName": "a"})
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Code != c.code || apiErr.Message != "gone" || apiErr.Status != 400 {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

// An error code the override declares absent is absence; any other code
// stays an error, so a lookup falls back rather than reading it as gone.
func TestReadAbsentErrorIsAbsence(t *testing.T) {
	for name, c := range map[string]struct {
		body string
		want error
	}{
		"declared": {`<Response><Errors><Error><Code>InvalidSubnetID.NotFound</Code><Message>gone</Message></Error></Errors></Response>`, ErrAbsent},
		"another":  {`<Response><Errors><Error><Code>InvalidSubnetID.Malformed</Code><Message>bad</Message></Error></Errors></Response>`, nil},
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := xmlServer(t, 400, c.body)
			_, err := client.Read(context.Background(), subnets, map[string]string{"SubnetId": "subnet-1"})
			if c.want != nil && !errors.Is(err, c.want) || c.want == nil && (err == nil || errors.Is(err, ErrAbsent)) {
				t.Fatalf("Read = %v, want %v", err, c.want)
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

const cachePolicies = "Test::RestXML::CachePolicy"

// registerCachePolicy registers a restXml reader shaped as CloudFront's
// cache policy read is: the identifier in the path, the payload the body
// itself, and lists wrapped in a Quantity and Items structure. No kept type
// reads that shape; S3 answers a bare payload with flattened lists.
func registerCachePolicy(t *testing.T) {
	t.Helper()
	str := func(property, name string) Field {
		return Field{Property: property, Member: name, Kind: "scalar", XMLName: name, Scalar: "string"}
	}
	readers[cachePolicies] = Reader{
		Type: cachePolicies, Protocol: "restXml", SigningName: "cloudfront", Host: "cloudfront.{region}.amazonaws.com",
		Method: "GET", URI: "/2020-05-31/cache-policy/{Id}",
		Identifier:   []Binding{{Property: "Id", Member: "Id", Location: "label"}},
		AbsentErrors: []string{"NoSuchCachePolicy"},
		Fields: []Field{
			str("Id", "Id"),
			{Property: "LastModifiedTime", Member: "LastModifiedTime", Kind: "timestamp", XMLName: "LastModifiedTime", Scalar: "timestamp"},
			{Property: "CachePolicyConfig", Member: "CachePolicyConfig", Kind: "structure", XMLName: "CachePolicyConfig", Fields: []Field{
				str("Name", "Name"),
				{Property: "MinTTL", Member: "MinTTL", Kind: "scalar", XMLName: "MinTTL", Scalar: "number"},
				{Property: "ParametersInCacheKeyAndForwardedToOrigin", Member: "ParametersInCacheKeyAndForwardedToOrigin", Kind: "structure",
					XMLName: "ParametersInCacheKeyAndForwardedToOrigin", Fields: []Field{
						{Property: "EnableAcceptEncodingGzip", Member: "EnableAcceptEncodingGzip", Kind: "scalar", XMLName: "EnableAcceptEncodingGzip", Scalar: "boolean"},
						{Property: "HeadersConfig", Member: "HeadersConfig", Kind: "structure", XMLName: "HeadersConfig", Fields: []Field{
							str("HeaderBehavior", "HeaderBehavior"),
							{Property: "Headers", Member: "Items", Kind: "list", Via: []Step{{Name: "Headers"}}, XMLName: "Items", Item: "Name", Scalar: "string"},
						}},
						{Property: "CookiesConfig", Member: "CookiesConfig", Kind: "structure", XMLName: "CookiesConfig", Fields: []Field{
							str("CookieBehavior", "CookieBehavior"),
							{Property: "Cookies", Member: "Items", Kind: "list", Via: []Step{{Name: "Cookies"}}, XMLName: "Items", Item: "Name", Scalar: "string"},
						}},
					}},
			}},
		},
	}
	t.Cleanup(func() { delete(readers, cachePolicies) })
}

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
	registerCachePolicy(t)
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
	registerCachePolicy(t)
	serve := func(status int, code string) *Client {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `<ErrorResponse><Error><Type>Sender</Type><Code>`+code+`</Code><Message>m</Message></Error></ErrorResponse>`)
		}))
		t.Cleanup(srv.Close)
		return &Client{
			HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
			Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, RetryDelay: time.Millisecond,
		}
	}
	_, err := serve(403, "AccessDenied").Read(context.Background(), cachePolicies, map[string]string{"Id": "x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "AccessDenied" || apiErr.Status != 403 {
		t.Fatalf("error = %#v", err)
	}
	// The code the read lists for a policy that does not exist is absence.
	if _, err := serve(404, "NoSuchCachePolicy").Read(context.Background(), cachePolicies, map[string]string{"Id": "x"}); !errors.Is(err, ErrAbsent) {
		t.Fatalf("a missing policy read %v, want absent", err)
	}
}

func TestCompileRefusesAMemberPath(t *testing.T) {
	const file = "AWS--IAM--Role.yaml"
	const path = "PermissionsBoundary: PermissionsBoundary.PermissionsBoundaryArn"
	cases := map[string]struct {
		old, replacement, want string
	}{
		"a step that is not a member":     {path, "PermissionsBoundary: Nope.PermissionsBoundaryArn", "Nope is not a structure member"},
		"a step that is not a structure":  {path, "PermissionsBoundary: Description.PermissionsBoundaryArn", "Description is not a structure member"},
		"a last step the structure lacks": {path, "PermissionsBoundary: PermissionsBoundary.Nope", "maps to PermissionsBoundary.Nope, which"},
		"a path to the wrong type":        {path, "PermissionsBoundary: MaxSessionDuration", "PermissionsBoundary is [string] in the schema, but MaxSessionDuration is integer"},
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

// A VPC reads each DNS attribute in its own call to the same operation, told
// apart by a fixed input, alongside filtered calls for its default network
// ACL and security group.
func TestReadVPCAttributesByFixedInput(t *testing.T) {
	byKey := map[string]string{
		"DescribeVpcs": `<DescribeVpcsResponse><vpcSet><item><vpcId>vpc-1</vpcId><cidrBlock>10.0.0.0/16</cidrBlock>
<cidrBlockAssociationSet><item><associationId>assoc-4</associationId></item></cidrBlockAssociationSet>
<ipv6CidrBlockAssociationSet><item><ipv6CidrBlock>2600::/56</ipv6CidrBlock></item></ipv6CidrBlockAssociationSet>
</item></vpcSet></DescribeVpcsResponse>`,
		"DescribeNetworkAcls":                     `<DescribeNetworkAclsResponse><networkAclSet><item><networkAclId>acl-1</networkAclId></item></networkAclSet></DescribeNetworkAclsResponse>`,
		"DescribeSecurityGroups":                  `<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>sg-1</groupId></item></securityGroupInfo></DescribeSecurityGroupsResponse>`,
		"DescribeVpcAttribute/enableDnsSupport":   `<DescribeVpcAttributeResponse><enableDnsSupport><value>true</value></enableDnsSupport></DescribeVpcAttributeResponse>`,
		"DescribeVpcAttribute/enableDnsHostnames": `<DescribeVpcAttributeResponse><enableDnsHostnames><value>false</value></enableDnsHostnames></DescribeVpcAttributeResponse>`,
	}
	var mu sync.Mutex
	var forms []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		mu.Lock()
		forms = append(forms, form)
		mu.Unlock()
		key := form.Get("Action")
		if a := form.Get("Attribute"); a != "" {
			key += "/" + a
		}
		_, _ = io.WriteString(w, byKey[key])
	}))
	t.Cleanup(srv.Close)
	client := &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }}

	got, err := client.Read(context.Background(), "AWS::EC2::VPC", map[string]string{"VpcId": "vpc-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"VpcId": "vpc-1", "CidrBlock": "10.0.0.0/16", "CidrBlockAssociations": []any{"assoc-4"}, "Ipv6CidrBlocks": []any{"2600::/56"},
		"DefaultNetworkAcl": "acl-1", "DefaultSecurityGroup": "sg-1", "EnableDnsSupport": true, "EnableDnsHostnames": false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant   %#v", got, want)
	}
	var acl url.Values
	for _, f := range forms {
		if f.Get("Action") == "DescribeNetworkAcls" {
			acl = f
		}
	}
	if acl.Get("Filter.1.Value.1") != "vpc-1" || acl.Get("Filter.2.Name") != "default" || acl.Get("Filter.2.Value.1") != "true" {
		t.Fatalf("network ACL request = %v", acl)
	}
}
