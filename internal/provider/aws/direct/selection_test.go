package direct

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// A response path may select the resource from a list by any of several
// members: a widget found by its id or by a name that holds one.
func TestReadJSONSelectsTheResourceByAnyAlternative(t *testing.T) {
	o := widgetOverride("Widgets[Name|WidgetId={WidgetId}]")
	o.Read.Identifier = map[string]string{"WidgetId": "WidgetIds"}
	r, errs := compileWidget(t, listWidget(), o)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if want := []Step{{Name: "Widgets", List: true, Where: "Name|WidgetId", Equals: "{WidgetId}"}}; !reflect.DeepEqual(r.Response, want) {
		t.Fatalf("Response = %+v, want %+v", r.Response, want)
	}
	readers[r.Type] = r
	t.Cleanup(func() { delete(readers, r.Type) })

	for name, c := range map[string]struct {
		body string
		want map[string]any
		err  string
	}{
		"by the id":   {`{"Widgets":[{"WidgetId":"w-2","Name":"x"},{"WidgetId":"w-1","Name":"a"}]}`, map[string]any{"WidgetId": "w-1", "Name": "a"}, ""},
		"by the name": {`{"Widgets":[{"WidgetId":"w-2","Name":"x"},{"WidgetId":"w-3","Name":"w-1"}]}`, map[string]any{"WidgetId": "w-3", "Name": "w-1"}, ""},
		"by neither":  {`{"Widgets":[{"WidgetId":"w-2","Name":"x"}]}`, nil, ErrAbsent.Error()},
		"by both":     {`{"Widgets":[{"WidgetId":"w-1"},{"WidgetId":"w-3","Name":"w-1"}]}`, nil, "lists 2 instances"},
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := bodyPages(t, func(string) (int, string) { return 200, c.body })
			got, err := client.Read(context.Background(), r.Type, map[string]string{"WidgetId": "w-1"})
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) || errors.Is(err, ErrAbsent) != (c.err == ErrAbsent.Error()) {
					t.Fatalf("Read = %v, %v; want an error containing %q", got, err, c.err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Read = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}

// A selection's member is a path through structures, and a missing step
// reads as no value.
func TestMemberAtFollowsStructures(t *testing.T) {
	obj := map[string]any{"State": map[string]any{"Code": "ok"}, "Name": "n", "Plain": 3}
	for path, want := range map[string]any{"Name": "n", "State/Code": "ok", "State/Nope": nil, "Name/Code": nil, "Nope/Code": nil} {
		if got := memberAt(obj, path); got != want {
			t.Errorf("memberAt(%q) = %v, want %v", path, got, want)
		}
	}
	w := &walk{vars: map[string]string{"Id": "ok"}}
	step := Step{Where: "Nope|State/Code", Equals: "{Id}"}
	if !w.selects(step, func(p string) any { return memberAt(obj, p) }) || w.selects(Step{Where: "Name", Equals: "{Id}"}, func(p string) any { return memberAt(obj, p) }) {
		t.Fatal("selects does not pass an element by any alternative, and only by one that equals")
	}
}

func TestCompileRefusesASelectionItCannotRead(t *testing.T) {
	for name, c := range map[string]struct{ response, want string }{
		"a member the element lacks":       {"Widgets[Nope={WidgetId}]", "selects by Nope, not a string member"},
		"an alternative that is not there": {"Widgets[Name|Nope={WidgetId}]", "selects by Name|Nope, not a string member"},
		"a member that is not a string":    {"Widgets[Size={WidgetId}]", "selects by Size, not a string member"},
		"a value that is not the identity": {"Widgets[Name={Size}]", "selects by {Size}, which is not the primary identifier"},
		"every element that passes":        {"Widgets[Name={WidgetId}]*", "keeps every element"},
	} {
		t.Run(name, func(t *testing.T) {
			o := widgetOverride(c.response)
			o.Read.Identifier = map[string]string{"WidgetId": "WidgetIds"}
			if _, errs := compileWidget(t, listWidget(), o); !containsErr(errs, c.want) {
				t.Fatalf("errors = %v, want one containing %q", errs, c.want)
			}
		})
	}
}

// What the composite identifiers and gateway attachments the overrides
// declare must be, or the compiler refuses them before any call is made.
func TestCompileRefusesWhatACompositeIdentifierNeeds(t *testing.T) {
	const route, attachment = "AWS--EC2--Route.yaml", "AWS--EC2--VPCGatewayAttachment.yaml"
	alternatives := "{DestinationCidrBlock|DestinationIpv6CidrBlock|DestinationPrefixListId}"
	for name, c := range map[string]struct {
		file, old, replacement, want string
	}{
		"an alternative that is no property":       {route, alternatives, "{DestinationCidrBlock|Nope}", "Nope must be a property the input sends"},
		"an alternative the create does not send":  {route, alternatives, "{DestinationCidrBlock|CidrBlock}", "CidrBlock must be a property the input sends"},
		"an identifier mapped to the wrong echo":   {route, `RouteTableId: "{RouteTableId}"` + "\n    CidrBlock", `RouteTableId: "{CidrBlock}"` + "\n    CidrBlock", "it must be {RouteTableId}"},
		"a fixed identifier value naming nothing":  {attachment, "AttachmentType: =IGW", "AttachmentType: =", "names no value"},
		"a response selected by another property":  {route, "={CidrBlock}]", "={VpcId}]", "selects by {VpcId}"},
		"a served value of a property not an id":   {attachment, "AttachmentType: [IGW]", "InternetGatewayId: [IGW]", "not a property of a composite identifier"},
		"unserved without serves":                  {attachment, "  serves:\n    AttachmentType: [IGW]\n", "", "unserved VpnGatewayId must be"},
		"an unserved property that is also mapped": {attachment, "  AttachmentType: \"{AttachmentType}\"", "  VpnGatewayId: InternetGatewayId\n  AttachmentType: \"{AttachmentType}\"", "unserved VpnGatewayId is also mapped"},
		"a before call that names no property":     {attachment, "InternetGatewayId: \"{CurrentInternetGatewayId}\"\n        VpcId: \"{VpcId}\"\n      absentErrors", "InternetGatewayId: \"{Nope}\"\n        VpcId: \"{VpcId}\"\n      absentErrors", "names {Nope}, which is not a property"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := compileAll(edit(t, c.file, c.old, c.replacement))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("compile = %v\nwant an error containing %q", err, c.want)
			}
		})
	}
}
