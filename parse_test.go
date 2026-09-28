package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExtractUses(t *testing.T) {
	got := ExtractUses(fixture(t, "workflow.yml"))
	want := []string{
		"actions/checkout@v4",
		"actions/setup-node@v5",
		"./.github/actions/local-thing",
		"docker://alpine:3.20",
		"github/codeql-action/init@v3",
		"octo/reusable/.github/workflows/build.yml@main",
		"actions/checkout@v4", // duplicates are kept here and deduplicated per workflow by the auditor
		"softprops/action-gh-release@v2",
		"actions/cache@0400d5f644dc74513175e3cd8d07132dd435b249",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractUses mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestParseUses(t *testing.T) {
	cases := []struct {
		in     string
		want   UsesRef
		reason SkipReason
	}{
		{"actions/checkout@v4", UsesRef{Owner: "actions", Repo: "checkout", Ref: "v4"}, ""},
		{"github/codeql-action/init@v3", UsesRef{Owner: "github", Repo: "codeql-action", Path: "init", Ref: "v3"}, ""},
		{"./.github/actions/x", UsesRef{}, SkipLocal},
		{"docker://alpine:3.20", UsesRef{}, SkipDocker},
		{"octo/reusable/.github/workflows/build.yml@main", UsesRef{}, SkipReusable},
		{"actions/checkout", UsesRef{}, SkipInvalid},
		{"checkout@v4", UsesRef{}, SkipInvalid},
		{"", UsesRef{}, SkipInvalid},
	}
	for _, c := range cases {
		got, reason := ParseUses(c.in)
		if got != c.want || reason != c.reason {
			t.Errorf("ParseUses(%q) = %+v, %q; want %+v, %q", c.in, got, reason, c.want, c.reason)
		}
	}
	u, _ := ParseUses("github/codeql-action/init@v3")
	if u.String() != "github/codeql-action/init@v3" || u.Slug() != "github/codeql-action/init" {
		t.Errorf("String/Slug round trip broken: %q %q", u.String(), u.Slug())
	}
}

func TestParseActionYAML(t *testing.T) {
	cases := map[string]struct {
		rt       Runtime
		outdated bool
		children int
	}{
		"action-node20.yml":    {RuntimeNode20, true, 0},
		"action-node24.yml":    {RuntimeNode24, false, 0},
		"action-node16.yml":    {RuntimeNode16, true, 0},
		"action-docker.yml":    {RuntimeDocker, false, 0},
		"action-composite.yml": {RuntimeComposite, false, 2},
	}
	for name, want := range cases {
		meta, err := ParseActionYAML(fixture(t, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if meta.Runtime != want.rt || meta.Runtime.Outdated() != want.outdated || len(meta.CompositeUses) != want.children {
			t.Errorf("%s: got %s outdated=%v children=%d; want %s %v %d", name, meta.Runtime, meta.Runtime.Outdated(), len(meta.CompositeUses), want.rt, want.outdated, want.children)
		}
	}
	// Broken YAML still yields the runtime through the line scanner.
	meta, err := ParseActionYAML("name: x\nruns:\n  using: node20\n  main: [unclosed\n")
	if err != nil || meta.Runtime != RuntimeNode20 {
		t.Errorf("fallback scan: got %s, %v", meta.Runtime, err)
	}
}

func TestMajorTags(t *testing.T) {
	got := MajorTags([]string{"v1", "v4.2.1", "v10", "v2", "release-1", "v3", "v3.0.0"})
	want := []string{"v10", "v3", "v2", "v1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MajorTags = %v, want %v", got, want)
	}
}
