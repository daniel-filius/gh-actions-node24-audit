package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// fakeGitHub serves action.yml/workflow content from memory. Keys for files
// are "owner/repo/path@ref" (ref "" for the default branch).
type fakeGitHub struct {
	files     map[string]string
	workflows map[string][]string
	latest    map[string]string
	tags      map[string][]string
	calls     int
}

func (f *fakeGitHub) ListWorkflows(o, r string) ([]string, error) {
	f.calls++
	w, ok := f.workflows[o+"/"+r]
	if !ok {
		return nil, ErrNotFound
	}
	return w, nil
}
func (f *fakeGitHub) FileContent(o, r, p, ref string) (string, error) {
	f.calls++
	s, ok := f.files[o+"/"+r+"/"+p+"@"+ref]
	if !ok {
		return "", ErrNotFound
	}
	return s, nil
}
func (f *fakeGitHub) ResolveRef(o, r, ref string) (string, error) {
	f.calls++
	return "deadbeef" + ref, nil
}
func (f *fakeGitHub) LatestReleaseTag(o, r string) (string, error) {
	f.calls++
	return f.latest[o+"/"+r], nil
}
func (f *fakeGitHub) Tags(o, r string) ([]string, error) {
	f.calls++
	return f.tags[o+"/"+r], nil
}
func (f *fakeGitHub) ListRepos(org string) ([]string, error) { return nil, ErrNotFound }

func newFake(t *testing.T) *fakeGitHub {
	node20 := fixture(t, "action-node20.yml")
	node24 := fixture(t, "action-node24.yml")
	return &fakeGitHub{
		workflows: map[string][]string{"me/app": {".github/workflows/ci.yml"}},
		files: map[string]string{
			"me/app/.github/workflows/ci.yml@": fixture(t, "workflow.yml"),
			// checkout: v4 is node20, v5 is node24, latest release v5.0.0 is node24
			"actions/checkout/action.yml@v4":     node20,
			"actions/checkout/action.yml@v5":     node24,
			"actions/checkout/action.yml@v5.0.0": node24,
			// setup-node: already node24
			"actions/setup-node/action.yml@v5": node24,
			// codeql init: composite with a node20 child and a node24 child
			"github/codeql-action/init/action.yml@v3": fixture(t, "action-composite.yml"),
			"octo/inner-old/action.yml@v1":            node20,
			"octo/inner-new/action.yml@v2":            node24,
			// gh-release: node20 with no node24 release anywhere
			"softprops/action-gh-release/action.yml@v2": node20,
			"softprops/action-gh-release/action.yml@v1": fixture(t, "action-node16.yml"),
			// cache pinned by sha: node20, v5 is node24
			"actions/cache/action.yml@0400d5f644dc74513175e3cd8d07132dd435b249": node20,
			"actions/cache/action.yml@v5":                                       node24,
			"actions/cache/action.yml@v4":                                       node20,
		},
		latest: map[string]string{
			"actions/checkout":            "v5.0.0",
			"softprops/action-gh-release": "v2.3.2", // release tag without action.yml in the fake -> skipped
		},
		tags: map[string][]string{
			"actions/checkout":            {"v1", "v2", "v3", "v4", "v4.2.2", "v5", "v5.0.0"},
			"softprops/action-gh-release": {"v1", "v2", "v2.3.2"},
			"actions/cache":               {"v1", "v2", "v3", "v4", "v5"},
		},
	}
}

func findingFor(rep *Report, uses string) *Finding {
	for i := range rep.Findings {
		if rep.Findings[i].Uses == uses && rep.Findings[i].Via == "" {
			return &rep.Findings[i]
		}
	}
	return nil
}

func TestAuditorRun(t *testing.T) {
	gh := newFake(t)
	a := &Auditor{GH: newCached(gh), Concurrency: 2}
	rep, err := a.Run([]string{"me/app"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Summary.Workflows != 1 || rep.Summary.Repos != 1 {
		t.Fatalf("summary %+v", rep.Summary)
	}
	// 5 remote actions in the fixture (local, docker and reusable skipped).
	if rep.Summary.Actions != 5 {
		t.Fatalf("expected 5 actions, got %d: %+v", rep.Summary.Actions, rep.Findings)
	}
	if len(rep.Skipped) != 3 {
		t.Fatalf("expected 3 skipped, got %+v", rep.Skipped)
	}

	co := findingFor(rep, "actions/checkout@v4")
	if co == nil || co.Runtime != RuntimeNode20 || !co.Outdated || co.Suggestion != "actions/checkout@v5" {
		t.Errorf("checkout@v4: %+v", co)
	}
	if co != nil && co.SHA != "deadbeefv4" {
		t.Errorf("checkout sha not resolved: %+v", co)
	}
	sn := findingFor(rep, "actions/setup-node@v5")
	if sn == nil || sn.Runtime != RuntimeNode24 || sn.Outdated || sn.Suggestion != "" {
		t.Errorf("setup-node@v5: %+v", sn)
	}
	rel := findingFor(rep, "softprops/action-gh-release@v2")
	if rel == nil || !rel.Outdated || rel.Suggestion != "" || !strings.Contains(rel.Note, "no Node 24 version") {
		t.Errorf("gh-release@v2: %+v", rel)
	}
	ca := findingFor(rep, "actions/cache@0400d5f644dc74513175e3cd8d07132dd435b249")
	if ca == nil || ca.Suggestion != "actions/cache@v5" {
		t.Errorf("cache@sha: %+v", ca)
	}
	cq := findingFor(rep, "github/codeql-action/init@v3")
	if cq == nil || cq.Runtime != RuntimeComposite || cq.Outdated {
		t.Errorf("codeql init: %+v", cq)
	}
	for _, f := range rep.Findings {
		if f.Via != "" {
			t.Errorf("composite children must not be inspected without --include-composite: %+v", f)
		}
	}
	if rep.Summary.Outdated != 3 {
		t.Errorf("expected 3 outdated, got %d", rep.Summary.Outdated)
	}
}

func TestAuditorIncludeComposite(t *testing.T) {
	gh := newFake(t)
	a := &Auditor{GH: newCached(gh), IncludeComposite: true}
	rep, err := a.Run([]string{"me/app"})
	if err != nil {
		t.Fatal(err)
	}
	var nested []Finding
	for _, f := range rep.Findings {
		if f.Via == "github/codeql-action/init@v3" {
			nested = append(nested, f)
		}
	}
	if len(nested) != 2 {
		t.Fatalf("expected 2 nested findings, got %+v", nested)
	}
	var sawOld bool
	for _, n := range nested {
		if n.Uses == "octo/inner-old@v1" && n.Runtime == RuntimeNode20 && n.Outdated {
			sawOld = true
		}
	}
	if !sawOld {
		t.Errorf("nested node20 child not flagged: %+v", nested)
	}
	if rep.Summary.Outdated != 4 {
		t.Errorf("expected 4 outdated with composite, got %d", rep.Summary.Outdated)
	}
}

func TestCacheDeduplicates(t *testing.T) {
	gh := newFake(t)
	gh.workflows["me/app"] = []string{".github/workflows/ci.yml", ".github/workflows/ci2.yml"}
	gh.files["me/app/.github/workflows/ci2.yml@"] = gh.files["me/app/.github/workflows/ci.yml@"]
	a := &Auditor{GH: newCached(gh), Concurrency: 1}
	if _, err := a.Run([]string{"me/app"}); err != nil {
		t.Fatal(err)
	}
	first := gh.calls
	gh.calls = 0
	// A second identical run through the same cache must only re-list workflows
	// and re-read the two workflow files that are fetched with ref "" (cached too).
	if _, err := a.Run([]string{"me/app"}); err != nil {
		t.Fatal(err)
	}
	if gh.calls >= first/2 {
		t.Errorf("cache ineffective: %d calls first run, %d second", first, gh.calls)
	}
}

func TestPrintTableAndJSON(t *testing.T) {
	gh := newFake(t)
	a := &Auditor{GH: newCached(gh)}
	rep, _ := a.Run([]string{"me/app"})

	var buf bytes.Buffer
	printTable(&buf, rep, false, true)
	out := buf.String()
	for _, want := range []string{"! node20", "actions/checkout@v5", "no Node 24 version", "SKIPPED", "declarative check"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}

	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Summary.Outdated != 3 || len(back.Findings) != 5 {
		t.Errorf("json round trip: %+v", back.Summary)
	}
}
