package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Finding is one audited `uses:` occurrence.
type Finding struct {
	Repo       string  `json:"repo"`                 // owner/repo that owns the workflow
	Workflow   string  `json:"workflow"`             // .github/workflows/ci.yml
	Uses       string  `json:"uses"`                 // owner/action@ref as written
	Via        string  `json:"via,omitempty"`        // parent composite action, when nested
	SHA        string  `json:"sha,omitempty"`        // commit the ref resolves to
	Runtime    Runtime `json:"runtime"`              // node16|node20|node24|docker|composite|unknown
	Outdated   bool    `json:"outdated"`             // runtime removed from hosted runners
	Suggestion string  `json:"suggestion,omitempty"` // tag that declares node24, or ""
	Note       string  `json:"note,omitempty"`       // human hint / error
}

// Report is the JSON document printed with --json.
type Report struct {
	Findings []Finding `json:"findings"`
	Skipped  []Skipped `json:"skipped,omitempty"`
	Summary  Summary   `json:"summary"`
}

// Skipped records a `uses:` that was intentionally not audited.
type Skipped struct {
	Repo     string     `json:"repo"`
	Workflow string     `json:"workflow"`
	Uses     string     `json:"uses"`
	Reason   SkipReason `json:"reason"`
}

// Summary aggregates counts for the footer and for CI.
type Summary struct {
	Repos      int            `json:"repos"`
	Workflows  int            `json:"workflows"`
	Actions    int            `json:"actions"`
	Outdated   int            `json:"outdated"`
	ByRuntime  map[string]int `json:"by_runtime"`
	Errors     int            `json:"errors"`
	RateLimits int            `json:"rate_limit_hits"`
}

// Auditor walks repositories and classifies every remote action they use.
type Auditor struct {
	GH               GitHub
	IncludeComposite bool
	Concurrency      int
	// MaxCandidates caps how many tags are inspected per outdated action.
	MaxCandidates int

	mu         sync.Mutex
	rateLimits int
}

// Run audits the given owner/repo pairs and returns the report.
func (a *Auditor) Run(repos []string) (*Report, error) {
	if a.Concurrency <= 0 {
		a.Concurrency = 6
	}
	if a.MaxCandidates <= 0 {
		a.MaxCandidates = 6
	}
	rep := &Report{Summary: Summary{ByRuntime: map[string]int{}}}
	rep.Summary.Repos = len(repos)

	type job struct {
		repo, workflow string
	}
	var jobs []job
	for _, r := range repos {
		owner, name, ok := strings.Cut(r, "/")
		if !ok {
			return nil, fmt.Errorf("repository %q must be OWNER/REPO", r)
		}
		wfs, err := a.GH.ListWorkflows(owner, name)
		if errors.Is(err, ErrNotFound) {
			continue // no .github/workflows directory
		}
		if err != nil {
			a.noteErr(err)
			rep.Summary.Errors++
			rep.Findings = append(rep.Findings, Finding{Repo: r, Runtime: RuntimeUnknown, Note: "list workflows: " + err.Error()})
			continue
		}
		for _, w := range wfs {
			jobs = append(jobs, job{r, w})
		}
	}
	rep.Summary.Workflows = len(jobs)

	var (
		wg   sync.WaitGroup
		sem  = make(chan struct{}, a.Concurrency)
		outM sync.Mutex
	)
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			fs, sk := a.auditWorkflow(j.repo, j.workflow)
			outM.Lock()
			rep.Findings = append(rep.Findings, fs...)
			rep.Skipped = append(rep.Skipped, sk...)
			outM.Unlock()
		}(j)
	}
	wg.Wait()

	sort.Slice(rep.Findings, func(i, k int) bool {
		x, y := rep.Findings[i], rep.Findings[k]
		if x.Repo != y.Repo {
			return x.Repo < y.Repo
		}
		if x.Workflow != y.Workflow {
			return x.Workflow < y.Workflow
		}
		return x.Uses < y.Uses
	})
	for _, f := range rep.Findings {
		if f.Uses == "" {
			continue
		}
		rep.Summary.Actions++
		rep.Summary.ByRuntime[string(f.Runtime)]++
		if f.Outdated {
			rep.Summary.Outdated++
		}
		if f.Runtime == RuntimeUnknown && f.Note != "" {
			rep.Summary.Errors++
		}
	}
	rep.Summary.RateLimits = a.rateLimits
	return rep, nil
}

func (a *Auditor) noteErr(err error) {
	if errors.Is(err, ErrRateLimited) {
		a.mu.Lock()
		a.rateLimits++
		a.mu.Unlock()
	}
}

func (a *Auditor) auditWorkflow(repo, workflow string) ([]Finding, []Skipped) {
	owner, name, _ := strings.Cut(repo, "/")
	text, err := a.GH.FileContent(owner, name, workflow, "")
	if err != nil {
		a.noteErr(err)
		return []Finding{{Repo: repo, Workflow: workflow, Runtime: RuntimeUnknown, Note: "read workflow: " + err.Error()}}, nil
	}
	var findings []Finding
	var skipped []Skipped
	seen := map[string]bool{}
	for _, raw := range ExtractUses(text) {
		if seen[raw] {
			continue
		}
		seen[raw] = true
		u, reason := ParseUses(raw)
		if reason != "" {
			skipped = append(skipped, Skipped{repo, workflow, raw, reason})
			continue
		}
		f, nested := a.inspect(u)
		f.Repo, f.Workflow = repo, workflow
		findings = append(findings, f)
		for _, n := range nested {
			n.Repo, n.Workflow, n.Via = repo, workflow, u.String()
			findings = append(findings, n)
		}
	}
	return findings, skipped
}

// inspect classifies one remote action and, when it is composite and
// IncludeComposite is on, its direct children (one level).
func (a *Auditor) inspect(u UsesRef) (Finding, []Finding) {
	f := Finding{Uses: u.String()}
	meta, err := a.actionMeta(u)
	if err != nil {
		a.noteErr(err)
		f.Runtime = RuntimeUnknown
		switch {
		case errors.Is(err, ErrNotFound):
			f.Note = "action.yml not found at this ref (deleted tag, private repo or wrong path?)"
		default:
			f.Note = err.Error()
		}
		return f, nil
	}
	f.Runtime = meta.Runtime
	f.Outdated = meta.Runtime.Outdated()
	if sha, err := a.GH.ResolveRef(u.Owner, u.Repo, u.Ref); err == nil {
		f.SHA = sha
	} else {
		a.noteErr(err)
	}
	if f.Outdated {
		f.Suggestion, f.Note = a.suggest(u)
	}
	var nested []Finding
	if meta.Runtime == RuntimeComposite && a.IncludeComposite {
		for _, raw := range meta.CompositeUses {
			cu, reason := ParseUses(raw)
			if reason != "" {
				continue
			}
			cf, _ := a.inspect(cu) // one level only: children of children are ignored
			nested = append(nested, cf)
		}
	}
	return f, nested
}

func (a *Auditor) actionMeta(u UsesRef) (ActionMeta, error) {
	base := ""
	if u.Path != "" {
		base = u.Path + "/"
	}
	text, err := a.GH.FileContent(u.Owner, u.Repo, base+"action.yml", u.Ref)
	if errors.Is(err, ErrNotFound) {
		text, err = a.GH.FileContent(u.Owner, u.Repo, base+"action.yaml", u.Ref)
	}
	if err != nil {
		return ActionMeta{}, err
	}
	return ParseActionYAML(text)
}

// suggest looks for the newest tag of the action that already declares node24.
// Candidates, in order: floating majors (vN) newest first — that is what people
// pin — then the latest release tag as a fallback for actions without floating
// majors. Returns (suggestion, note).
func (a *Auditor) suggest(u UsesRef) (string, string) {
	var candidates []string
	tags, err := a.GH.Tags(u.Owner, u.Repo)
	if err != nil {
		a.noteErr(err)
	}
	candidates = append(candidates, MajorTags(tags)...)
	if latest, err := a.GH.LatestReleaseTag(u.Owner, u.Repo); err == nil && latest != "" {
		if !contains(candidates, latest) {
			candidates = append(candidates, latest)
		}
	} else if err != nil {
		a.noteErr(err)
	}
	if len(candidates) > a.MaxCandidates {
		candidates = candidates[:a.MaxCandidates]
	}
	checked := 0
	for _, tag := range candidates {
		if tag == u.Ref {
			// The pinned ref itself is outdated; still worth checking the same
			// floating tag? No: same ref, same file. Skip.
			continue
		}
		meta, err := a.actionMeta(UsesRef{Owner: u.Owner, Repo: u.Repo, Path: u.Path, Ref: tag})
		if err != nil {
			a.noteErr(err)
			continue
		}
		checked++
		if meta.Runtime == RuntimeNode24 {
			return u.Slug() + "@" + tag, fmt.Sprintf("bump %s -> @%s", u.String(), tag)
		}
	}
	if checked == 0 && len(candidates) == 0 {
		return "", "no release or vN tag found to compare"
	}
	return "", "no Node 24 version published yet - open an issue upstream"
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
