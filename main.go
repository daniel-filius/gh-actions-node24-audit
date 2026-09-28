// Command gh-actions-node24-audit is a GitHub CLI extension that lists the
// third-party actions used by a repository (or every repository of an org)
// whose action.yml still declares `runs.using: node20` or `node16` — runtimes
// GitHub removed from hosted runners on 2026-09-23 — and points at the tag
// that already declares node24.
//
// Usage:
//
//	gh actions-node24-audit                      # current repository
//	gh actions-node24-audit --repo OWNER/REPO
//	gh actions-node24-audit --org ORG [--json] [--exit-code] [--include-composite]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/cli/go-gh/v2/pkg/repository"
)

// version is overwritten at build time by gh-extension-precompile (-ldflags).
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gh actions-node24-audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		repoFlag  = fs.String("repo", "", "audit one repository, OWNER/REPO (default: the current repository)")
		orgFlag   = fs.String("org", "", "audit every non-archived repository of an organisation or user")
		jsonFlag  = fs.Bool("json", false, "print a JSON report instead of a table")
		exitCode  = fs.Bool("exit-code", false, "exit 1 when any action still declares node16/node20")
		composite = fs.Bool("include-composite", false, "also inspect the actions used inside composite actions (one level)")
		showSkip  = fs.Bool("show-skipped", false, "list local, docker:// and reusable-workflow references that were not audited")
		conc      = fs.Int("concurrency", 6, "parallel workflows being inspected")
		ver       = fs.Bool("version", false, "print version and exit")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "gh actions-node24-audit %s\n\n", version)
		fmt.Fprintln(stderr, "Find GitHub Actions that still declare runs.using node16/node20 (removed from hosted runners on 2026-09-23) and the tag that already runs on Node 24.")
		fmt.Fprint(stderr, "\nUsage: gh actions-node24-audit [--repo OWNER/REPO | --org ORG] [--json] [--exit-code] [--include-composite]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *ver {
		fmt.Fprintln(stdout, version)
		return 0
	}
	if *repoFlag != "" && *orgFlag != "" {
		fmt.Fprintln(stderr, "error: --repo and --org are mutually exclusive")
		return 2
	}

	gh, err := NewGitHub()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n(run `gh auth login` first)\n", err)
		return 2
	}
	cached := newCached(gh)

	var repos []string
	switch {
	case *orgFlag != "":
		names, err := cached.ListRepos(*orgFlag)
		if err != nil {
			fmt.Fprintf(stderr, "error: list repositories of %s: %v\n", *orgFlag, err)
			return 2
		}
		for _, n := range names {
			repos = append(repos, *orgFlag+"/"+n)
		}
		if !*jsonFlag {
			fmt.Fprintf(stderr, "scanning %d repositories of %s...\n", len(repos), *orgFlag)
		}
	case *repoFlag != "":
		if !strings.Contains(*repoFlag, "/") {
			fmt.Fprintln(stderr, "error: --repo must be OWNER/REPO")
			return 2
		}
		repos = []string{*repoFlag}
	default:
		cur, err := repository.Current()
		if err != nil {
			fmt.Fprintf(stderr, "error: not inside a GitHub repository; pass --repo OWNER/REPO or --org ORG (%v)\n", err)
			return 2
		}
		repos = []string{cur.Owner + "/" + cur.Name}
	}

	a := &Auditor{GH: cached, IncludeComposite: *composite, Concurrency: *conc}
	rep, err := a.Run(repos)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}

	if *jsonFlag {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
	} else {
		printTable(stdout, rep, len(repos) > 1, *showSkip)
	}
	if rep.Summary.RateLimits > 0 {
		fmt.Fprintf(stderr, "warning: %d request(s) hit the API rate limit; results may be incomplete\n", rep.Summary.RateLimits)
	}
	if *exitCode && rep.Summary.Outdated > 0 {
		return 1
	}
	return 0
}

func printTable(w io.Writer, rep *Report, multiRepo, showSkipped bool) {
	if len(rep.Findings) == 0 {
		fmt.Fprintln(w, "no remote actions found in .github/workflows")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if multiRepo {
		fmt.Fprintln(tw, "REPO\tWORKFLOW\tUSES\tRUNTIME\tSUGGESTION")
	} else {
		fmt.Fprintln(tw, "WORKFLOW\tUSES\tRUNTIME\tSUGGESTION")
	}
	for _, f := range rep.Findings {
		mark := "  "
		if f.Outdated {
			mark = "! "
		}
		uses := f.Uses
		if f.Via != "" {
			uses = "  └ " + uses
		}
		if uses == "" {
			uses = "-"
		}
		sug := f.Suggestion
		if sug == "" && f.Note != "" {
			sug = f.Note
		}
		wf := strings.TrimPrefix(f.Workflow, ".github/workflows/")
		if multiRepo {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s%s\t%s\n", f.Repo, wf, uses, mark, f.Runtime, sug)
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%s%s\t%s\n", wf, uses, mark, f.Runtime, sug)
		}
	}
	if showSkipped && len(rep.Skipped) > 0 {
		fmt.Fprintln(tw, "\nSKIPPED\tWORKFLOW\tUSES\tREASON")
		for _, s := range rep.Skipped {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Repo, strings.TrimPrefix(s.Workflow, ".github/workflows/"), s.Uses, s.Reason)
		}
	}
	tw.Flush()

	s := rep.Summary
	keys := make([]string, 0, len(s.ByRuntime))
	for k := range s.ByRuntime {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, s.ByRuntime[k]))
	}
	fmt.Fprintf(w, "\n%d repo(s), %d workflow(s), %d action reference(s): %s\n", s.Repos, s.Workflows, s.Actions, strings.Join(parts, " "))
	switch {
	case s.Outdated == 0 && s.Errors == 0:
		fmt.Fprintln(w, "OK - every action declares node24, docker or composite.")
	case s.Outdated == 0:
		fmt.Fprintf(w, "no node16/node20 found, but %d reference(s) could not be inspected.\n", s.Errors)
	default:
		fmt.Fprintf(w, "%d action reference(s) still declare node16/node20 (marked with !). These runtimes were removed from GitHub-hosted runners on 2026-09-23.\n", s.Outdated)
	}
	fmt.Fprintln(w, "Note: this is a declarative check of action.yml (runs.using), not a behavioural test.")
}
