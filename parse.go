package main

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Runtime is the value of `runs.using` in an action.yml, normalised.
type Runtime string

const (
	RuntimeNode12    Runtime = "node12"
	RuntimeNode16    Runtime = "node16"
	RuntimeNode20    Runtime = "node20"
	RuntimeNode24    Runtime = "node24"
	RuntimeDocker    Runtime = "docker"
	RuntimeComposite Runtime = "composite"
	RuntimeUnknown   Runtime = "unknown"
)

// Outdated reports whether the runtime is a JavaScript runtime that GitHub no
// longer ships on hosted runners (Node 20 was removed on 2026-09-23).
func (r Runtime) Outdated() bool {
	switch r {
	case RuntimeNode12, RuntimeNode16, RuntimeNode20:
		return true
	}
	return false
}

// UsesRef is one `uses:` reference to a remote action: owner/repo[/path]@ref.
type UsesRef struct {
	Owner string
	Repo  string
	Path  string // sub-directory inside the repo, "" for the root action
	Ref   string
}

// Slug returns owner/repo[/path].
func (u UsesRef) Slug() string {
	if u.Path != "" {
		return u.Owner + "/" + u.Repo + "/" + u.Path
	}
	return u.Owner + "/" + u.Repo
}

// String returns the reference exactly as it would appear after `uses:`.
func (u UsesRef) String() string { return u.Slug() + "@" + u.Ref }

// CacheKey identifies the action.yml that must be read: owner/repo[/path]@ref.
func (u UsesRef) CacheKey() string { return u.String() }

// uses: value  |  - uses: "value"  |  uses: 'value' # comment
var usesLine = regexp.MustCompile(`^\s*-?\s*uses:\s*["']?([^"'\s#]+)["']?`)

// ExtractUses scans YAML text (a workflow or a composite action) line by line
// and returns every `uses:` value in order of appearance, including local
// (`./x`), docker (`docker://x`) and reusable-workflow references. Filtering is
// done by ParseUses. Line scanning is deliberately tolerant: workflows in the
// wild contain anchors, templates and invalid YAML that a strict parser rejects.
func ExtractUses(text string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		if m := usesLine.FindStringSubmatch(sc.Text()); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// SkipReason explains why a `uses:` value is not audited.
type SkipReason string

const (
	SkipLocal    SkipReason = "local action (./...)"
	SkipDocker   SkipReason = "docker:// image"
	SkipReusable SkipReason = "reusable workflow"
	SkipInvalid  SkipReason = "unparseable reference"
)

// ParseUses turns a raw `uses:` value into a UsesRef, or a SkipReason when the
// reference is not a remote action that has an action.yml to inspect.
func ParseUses(raw string) (UsesRef, SkipReason) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return UsesRef{}, SkipInvalid
	case strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, ".\\"):
		return UsesRef{}, SkipLocal
	case strings.HasPrefix(raw, "docker://"):
		return UsesRef{}, SkipDocker
	case strings.Contains(raw, "/.github/workflows/"):
		return UsesRef{}, SkipReusable
	}
	at := strings.LastIndex(raw, "@")
	if at <= 0 || at == len(raw)-1 {
		return UsesRef{}, SkipInvalid
	}
	slug, ref := raw[:at], raw[at+1:]
	parts := strings.SplitN(slug, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return UsesRef{}, SkipInvalid
	}
	u := UsesRef{Owner: parts[0], Repo: parts[1], Ref: ref}
	if len(parts) == 3 {
		u.Path = strings.Trim(parts[2], "/")
	}
	return u, ""
}

type actionFile struct {
	Runs struct {
		Using string `yaml:"using"`
		Steps []struct {
			Uses string `yaml:"uses"`
		} `yaml:"steps"`
	} `yaml:"runs"`
}

// ActionMeta is what the auditor needs from an action.yml.
type ActionMeta struct {
	Runtime Runtime
	// CompositeUses lists the `uses:` of a composite action's steps (raw values).
	CompositeUses []string
}

// ParseActionYAML reads `runs.using` (and composite steps) from action.yml text.
func ParseActionYAML(text string) (ActionMeta, error) {
	var f actionFile
	if err := yaml.Unmarshal([]byte(text), &f); err != nil {
		// Fall back to a line scan: some action.yml files are not strict YAML.
		if rt := scanUsing(text); rt != RuntimeUnknown {
			return ActionMeta{Runtime: rt}, nil
		}
		return ActionMeta{Runtime: RuntimeUnknown}, fmt.Errorf("parse action.yml: %w", err)
	}
	meta := ActionMeta{Runtime: normaliseUsing(f.Runs.Using)}
	if meta.Runtime == RuntimeComposite {
		for _, s := range f.Runs.Steps {
			if s.Uses != "" {
				meta.CompositeUses = append(meta.CompositeUses, s.Uses)
			}
		}
	}
	return meta, nil
}

var usingLine = regexp.MustCompile(`^\s*using:\s*["']?([A-Za-z0-9]+)["']?`)

func scanUsing(text string) Runtime {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		if m := usingLine.FindStringSubmatch(sc.Text()); m != nil {
			return normaliseUsing(m[1])
		}
	}
	return RuntimeUnknown
}

func normaliseUsing(s string) Runtime {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "node12":
		return RuntimeNode12
	case "node16":
		return RuntimeNode16
	case "node20":
		return RuntimeNode20
	case "node24":
		return RuntimeNode24
	case "docker":
		return RuntimeDocker
	case "composite":
		return RuntimeComposite
	}
	return RuntimeUnknown
}

var majorTag = regexp.MustCompile(`^v(\d+)$`)

// MajorTags filters a list of tag names down to floating majors (v1, v2, ...)
// sorted from highest to lowest.
func MajorTags(tags []string) []string {
	type mt struct {
		n    int
		name string
	}
	var ms []mt
	for _, t := range tags {
		if m := majorTag.FindStringSubmatch(t); m != nil {
			n := 0
			fmt.Sscanf(m[1], "%d", &n)
			ms = append(ms, mt{n, t})
		}
	}
	for i := 1; i < len(ms); i++ { // insertion sort, lists are tiny
		for j := i; j > 0 && ms[j].n > ms[j-1].n; j-- {
			ms[j], ms[j-1] = ms[j-1], ms[j]
		}
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.name)
	}
	return out
}
