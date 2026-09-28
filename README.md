# gh-actions-node24-audit

A [GitHub CLI](https://cli.github.com) extension that finds, in one repository or in every repository of an organisation, the third-party actions whose `action.yml` still declares **`runs.using: node20`** (or `node16`) — the runtimes GitHub **removed from hosted runners on 2026-09-23** — and tells you which tag of that action already runs on **Node 24**.

```
$ gh actions-node24-audit --org my-org --exit-code
scanning 28 repositories of my-org...
REPO                  WORKFLOW         USES                                RUNTIME   SUGGESTION
my-org/api            publish.yml      actions/setup-python@v5             ! node20  actions/setup-python@v7
my-org/api            publish.yml      docker/build-push-action@v6         ! node20  docker/build-push-action@v7
my-org/api            publish.yml      docker/login-action@v3              ! node20  docker/login-action@v4
my-org/docs           verify.yml       actions/checkout@v4                 ! node20  actions/checkout@v7
my-org/docs           verify.yml       peter-evans/create-pull-request@v7  ! node20  peter-evans/create-pull-request@v8
my-org/docs           verify.yml       actions/setup-go@v6                   node24
my-org/bot            tests.yml        actions/setup-python@v5             ! node20  actions/setup-python@v7

28 repo(s), 3 workflow(s), 9 action reference(s): node20=7 node24=2
7 action reference(s) still declare node16/node20 (marked with !). These runtimes were removed from GitHub-hosted runners on 2026-09-23.
Note: this is a declarative check of action.yml (runs.using), not a behavioural test.
$ echo $?
1
```

## Why

On **2026-09-23** GitHub shipped [Node 20 is no longer available in GitHub Actions](https://github.blog/changelog/2026-09-23-node-20-is-no-longer-available-in-github-actions/): Node 20 was removed from the hosted runners and the temporary `ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION` opt-out stopped working. The guidance is "update to the latest versions of those actions that support Node 24" — but nothing tells you *which* of the `uses:` lines across your repositories still point at a `node20` action, nor which tag to bump to. Doing it by hand means opening the `action.yml` of every pinned ref.

This extension does exactly that, read-only, with the token `gh` already has.

## Install

```sh
gh extension install daniel-filius/gh-actions-node24-audit
```

Pre-compiled binaries for Linux, macOS and Windows (amd64/arm64) are attached to each [release](https://github.com/daniel-filius/gh-actions-node24-audit/releases); `gh` picks the right one. Upgrade with `gh extension upgrade actions-node24-audit`.

## Usage

```
gh actions-node24-audit                      # the repository you are in
gh actions-node24-audit --repo OWNER/REPO    # one repository
gh actions-node24-audit --org ORG            # every non-archived repo of an org (or a user)

  --json               machine-readable report (findings + summary)
  --exit-code          exit 1 when any action still declares node16/node20 (for CI)
  --include-composite  also inspect the actions used inside composite actions (one level)
  --show-skipped       list ./local, docker:// and reusable-workflow references that were not audited
  --concurrency N      parallel workflow reads (default 6)
```

What it does for every `.github/workflows/*.yml|yaml`:

1. extracts each `uses: owner/repo[/path]@ref` (skips `./local`, `docker://` and reusable workflows);
2. resolves the ref to a commit and reads `action.yml`/`action.yaml` at that ref → `runs.using`;
3. classifies it as `node16` / `node20` / `node24` / `docker` / `composite`;
4. for `node16`/`node20`: checks the action's floating major tags (`vN`, newest first) and its latest release, and reports the first one that declares `node24` — or *"no Node 24 version published yet - open an issue upstream"*.

Results are cached per `owner/repo@ref`, so an action used by 40 workflows across an org is fetched once. An org scan of ~30 repositories costs a few hundred API calls, far below the 5,000/hour of an authenticated user.

### In CI

```yaml
- name: audit actions runtime
  env:
    GH_TOKEN: ${{ github.token }}
  run: |
    gh extension install daniel-filius/gh-actions-node24-audit
    gh actions-node24-audit --repo ${{ github.repository }} --exit-code
```

(`gh` is preinstalled on GitHub-hosted runners; `GITHUB_TOKEN` is enough for public repositories and for the repository the workflow runs in.)

### JSON

```sh
gh actions-node24-audit --repo OWNER/REPO --json | jq '.findings[] | select(.outdated) | {uses, suggestion}'
```

Each finding has `repo`, `workflow`, `uses`, `sha`, `runtime`, `outdated`, `suggestion`, `note` and — for nested composite steps — `via`. `summary` carries counts by runtime plus `errors` and `rate_limit_hits`.

## How it compares

| | [actionlint](https://github.com/rhysd/actionlint) | Dependabot | gh-actions-node24-audit |
|---|---|---|---|
| Validates `runs.using` of **remote** actions referenced by `uses:` | no — only the action defined locally in the repo | no | **yes**, at the pinned ref |
| Knows which **tag** of the action already runs on Node 24 | no | bumps version by version, unaware of runtime | **yes** (`v3 → v5`) |
| Scans a whole organisation | no (per checkout) | per repository, needs config in each | **yes** (`--org`) |
| Works on actions pinned by commit SHA | — | yes | yes |
| Lints workflow syntax, expressions, shellcheck | **yes** | no | no |
| Runs offline | yes | — | no (uses the GitHub API) |

Use actionlint for lint; use this to get through the Node 20 removal.

## Honest limits

- **Declarative, not behavioural.** The tool reports what the action's `action.yml` *declares*. A `node20` action may happen to run fine on Node 24, and a `node24` action may still break because of a dependency. Treat the output as a to-do list, not a verdict.
- Composite actions are followed one level deep, only with `--include-composite`.
- Reusable workflows (`owner/repo/.github/workflows/x.yml@ref`) are listed as skipped; run the audit on that repository too.
- Private actions require a token that can read them (`gh auth status` shows the scopes).
- `--org` on a user account scans that user's repositories (the tool falls back automatically).

## Support the maintenance

This is a volunteer, MIT-licensed tool built during the Node 20 removal window. If it saved you an afternoon of opening `action.yml` files, a Pix of any amount (Brazil) keeps it maintained through the next runtime retirement:

```
00020101021226530014br.gov.bcb.pix0131danielfilho.workspace@gmail.com5204000053039865802BR5913DANIEL FILIUS6009SAO PAULO62170513GHNODE24AUDIT6304E8DE
```

Companies selling CI/runners: there is a single "Sponsored by" slot at the top of this README — open a [sponsor issue](https://github.com/daniel-filius/gh-actions-node24-audit/issues/new?template=sponsor.yml).

Free ways to help: star the repository and report a wrong suggestion via a [bug issue](https://github.com/daniel-filius/gh-actions-node24-audit/issues/new?template=bug.yml).

## Development

```sh
go test ./...
go build -o gh-actions-node24-audit . && gh extension install .   # local dev install
```

Releases are built by [`cli/gh-extension-precompile`](https://github.com/cli/gh-extension-precompile) when a `v*` tag is pushed. The CI workflow runs the tool against this very repository with `--exit-code`.

## License

[MIT](LICENSE)
