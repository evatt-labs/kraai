package direct

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// wireCodes is every error code the service answers that its model
// declares: the awsQueryError code under awsQuery and ec2Query, where the
// SDKs match on it, and the shape name otherwise. A code declared on any
// operation counts, not only the one that names it: a service answers
// codes its operations do not each declare.
func wireCodes(model *smithyModel, protocol string) map[string]bool {
	out := map[string]bool{}
	for id, s := range model.Shapes {
		if s.Traits["smithy.api#error"] == nil {
			continue
		}
		code := id[strings.Index(id, "#")+1:]
		if protocol == "awsQuery" || protocol == "ec2Query" {
			var q struct{ Code string }
			if json.Unmarshal(s.Traits["aws.protocols#awsQueryError"], &q) == nil && q.Code != "" {
				code = q.Code
			}
		}
		out[code] = true
	}
	return out
}

// errorSite is one place an override names error codes.
type errorSite struct {
	at             string
	absent, retry  []string
	absentAllowed  bool
	read, deletion bool
}

// errorSites lists every place o names error codes. Only a read, a further
// call, a delete and an update's before call act on absentErrors; anywhere
// else they would never be read, so they are refused rather than inert.
func errorSites(o Override) []errorSite {
	sites := []errorSite{{at: "read", absent: o.Read.AbsentErrors, absentAllowed: true, read: true}}
	for _, a := range o.Also {
		sites = append(sites, errorSite{at: "also " + a.Operation, absent: a.AbsentErrors, absentAllowed: true, read: true})
	}
	mutation := func(at string, m Mutation, absentAllowed, deletion bool) {
		retry := make([]string, 0, len(m.RetryErrors))
		for _, entry := range m.RetryErrors {
			code, _, _ := strings.Cut(entry, ":")
			retry = append(retry, code)
		}
		sites = append(sites, errorSite{at: at, absent: m.AbsentErrors, retry: retry, absentAllowed: absentAllowed, deletion: deletion})
	}
	if o.Create != nil {
		mutation("create", o.Create.Mutation, false, false)
	}
	for _, u := range o.Update {
		at := "update " + u.Operation
		mutation(at, u.Mutation, false, false)
		if u.Before != nil {
			mutation(at+" before", *u.Before, true, false)
		}
		if u.Tags != nil {
			mutation(at+" tags add", u.Tags.Add, false, false)
			mutation(at+" tags remove", u.Tags.Remove, false, false)
		}
		if l := u.List; l != nil {
			mutation(at+" list add", l.Add, false, false)
			for name, m := range map[string]*Mutation{"remove": l.Remove, "change": l.Change} {
				if m != nil {
					mutation(at+" list "+name, *m, false, false)
				}
			}
			for _, c := range l.Changes {
				mutation(at+" list changes", c.Mutation, false, false)
			}
		}
	}
	if o.Delete != nil {
		mutation("delete", *o.Delete, true, true)
	}
	return sites
}

// checkErrorCodes refuses every code o names that the model declares on
// no error shape and o.UndeclaredErrors does not list, and every listing
// that is not needed. It returns the listed codes a read and a delete
// answer for absence, which evidence must show observed.
func checkErrorCodes(model *smithyModel, protocol string, o Override) (read, deletion []string, errs []error) {
	known := wireCodes(model, protocol)
	used := map[string]bool{}
	check := func(at, kind, code string) {
		used[code] = true
		if known[code] {
			return
		}
		if _, listed := o.UndeclaredErrors[code]; !listed {
			errs = append(errs, fmt.Errorf("%s %s names %s, which no error shape in the checked-in model declares under %s; list it under undeclaredErrors with why if the service answers it", at, kind, code, protocol))
		}
	}
	for _, s := range errorSites(o) {
		if len(s.absent) > 0 && !s.absentAllowed {
			errs = append(errs, fmt.Errorf("%s names absentErrors, which only a read, a further call, a delete and a before call act on", s.at))
		}
		for _, code := range s.absent {
			check(s.at, "absentErrors", code)
			if _, listed := o.UndeclaredErrors[code]; known[code] || !listed {
				continue
			}
			if s.read && !slices.Contains(read, code) {
				read = append(read, code)
			}
			if s.deletion && !slices.Contains(deletion, code) {
				deletion = append(deletion, code)
			}
		}
		for _, code := range s.retry {
			check(s.at, "retryErrors", code)
		}
	}
	for _, code := range sortedKeys(o.UndeclaredErrors) {
		switch {
		case known[code]:
			errs = append(errs, fmt.Errorf("undeclaredErrors lists %s, which the model declares", code))
		case !used[code]:
			errs = append(errs, fmt.Errorf("undeclaredErrors lists %s, which no absentErrors or retryErrors names", code))
		case strings.TrimSpace(o.UndeclaredErrors[code]) == "":
			errs = append(errs, fmt.Errorf("undeclaredErrors lists %s without why", code))
		}
	}
	slices.Sort(read)
	slices.Sort(deletion)
	return read, deletion, errs
}
