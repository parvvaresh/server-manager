package main

import (
	"reflect"
	"strings"
	"testing"
)

func names(list []Server) []string {
	out := []string{}
	for _, s := range list {
		out = append(out, s.Name)
	}
	return out
}

func TestResolveTargets(t *testing.T) {
	list := []Server{
		{Name: "web-1", Tags: "prod,web"},
		{Name: "web-2", Tags: "prod, web"},
		{Name: "db-1", Tags: "prod,db"},
		{Name: "lab", Tags: "test"},
	}
	cases := []struct {
		targets []string
		want    []string
		err     string
	}{
		{[]string{"lab"}, []string{"lab"}, ""},
		{[]string{"web-*"}, []string{"web-1", "web-2"}, ""},
		{[]string{"tag:db", "tag:PROD"}, []string{"db-1", "web-1", "web-2"}, ""},
		{[]string{"web-2", "tag:web"}, []string{"web-2", "web-1"}, ""},
		{[]string{"all"}, []string{"web-1", "web-2", "db-1", "lab"}, ""},
		{[]string{"nope"}, nil, "matches no server"},
		{[]string{"tag:nope"}, nil, "matches no server"},
		{[]string{"web-["}, nil, "bad pattern"},
	}
	for _, c := range cases {
		got, err := resolveTargets(list, c.targets)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v: want error %q, got %v", c.targets, c.err, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: %v", c.targets, err)
			continue
		}
		if !reflect.DeepEqual(names(got), c.want) {
			t.Errorf("%v: got %v, want %v", c.targets, names(got), c.want)
		}
	}
}

func TestGroupResults(t *testing.T) {
	order := []Server{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}, {Name: "e"}}
	res := []execResult{
		{Server: order[3], Out: []byte("v2\n")},
		{Server: order[0], Out: []byte("v1\n")},
		{Server: order[4], Out: []byte("v1\n"), Code: 1},
		{Server: order[2], Out: []byte("v1\n")},
		{Server: order[1], Out: []byte("v2\n")},
	}
	var got [][]string
	for _, g := range groupResults(res, order) {
		var n []string
		for _, r := range g {
			n = append(n, r.Server.Name)
		}
		got = append(got, n)
	}
	// Same output but a different exit status is a different answer.
	want := [][]string{{"a", "c"}, {"b", "d"}, {"e"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
