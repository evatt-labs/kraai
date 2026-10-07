package direct

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const securityGroupType = "AWS::EC2::SecurityGroup"

// sgRule is one rule as EC2 keeps it: one source or destination each.
type sgRule struct {
	egress                                bool
	proto, from, to                       string
	cidr4, cidr6, prefixList, group, user string
	desc                                  string
}

// same is whether two rules are one: EC2 compares everything but the
// description.
func (r sgRule) same(o sgRule) bool {
	r.desc, o.desc = "", ""
	return r == o
}

// fakeSecurityGroups is one security group, answered as EC2 does: a create
// in a VPC holds an allow-all egress rule, an authorize of a rule already
// there is refused, and a revoke matches a rule whatever its description.
type fakeSecurityGroups struct {
	mu     sync.Mutex
	exists bool
	name   string
	rules  []sgRule
	tags   []any
	calls  map[string][]url.Values
}

// formRules reads the rules an IpPermissions form sends, one per range.
func formRules(form url.Values, egress bool) []sgRule {
	var out []sgRule
	for i := 1; form.Has(fmt.Sprintf("IpPermissions.%d.IpProtocol", i)); i++ {
		p := fmt.Sprintf("IpPermissions.%d.", i)
		base := sgRule{egress: egress, proto: form.Get(p + "IpProtocol"), from: form.Get(p + "FromPort"), to: form.Get(p + "ToPort")}
		if base.proto == "-1" {
			base.from, base.to = "-1", "-1"
		}
		for _, r := range []struct{ list, member string }{{"IpRanges", "CidrIp"}, {"Ipv6Ranges", "CidrIpv6"}, {"PrefixListIds", "PrefixListId"}, {"Groups", "GroupId"}} {
			for j := 1; form.Has(fmt.Sprintf("%s%s.%d.%s", p, r.list, j, r.member)); j++ {
				q := fmt.Sprintf("%s%s.%d.", p, r.list, j)
				rule := base
				rule.desc = form.Get(q + "Description")
				switch r.list {
				case "IpRanges":
					rule.cidr4 = form.Get(q + r.member)
				case "Ipv6Ranges":
					rule.cidr6 = form.Get(q + r.member)
				case "PrefixListIds":
					rule.prefixList = form.Get(q + r.member)
				case "Groups":
					rule.group, rule.user = form.Get(q+r.member), "111"
				}
				out = append(out, rule)
			}
		}
	}
	return out
}

func (f *fakeSecurityGroups) serve(t *testing.T) *Client {
	t.Helper()
	f.calls = map[string][]url.Values{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		f.mu.Lock()
		defer f.mu.Unlock()
		op := form.Get("Action")
		f.calls[op] = append(f.calls[op], form)
		refuse := func(code string) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `<Response><Errors><Error><Code>`+code+`</Code><Message>refused</Message></Error></Errors></Response>`)
		}
		if !f.exists && op != "CreateSecurityGroup" {
			refuse("InvalidGroup.NotFound")
			return
		}
		egress := strings.HasSuffix(op, "Egress")
		switch op {
		case "CreateSecurityGroup":
			f.exists, f.name = true, form.Get("GroupName")
			f.tags = formTagList(form, "TagSpecification.1.Tag")
			f.rules = []sgRule{{egress: true, proto: "-1", from: "-1", to: "-1", cidr4: "0.0.0.0/0"}}
			_, _ = io.WriteString(w, `<CreateSecurityGroupResponse><return>true</return><groupId>sg-1</groupId></CreateSecurityGroupResponse>`)
		case "AuthorizeSecurityGroupIngress", "AuthorizeSecurityGroupEgress":
			for _, rule := range formRules(form, egress) {
				if slices.ContainsFunc(f.rules, rule.same) {
					refuse("InvalidPermission.Duplicate")
					return
				}
				f.rules = append(f.rules, rule)
			}
			_, _ = io.WriteString(w, `<Response><return>true</return></Response>`)
		case "RevokeSecurityGroupIngress", "RevokeSecurityGroupEgress":
			for _, rule := range formRules(form, egress) {
				f.rules = slices.DeleteFunc(f.rules, rule.same)
			}
			_, _ = io.WriteString(w, `<Response><return>true</return></Response>`)
		case "UpdateSecurityGroupRuleDescriptionsIngress", "UpdateSecurityGroupRuleDescriptionsEgress":
			for _, rule := range formRules(form, egress) {
				for i := range f.rules {
					if f.rules[i].same(rule) {
						f.rules[i].desc = rule.desc
					}
				}
			}
			_, _ = io.WriteString(w, `<Response><return>true</return></Response>`)
		case "CreateTags":
			for _, tag := range formTagList(form, "Tag") {
				key := tag.(map[string]any)["Key"]
				f.tags = slices.DeleteFunc(f.tags, func(have any) bool { return have.(map[string]any)["Key"] == key })
				f.tags = append(f.tags, tag)
			}
			_, _ = io.WriteString(w, `<Response><return>true</return></Response>`)
		case "DeleteTags":
			for _, tag := range formTagList(form, "Tag") {
				key := tag.(map[string]any)["Key"]
				f.tags = slices.DeleteFunc(f.tags, func(have any) bool { return have.(map[string]any)["Key"] == key })
			}
			_, _ = io.WriteString(w, `<Response><return>true</return></Response>`)
		case "DeleteSecurityGroup":
			f.exists = false
			_, _ = io.WriteString(w, `<Response><return>true</return></Response>`)
		case "DescribeSecurityGroups":
			_, _ = io.WriteString(w, `<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>sg-1</groupId><groupName>`+
				f.name+`</groupName><groupDescription>d</groupDescription><vpcId>vpc-1</vpcId>`+ec2TagSet(f.tags)+`</item></securityGroupInfo></DescribeSecurityGroupsResponse>`)
		case "DescribeSecurityGroupRules":
			var b strings.Builder
			for _, rule := range f.rules {
				b.WriteString("<item><isEgress>" + strconv.FormatBool(rule.egress) + "</isEgress><ipProtocol>" + rule.proto + "</ipProtocol>")
				b.WriteString("<fromPort>" + rule.from + "</fromPort><toPort>" + rule.to + "</toPort>")
				for _, e := range [][2]string{{"cidrIpv4", rule.cidr4}, {"cidrIpv6", rule.cidr6}, {"prefixListId", rule.prefixList}, {"description", rule.desc}} {
					if e[1] != "" {
						b.WriteString("<" + e[0] + ">" + e[1] + "</" + e[0] + ">")
					}
				}
				if rule.group != "" {
					b.WriteString("<referencedGroupInfo><groupId>" + rule.group + "</groupId><userId>" + rule.user + "</userId></referencedGroupInfo>")
				}
				b.WriteString("</item>")
			}
			_, _ = io.WriteString(w, `<DescribeSecurityGroupRulesResponse><securityGroupRuleSet>`+b.String()+`</securityGroupRuleSet></DescribeSecurityGroupRulesResponse>`)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: time.Second, Poll: time.Millisecond}
}

// ec2TagSet is a <tagSet> of Key/Value tags, as EC2 answers them.
func ec2TagSet(tags []any) string {
	var b strings.Builder
	for _, tag := range tags {
		m := tag.(map[string]any)
		b.WriteString("<item><key>" + m["Key"].(string) + "</key><value>" + m["Value"].(string) + "</value></item>")
	}
	return "<tagSet>" + b.String() + "</tagSet>"
}

func rule(members ...any) map[string]any {
	out := map[string]any{}
	for pair := range slices.Chunk(members, 2) {
		out[pair[0].(string)] = pair[1]
	}
	return out
}

var ingressMatch = []string{"IpProtocol", "FromPort", "ToPort", "CidrIp", "CidrIpv6", "SourcePrefixListId", "SourceSecurityGroupId", "SourceSecurityGroupOwnerId"}

// What the read fills in does not unpair a rule: an all-protocol rule's
// port range, and a source group's owner.
func TestMatchChangesPairsDespiteFilledMembers(t *testing.T) {
	current := []any{
		rule("IpProtocol", "-1", "FromPort", -1, "ToPort", -1, "CidrIp", "10.0.0.0/24"),
		rule("IpProtocol", "tcp", "FromPort", 80, "ToPort", 80, "SourceSecurityGroupId", "sg-2", "SourceSecurityGroupOwnerId", "111"),
	}
	desired := []any{
		rule("IpProtocol", "-1", "CidrIp", "10.0.0.0/24"),
		rule("IpProtocol", "tcp", "FromPort", 80, "ToPort", 80, "SourceSecurityGroupId", "sg-2"),
	}
	added, removed, changed, err := matchChanges(ingressMatch, current, desired)
	if err != nil || added != nil || removed != nil || changed != nil {
		t.Fatalf("added %v, removed %v, changed %v, %v; want nothing", added, removed, changed, err)
	}
}

// A description is not what a rule is: a new one pairs as changed, and a
// rule that differs in a match member is another rule.
func TestMatchChangesDescriptionIsAChange(t *testing.T) {
	current := []any{rule("IpProtocol", "tcp", "FromPort", 443, "ToPort", 443, "CidrIp", "10.0.0.0/16", "Description", "old")}
	desired := []any{
		rule("IpProtocol", "tcp", "FromPort", 443, "ToPort", 443, "CidrIp", "10.0.0.0/16", "Description", "new"),
		rule("IpProtocol", "tcp", "FromPort", 443, "ToPort", 443, "CidrIpv6", "::/0"),
	}
	added, removed, changed, err := matchChanges(ingressMatch, current, desired)
	if err != nil || len(added) != 1 || removed != nil || len(changed) != 1 || changed[0][0].(map[string]any)["Description"] != "new" {
		t.Fatalf("added %v, removed %v, changed %v, %v", added, removed, changed, err)
	}
	if _, _, _, err := matchChanges(ingressMatch, nil, []any{desired[1], desired[1]}); err == nil {
		t.Fatal("two copies of one rule were accepted")
	}
}

// A greedy pairing would give the looser desired rule the only current rule
// the stricter one can take, and report a rule added and one removed.
func TestMatchChangesFindsTheMaximumPairing(t *testing.T) {
	current := []any{
		rule("IpProtocol", "tcp", "FromPort", 1, "ToPort", 1, "CidrIp", "10.0.0.0/8"),
		rule("IpProtocol", "tcp", "FromPort", 2, "ToPort", 2, "CidrIp", "10.0.0.0/8"),
	}
	desired := []any{
		rule("IpProtocol", "tcp", "CidrIp", "10.0.0.0/8"),
		rule("IpProtocol", "tcp", "FromPort", 1, "ToPort", 1, "CidrIp", "10.0.0.0/8"),
	}
	added, removed, _, err := matchChanges(ingressMatch, current, desired)
	if err != nil || added != nil || removed != nil {
		t.Fatalf("added %v, removed %v, %v", added, removed, err)
	}
}

// An IPv6 rule is sent in the one range list its source belongs in, its
// description with it and nowhere else.
func TestElementSendsOneRangeList(t *testing.T) {
	r := readers[securityGroupType]
	i := slices.IndexFunc(r.Update, func(u MutationCall) bool { return u.ListProperty == "SecurityGroupIngress" })
	got, err := shaped(r.Update[i], []any{rule("IpProtocol", "tcp", "FromPort", 443, "ToPort", 443, "CidrIpv6", "::/0", "Description", "d")})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{map[string]any{"IpProtocol": "tcp", "FromPort": json.Number("443"), "ToPort": json.Number("443"),
		"Ipv6Ranges": []any{map[string]any{"CidrIpv6": "::/0", "Description": "d"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shaped = %#v\nwant %#v", got, want)
	}
}

// A desired egress list revokes the default allow-all rule a create in a
// VPC holds; a group whose manifest sets no egress keeps it.
func TestCreateSecurityGroupEgress(t *testing.T) {
	name := map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-sg"}
	for _, c := range []struct {
		egress  []any
		revoked int
		kept    bool
	}{
		{[]any{rule("IpProtocol", "tcp", "FromPort", 443, "ToPort", 443, "CidrIp", "0.0.0.0/0")}, 1, false},
		{nil, 0, true},
	} {
		f := &fakeSecurityGroups{}
		client := f.serve(t)
		desired := map[string]any{"GroupDescription": "d", "VpcId": "vpc-1", "Tags": []any{name},
			"SecurityGroupIngress": []any{rule("IpProtocol", "tcp", "FromPort", 22, "ToPort", 22, "CidrIp", "10.0.0.0/16")}}
		if c.egress != nil {
			desired["SecurityGroupEgress"] = c.egress
		}
		if id, err := client.Create(context.Background(), securityGroupType, desired); err != nil || id != "sg-1" {
			t.Fatalf("Create = %q, %v", id, err)
		}
		if n := len(f.calls["RevokeSecurityGroupEgress"]); n != c.revoked {
			t.Fatalf("RevokeSecurityGroupEgress sent %d times, want %d", n, c.revoked)
		}
		kept := slices.ContainsFunc(f.rules, func(r sgRule) bool { return r.egress && r.proto == "-1" })
		if kept != c.kept {
			t.Fatalf("default egress kept = %v, want %v", kept, c.kept)
		}
		if got := f.calls["CreateSecurityGroup"][0].Get("GroupName"); got != "kraai-e-sg" {
			t.Fatalf("GroupName = %q", got)
		}
	}
}

// A description change is one description call and no revoke: revoking and
// authorizing again would drop the rule's traffic between the two.
func TestUpdateSecurityGroupDescriptionWithoutRevoke(t *testing.T) {
	f := &fakeSecurityGroups{exists: true, name: "kraai-e-sg", rules: []sgRule{
		{proto: "tcp", from: "443", to: "443", cidr4: "10.0.0.0/16", desc: "old"},
		{proto: "tcp", from: "22", to: "22", cidr4: "10.0.0.0/24"},
	}}
	client := f.serve(t)
	current, err := client.ReadByID(context.Background(), securityGroupType, "sg-1")
	if err != nil {
		t.Fatal(err)
	}
	desired := []any{
		rule("IpProtocol", "tcp", "FromPort", 443, "ToPort", 443, "CidrIp", "10.0.0.0/16", "Description", "new"),
		rule("IpProtocol", "-1", "SourceSecurityGroupId", "sg-2"),
	}
	if err := client.Update(context.Background(), securityGroupType, "sg-1", current, map[string]any{"SecurityGroupIngress": desired}); err != nil {
		t.Fatal(err)
	}
	revoked := f.calls["RevokeSecurityGroupIngress"]
	if len(revoked) != 1 || revoked[0].Get("IpPermissions.1.FromPort") != "22" || revoked[0].Has("IpPermissions.2.IpProtocol") {
		t.Fatalf("RevokeSecurityGroupIngress = %v, want port 22 alone", revoked)
	}
	described := f.calls["UpdateSecurityGroupRuleDescriptionsIngress"]
	if len(described) != 1 || described[0].Get("IpPermissions.1.IpRanges.1.Description") != "new" {
		t.Fatalf("UpdateSecurityGroupRuleDescriptionsIngress = %v", described)
	}
	authorized := f.calls["AuthorizeSecurityGroupIngress"]
	if len(authorized) != 1 || authorized[0].Get("IpPermissions.1.Groups.1.GroupId") != "sg-2" || authorized[0].Has("IpPermissions.1.IpRanges.1.CidrIp") {
		t.Fatalf("AuthorizeSecurityGroupIngress = %v", authorized)
	}
}

// A revoke the service ignores leaves the rule in place, and the update's
// wait sees one rule too many rather than accepting a subset.
func TestUpdateSecurityGroupWaitsForExactRules(t *testing.T) {
	f := &fakeSecurityGroups{exists: true, name: "kraai-e-sg", rules: []sgRule{
		{proto: "tcp", from: "443", to: "443", cidr4: "10.0.0.0/16"},
		{proto: "tcp", from: "22", to: "22", cidr4: "10.0.0.0/24"},
	}}
	client := f.serve(t)
	current, err := client.ReadByID(context.Background(), securityGroupType, "sg-1")
	if err != nil {
		t.Fatal(err)
	}
	// The fake revokes by exact rule; a port it does not hold revokes nothing.
	current["SecurityGroupIngress"] = []any{
		rule("IpProtocol", "tcp", "FromPort", 443, "ToPort", 443, "CidrIp", "10.0.0.0/16"),
		rule("IpProtocol", "tcp", "FromPort", 23, "ToPort", 23, "CidrIp", "10.0.0.0/24"),
	}
	desired := []any{rule("IpProtocol", "tcp", "FromPort", 443, "ToPort", 443, "CidrIp", "10.0.0.0/16")}
	err = client.Update(context.Background(), securityGroupType, "sg-1", current, map[string]any{"SecurityGroupIngress": desired})
	if err == nil || !strings.Contains(err.Error(), "was not visible") {
		t.Fatalf("Update = %v, want the wait to refuse a read still holding a rule", err)
	}
}

func TestDeleteSecurityGroup(t *testing.T) {
	f := &fakeSecurityGroups{exists: true, name: "kraai-e-sg"}
	client := f.serve(t)
	// The second delete is answered InvalidGroup.NotFound, taken as done.
	for range 2 {
		if err := client.Delete(context.Background(), securityGroupType, "sg-1"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(f.calls["DeleteSecurityGroup"]); n != 2 {
		t.Fatalf("DeleteSecurityGroup called %d times, want 2", n)
	}
	if got := f.calls["DeleteSecurityGroup"][0].Get("GroupId"); got != "sg-1" {
		t.Fatalf("DeleteSecurityGroup GroupId = %q", got)
	}
}

// An ec2Query create sends its tags by the form keys EC2 takes and reads
// its identifier from the XML response; tags are added by CreateTags and
// removed by DeleteTags, the removed ones sent as structures naming each
// key and no value.
func TestSecurityGroupTagsOverEC2Query(t *testing.T) {
	f := &fakeSecurityGroups{}
	client := f.serve(t)
	name := map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-sg"}
	team := map[string]any{"Key": "team", "Value": "kraai"}
	ctx := context.Background()
	id, err := client.Create(ctx, securityGroupType, map[string]any{"GroupDescription": "d", "VpcId": "vpc-1", "Tags": []any{name, team}})
	if err != nil || id != "sg-1" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	sent := f.calls["CreateSecurityGroup"][0]
	if sent.Get("TagSpecification.1.ResourceType") != "security-group" || sent.Get("Version") != "2016-11-15" ||
		sent.Get("TagSpecification.1.Tag.1.Key") != "kraai:resource-name" || sent.Get("TagSpecification.1.Tag.1.Value") != "kraai-e-sg" ||
		sent.Get("TagSpecification.1.Tag.2.Key") != "team" || sent.Get("TagSpecification.1.Tag.2.Value") != "kraai" {
		t.Fatalf("CreateSecurityGroup form = %v", sent)
	}
	current := map[string]any{"Tags": []any{name, team}}
	if err := client.Update(ctx, securityGroupType, id, current, map[string]any{"Tags": []any{name}}); err != nil {
		t.Fatal(err)
	}
	del := f.calls["DeleteTags"][0]
	if del.Get("ResourceId.1") != "sg-1" || del.Get("Tag.1.Key") != "team" || del.Has("Tag.1.Value") {
		t.Fatalf("DeleteTags form = %v", del)
	}
	current = map[string]any{"Tags": []any{name}}
	changes := map[string]any{"Tags": []any{name, map[string]any{"Key": "owner", "Value": "cloud"}}}
	if err := client.Update(ctx, securityGroupType, id, current, changes); err != nil {
		t.Fatal(err)
	}
	add := f.calls["CreateTags"][0]
	if add.Get("ResourceId.1") != "sg-1" || add.Get("Tag.1.Key") != "owner" || add.Get("Tag.1.Value") != "cloud" || add.Has("Tag.2.Key") {
		t.Fatalf("CreateTags form = %v", add)
	}
}

// Every property of a security group the update has no call for is
// create-only, so a change to one is refused, never silently dropped.
func TestUpdateRefusesASecurityGroupVpcChange(t *testing.T) {
	f := &fakeSecurityGroups{exists: true, name: "kraai-e-sg"}
	client := f.serve(t)
	err := client.Update(context.Background(), securityGroupType, "sg-1", nil, map[string]any{"VpcId": "vpc-2"})
	if err == nil || !strings.Contains(err.Error(), "no direct update for VpcId") {
		t.Fatalf("Update = %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls = %v, want none", f.calls)
	}
}
