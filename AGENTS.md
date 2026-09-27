# AGENTS.md — project context for fresh agent sessions

anti-loop-proxy is an OpenAI-compatible reverse proxy (single Go module, binary
at `cmd/anti-loop-proxy`) that passes every request through to a single
upstream LLM API byte-for-byte, with one exception: for SSE streams
(`"stream": true` requests answered with `Content-Type: text/event-stream`)
it monitors `choices[].delta.content` for repeated spans. When a span of
length `[min_len, max_len]` runes repeats at least `min_count` times with at
most `max_gap` non-duplicated runes between consecutive occurrences, it cuts
the stream — emits an `event: anti_loop` block with JSON
`{"reason","count","span_len","span"}`, then `data: [DONE]`, then closes —
while the HTTP status stays `200`. Design goals: **pass-through purity**
(everything that is not a triggering SSE stream is forwarded untouched),
**fail-open on parse errors** (a malformed chunk is logged and skipped, never
a broken stream), and **bounded detection cost** (the scanner only inspects a
fixed trailing window, so per-stream cost never scales with response
length). Read this file to know what to grep first.

## Honesty and integrity

Honesty and integrity outrank every other consideration in this project —
speed, convenience, and looking competent included. Never take shortcuts that
diminish it: no fabricated results, no check skipped but reported as run, no
inflated claims, no hiding a problem you noticed. Do not lie, and do not dodge
difficult problems or blame — when something you did turns out wrong, say so
plainly, keep the evidence, and fix it. The same duty runs toward the user:
do not stay silent to appease them. If the user is wrong, is overlooking
something, or asserts something untrue, call it out directly — state your
disagreement plainly, cite the evidence (file, line, test output), and keep
asserting the accurate position after pushback, while making clear the
decision is the user's. This is not insubordination:
the user still makes every final decision, and may overrule you — but the
accurate position must be on the record first.

## Project map

Single Go module (`anti-loop-proxy`); the deployable binary is
`cmd/anti-loop-proxy`.

- **`cmd/anti-loop-proxy/`** — Entrypoint: config load (exits 1 with a
  descriptive error on invalid config), `slog` JSON logging to stdout,
  `http.Server` (10s `ReadHeaderTimeout`), graceful shutdown on
  SIGINT/SIGTERM (10s in-flight grace), and version metadata (`version`/
  `commit`/`date` vars, set via `-ldflags "-X main.version=..."` — the
  release workflow injects the tag, SHA, and UTC date).
- **`cmd/mockupstream/`** — Test-only fake OpenAI upstream for end-to-end
  verification (`POST /v1/chat/completions` with `stream`/`loop` flags;
  listens on `:9000`). **NOT part of the shipped image** (the Dockerfile
  builds only `cmd/anti-loop-proxy`).
- **`internal/config/`** — `Config` struct and `Load()`: defaults → optional
  YAML (from `ANTI_LOOP_CONFIG_FILE`, else `/etc/anti-loop-proxy/config.yaml`
  then `./config.yaml`; a missing file is not an error) → env overlay (env
  wins per key) → validation (`upstream` required absolute `http(s)://` URL
  with host, `min_count >= 2`, `1 <= min_len <= max_len <= 4096`,
  `max_gap >= 0`, `log_level` in `debug|info|warn|error`, `listen` parsed
  with `net.SplitHostPort` — a bare host gets `:8080` appended).
- **`internal/proxy/`** —
  - `detect.go` — `Params`, `Result`, `Detector`: a rune buffer fed per
    fragment; `scan()` only inspects the trailing window
    `MaxLen*(MinCount+1) + MaxGap*MinCount + MaxLen` runes, iterates span
    lengths from largest down so the **largest** triggering span wins,
    requires the final occurrence to end at the buffer tail, and chains
    non-overlapping predecessors within `MaxGap`. Once triggered, `Feed` is
    idempotent (returns the stored result, no re-scan).
  - `stream.go` — `StreamFilter`: reads the upstream SSE body line-by-line,
    forwards every line byte-for-byte, feeds `choices[].delta.content` into a
    per-choice `Detector` (keyed by `index`); on trigger writes
    `event: anti_loop` (payload span truncated to 80 runes) + `data:
    [DONE]`, logs at warn, and closes the upstream exactly once on every
    path (trigger, EOF, write error, ctx cancellation) via `closeOnce`.
    Malformed JSON in `data:` lines is logged at debug and skipped.
  - `proxy.go` — `New(cfg, log) *http.Handler`: serves `GET /healthz`
    (200 `ok`, not proxied); the `ReverseProxy` Director joins the upstream
    base path onto the request path via `path.Join` and optionally overrides
    `Authorization: Bearer` (`cfg.UpstreamAPIKey`); the wrapper handler
    sniffs the request body once for top-level `"stream"` (bodies with
    `ContentLength > 10 MB` are never read); `ModifyResponse` replaces the
    body of `text/event-stream` responses to `stream: true` requests with
    `filteredBody`, an `io.Pipe` bridge — the filter writes into the pipe
    and the proxy's own copy loop delivers the bytes, so transfer framing
    (chunked encoding) stays intact; a `requestState` context side channel
    carries `stream`/`streamCut` for the post-copy access log
    (`method`, `path`, `status`, `stream_cut`, `duration_ms`).

## Smoke / test commands (no dependencies required)

Quick validation while iterating — no external services or API keys needed:

```sh
go build ./...                     # build
go test -count=1 -race ./...       # unit tests with race detector
go vet ./...                       # vet
gofmt -l $(git ls-files '*.go') && gofumpt -l -modpath=anti-loop-proxy $(git ls-files '*.go')  # formatting, tracked files only (gofumpt from mvdan.cc/gofumpt@latest; -modpath keeps local imports out of the stdlib group)
golangci-lint run                  # full lint suite (see .golangci.yml)
govulncheck ./...                  # Go vulnerability scan
```

For the exact GitHub Actions parity (same commands, gates, and pinned tool
versions) run `bash scripts/local-ci.sh` — CI runs Go 1.24.x and enforces a
70% coverage gate.

## Working copies: always work in a worktree, never in the main checkout

**Work must NEVER be done directly in the main working copy** (the repository's
primary checkout). The main checkout is the user's and any other session's
living workspace: it may hold uncommitted work that belongs to neither the
task at hand nor the current session. Editing, staging, or committing in it
can destroy that work or silently fold it into your commit.

**ALWAYS create a dedicated worktree for any agent work**, no matter how small
the change — a single-line doc fix included:

```sh
git worktree add --detach <dir> <base-ref>   # e.g. <dir>=/tmp/opencode/<task>, <base-ref>=main
```

- Do all edits, CI runs, reviews, and commits inside that worktree. To land
  work on a branch, advance the branch ref after committing (e.g.
  `git update-ref refs/heads/<branch> <sha>`); never edit files in the main
  checkout.
- If the main checkout appears to need updating to a branch you advanced,
  sync it with `git reset` (mixed) + `git checkout HEAD -- .` — never by
  hand-editing its files — and treat any residual working-tree differences as
  other sessions' work to leave untouched.
- When the work is merged, remove the worktree (`git worktree remove <dir>`).

## Development style (non-trivial features and fixes)

For any non-trivial feature or fix — one spanning multiple commits, multiple
architecture sections, or with non-obvious interdependencies:

1. **One branch per feature.** Cut a feature branch off `main`
   (e.g. `feat/<name>` or `fix/<name>`). Develop the whole feature on that
   branch; `main` only receives finished, merged work.
2. **Sub-branch each non-trivial subset.** When the feature splits into
   subsets that are individually non-trivial (i.e. each would reasonably take
   more than one commit), give each subset its own sub-branch cut from the
   feature branch (e.g. `feat/<name>/<subset>`). Implement each sub-branch in
   its own dedicated worktree (e.g. `git worktree add <dir> feat/<name>/<subset>`)
   so its in-flight work stays isolated from the working trees of other
   sub-branches that may be developed in parallel (rule 3) — a shared checkout
   cannot hold two subsets' uncommitted work at once.
3. **Develop in parallel where the dependency DAG allows.** Map the feature's
   pieces into a DAG: which pieces can be built concurrently and which need
   other pieces complete before they can begin. Work independent branches in
   parallel to the greatest reasonable extent; start a branch only when its
   prerequisites are already merged into the feature branch.
4. **One commit per segment or sub-segment.** Each segment of the feature — or
   of a subset — gets its own commit. Even when a feature focuses on one
   thing, if the plan splits it by architecture section (e.g. config →
   proxy → docs), the commits are split the same way. The
   Git-management gates (CI checks, per-commit two-model review, attribution
   block) apply to each commit individually at merge time, not to the feature
   as a whole.
5. **Keep the review pipeline honest.** Commits may be made on the branch
   before they are reviewed, but the review pipeline must not lag: review
   findings are addressed inline via history rewriting *within the unmerged
   branch only* (see rule 10), and no branch is merged to `main` or pushed
   as a PR until **every** commit on it has been reviewed by **both** `gemma`
   and `qwen` (each commit, each reviewer — see rule 10 for running them in
   parallel) and carries proper attribution (see Git-management, steps 2–3
   and 7).
6. **Merge sub-branches into the feature branch.** When a non-trivial
   sub-branch (more than one commit) is complete, merge it back with
   `git merge --no-ff` so each subset stays visible as its own merge in
   history. A sub-feature that requires only one commit has no merge commit
   to keep it visible: merge it with a fast-forward merge
   (`git merge --ff-only`) so the single commit lands linearly on the
   feature branch.
7. **Merge the feature to `main` with `--no-ff`.** When the feature is
   complete (all sub-branches merged, all gates green), merge the feature
   branch into `main` with `git merge --no-ff`.
8. **Rebase along the way to keep merges conflict-free.** Rebase sub-branches
   (and the feature branch) as work progresses so no merge ever produces
   conflicts. This also lets the code be tested in the state it will have
   after merging — before the merge is actually made: rebase onto the merge
   target, run the CI gates against that state, then merge.
9. **Keep the todo list current at all times.** When working from a todo
   list, update it in real time, not in batches: the moment an item is
   started, mark it `in_progress`; the moment an item is finished (and
   verified), mark it `completed`. A stale todo list hides progress and
   misrepresents state — an item that is done but still shown as pending,
   or work that is underway but still shown as pending, is a reporting
   defect the same way a skipped check reported as run is one.
10. **Treat a pre-merge branch as a PR stack.** A feature or sub-branch that
    is not yet merged can consider its work as if it were a stack of PRs:
    **before** merging, a branch may run all of the necessary reviews for
    all of its commits **in parallel** (running multiple reviewers
    simultaneously against different commits) rather than serially, and
    feedback is addressed inline via history rewriting — amends and
    rebases — **WITHIN THE UNMERGED BRANCH ONLY**. Never rewrite history on
    `main` or on any branch already merged; those commits are final.
    Running reviewers in parallel is a scheduling optimization only: it does
    **not** relax the per-commit, per-reviewer gate (Git-management steps
    2–3 and 7) — a single review of multiple commits **never** counts as a
    sufficient review for any of those commits. Each commit still needs its
    **own** review **by both** `gemma` and `qwen` and its own attribution
    before merge or PR creation.

## Git management
Before merging any complete change to `main`, or pushing it as a PR:
1. Run the full CI checks defined in `.github/workflows/tests.yml` (or their local equivalent). The CI matrix runs these checks on Go 1.24.x: source tracking (`bash scripts/check-ignored-sources.sh`), formatting (`gofmt -l .` and `gofumpt -l -modpath=anti-loop-proxy .`, plus `go mod tidy` with `git diff --exit-code`), golangci-lint v2.13.1, unit tests with a **70% coverage gate** and the race detector, static analysis (`go vet` + Staticcheck + govulncheck), a strict build (`CGO_ENABLED=0`, `GOFLAGS=-trimpath`, `-buildvcs=true`, `-ldflags='-s -w'`), and a container build. To reproduce these checks locally run `bash scripts/local-ci.sh`. Fix every issue reported by these checks, then rerun `bash scripts/local-ci.sh` until it passes, before merging the change or pushing it as a PR.
2. **Require a code review by `qwen` and `gemma` as a mandatory gate before merging to `main` or pushing a PR.** Before any branch is merged to `main`, or pushed as a PR, every commit on it must be reviewed — individually, per commit — by **both** the `qwen` and `gemma` families. A multi-commit branch may run all of those reviews **in parallel** (multiple reviewers simultaneously against different commits; see Dev-style rule 10: a pre-merge branch is a PR stack), with feedback addressed inline via history rewriting within the unmerged branch only. Running reviews in parallel is purely a scheduling optimization: a single review of multiple commits **never** satisfies the per-commit requirement for any of those commits. Each commit **MUST** have its own review by both families (or, where a family is unavailable, its own review by the remaining family — below) and proper attribution before merge or PR creation (step 7). Do **not** merge to `main` or push a PR until the required reviews have been produced.

    **Required reviewers: both `qwen` and `gemma`.** Every commit requires a review from **each** of the `qwen` and `gemma` families. Prefer running each required review from a model **different from the authoring model** (e.g. a different provider, or a distinct qwen/gemma model when the author is of that family). If the preferred distinct-family review fails **twice** (the reviewer flakes, times out, or produces no usable verdict on both attempts), a review from the **authoring model's own family** is acceptable in its place — including the authoring model itself. When even that fallback is unavailable, the remaining family's review alone satisfies the requirement for that commit; a single family's repeated failure is not a blocker. The commit's attribution block records exactly which model(s) actually produced each review (step 7), and records `flake: <family>` for any required slot that ultimately had no review.

    Each reviewing model's name must appear in the review output (for example, "Review performed by `gemma*`" or another `qwen*`/`gemma*` model) so the separation is verifiable; record that name with the review findings.

    The review request must be adversarial and self-contained, containing at minimum:
    - **Tone**: direct the reviewer to be **rigorous and adversarial**: trace the diff against the surrounding code and look for correctness bugs, race conditions, edge cases, and regressions.
    - **Verification**: permit (and for nontrivial claims, expect) the reviewer to run the project's tests to verify claims — e.g. the affected test files via `go test -count=1 -race ./<affected-package>/...`, or the full suite when needed (`go test -count=1 -race ./...`).
    - **Change context**: what the code did before, what it does now, and why (including the relevant non-obvious invariants the change must preserve).
    - **User requirements**: the user-approved requirements or decisions the change implements, so the reviewer can judge intent, not just implementation — or, where the change implements none (agent-initiated fixes/refactors), what motivated it.
    - **Review scope**: an explicit check-at-minimum list tailored to the change (boundary conditions, consumer consistency, off-by-one risks, population/semantics of new parameters, test meaningfulness, stale docs/comments — as applicable).
    - **Strict reply format**: `VERDICT: APPROVE | REQUEST CHANGES | REJECT`, a `REVIEWER MODEL:` line, a `FINDINGS:` header with findings as bullets prefixed `[blocker|major|minor|nit]` with file:line references, then a short overall assessment.

    The review must cover correctness, adherence to existing codebase patterns,
    the CI/coverage results, and any security or regression risks.

    **Validate review feedback before acting on it.** Reviewer findings are
    claims, not commands: before fixing anything, verify the claim against
    the actual code (trace the cited path, run the suggested repro or the
    affected tests, e.g. `go test -count=1 -race ./<affected-package>/...`).
    Address the finding only when it checks out; invalid findings are
    recorded as false positives and dropped (or pushed back on), never blindly
    applied. Valid `major`-or-above findings must be addressed before the
    commit proceeds; valid `minor`/`nit` findings are worth addressing when
    reasonable. Reviewers differ in reliability — `gemma` in particular is
    prone to false positives — so extra skepticism toward its findings is
    expected, and a `REQUEST CHANGES` verdict never by itself blocks a merge
    (step 3 governs the outcome).
3. **Present the review results to the user and ask how to proceed.** Surface
    **each** reviewer's findings and recommendation (both `gemma` and `qwen`
    where both were run) to the user in exactly this shape: a
    `VERDICT: APPROVE | REQUEST CHANGES | REJECT` line and a `REVIEWER MODEL:`
    line naming the reviewing model, followed by a bulleted findings list in
    which every bullet is prefixed with its severity (`[blocker]`, `[major]`,
    `[minor]`, or `[nit]`); then ask the user how to proceed. Before
    presenting, validate the findings against the code (see the validation
    note in step 2) and mark which check out; valid `major`-or-above findings
    must be addressed (and the commit amended) before presenting. When a
    reviewer's `REQUEST CHANGES` or other findings turned out to be false
    positives, say so explicitly — the verdict is disclosed accurately as
    given, and the commit message clarifies that the `REQUEST CHANGES` was
    due to false positives (step 7) rather than masking or softening the
    verdict. A `REQUEST CHANGES` does not by itself prevent merging when its
    findings are invalid; it does block merging while valid `major`+ findings
    remain unaddressed. Options include merging or pushing as-is, revising
    per the review, or abandoning. Do not treat a clean or critical review as
    an automatic decision — the user's final call is definitive and cannot be
    overridden.
4. Update all relevant documentation to reflect the new reality, including `AGENTS.md`, `README.md`, `CHANGELOG.md`, `config.example.yaml`, and any other checked-in documentation affected by the change.
5. Confirm the documentation and runtime metadata agree, then merge the complete change to `main` or push it as a PR only after the user has decided how to proceed from the review.
6. When adding a footer or co-author attribution, include the agent name (e.g. OpenCode, FreeBuff, CodeBuff, Hermes Agent, etc.) and model name (e.g. GPT-5.6 Luna, Big Pickle, DeepSeek v4 Flash 0731, etc.)
7. **Attribute every commit message immediately after its detail body — truthfully.** Every commit message must include, immediately after the commit message detail (the explanatory body) and prior to any footer (such as a `Co-Authored-By:` block), an attribution block of exactly this form:

    ```
    Written by: [model] ([agent])
    Reviewed by: [review-model-1] ([review agent]) — VERDICT   # one line per reviewer that actually ran
    Reviewed by: [review-model-2] ([review agent]) — VERDICT
    ```

    One blank line separates the block from the message detail above (the
    explanatory body when one exists, otherwise the subject line) and from any
    footer below. `[model]` / `[review-model]` are human-readable display names,
    e.g. `GLM-5.3 Flash` / `Beast Qwen3.8 27B`; the parenthesized agent is the
    tool that produced the change or ran the review (e.g. `CodeBuff`, `OpenCode`,
    `FreeBuff`) and is included on all lines whenever the agent is known.
    The `Reviewed by:` lines — **one per reviewer, in the order the reviewers
    ran** — name the reviews of **this commit's diff** produced for it
    (normally one per family: `qwen` and `gemma`), and each `VERDICT` is
    that reviewer's actual verdict surfaced in step 3 from the step-2 review
    (`APPROVE`, `REQUEST CHANGES`, or `REJECT`) — the final verdict for that
    reviewer when the change went through multiple review rounds. A
    same-family review (the author's own family reviewing its own commit,
    allowed only after step 2's distinct-family attempts failed twice) is
    recorded exactly like any other. When a family's review ultimately could
    not be produced (step 2's fallback), the block carries no `Reviewed by:`
    line for that family and instead notes `flake: <family>` so the record
    shows which required slot went unreviewed. A `REQUEST CHANGES` verdict
    whose findings were validated as false positives is still recorded
    verbatim; the commit message detail (or a short `Note:` line after the
    block) clarifies that the `REQUEST CHANGES` was due to false positives,
    so the record shows both the true verdict and the accurate outcome.

    Each `Reviewed by:` line is an evidence-bearing assertion, not a
    formality: it must name a step-2 reviewer of **this commit's diff**,
    obtained **before** the commit is merged to `main` or pushed as a PR.
    Same-family reviews are permitted (step 2), but only where the distinct-
    family preference genuinely could not be met; the line must still name a
    real model that produced a real review of this diff. A commit created
    before its review is legitimate (see step 2 and Dev-style rules 5 and
    10), but a `Reviewed by:` line may only ever describe a review that has
    actually happened: never include it on a change that has not been
    reviewed, never reuse another diff's review, and never fill it in
    prospectively — if the reviews have not happened yet, the merge or PR
    does not happen yet (step 2), and the lines are added or corrected via
    history rewriting within the unmerged branch (Dev-style rule 10).
    Formatting-only or trivially mechanical changes are not exempt. This is
    an honesty-based gate: the message alone does not let a reader
    mechanically verify the claim, so hardening beyond wording (e.g. a
    pre-commit hook validating a review artifact) remains an option if
    violations recur.

## Dependency policy

Prefer adding a well-maintained dependency over rolling your own, especially
for complex problems and for areas where a hand-rolled implementation would
only approximate the solution.

When considering a dependency:

- Verify it is actively maintained, has a compatible license, and fits the
  problem.
- Prefer small, focused libraries over pulling in a large framework for one
  function.
- Keep roll-your-own code only where a dependency genuinely doesn't cover the
  need.

## Known gotchas (each is a real invariant; read before touching these areas)

1. **The upstream base path is joined with the request path — point
   `ANTI_LOOP_UPSTREAM` at the root, not at `.../v1`.** The Director in
   `internal/proxy/proxy.go` computes `req.URL.Path =
   path.Join(upstream.Path, req.URL.Path)`. With
   `ANTI_LOOP_UPSTREAM=https://api.openai.com`, a request to
   `/v1/chat/completions` reaches `https://api.openai.com/v1/chat/completions`.
   Point the upstream at the root and have clients send `/v1/...` paths;
   pointing it at `.../v1` makes the paths double up.
   (`internal/proxy/proxy.go` `Director`)

2. **Stream filtering is gated on BOTH the response `Content-Type:
   text/event-stream` AND the request body `"stream": true`.**
   `ModifyResponse` returns nil (byte-for-byte passthrough) unless both hold.
   A `stream: true` request whose upstream answer is not SSE — e.g. an
   upstream error JSON — passes through unfiltered, as does any SSE response
   to a non-stream request. Do not "helpfully" filter one side alone.
   (`internal/proxy/proxy.go` `ModifyResponse`)

3. **Request bodies with `ContentLength > 10 MB` are never read for sniffing
   — no filtering.** `sniffStream` (cap `maxSniffBytes = 10 << 20`) returns
   false without reading the body when the known length exceeds the cap, so
   large bodies pass through untouched even if they set `"stream": true`.
   (`internal/proxy/proxy.go` `sniffStream`)

4. **The filtered body pumps the filter through an `io.Pipe`; do not
   "simplify" it by writing to the `ResponseWriter` directly.**
   `filteredBody.Read` starts the `StreamFilter` once, and the filter writes
   into the pipe while the `ReverseProxy`'s own copy-to-client loop reads from
   it. Writing straight to the client `ResponseWriter` would race that copy
   loop and bypass the chunked encoder (transfer framing breaks). The pipe
   reader's `Close` is also how a client disconnect unblocks the filter.
   (`internal/proxy/proxy.go` `filteredBody`)

5. **Detection lengths are runes, not bytes; spans longer than `max_len` are
   undetectable by design.** The detector buffer, span lengths, and gaps are
   all counted in runes (a CJK character counts as 1), and `scan()` only ever
   inspects the trailing window
   `MaxLen*(MinCount+1) + MaxGap*MinCount + MaxLen` runes — an extremely long
   repetition never triggers.
   (`internal/proxy/detect.go` `scan`)

6. **The `anti_loop` event is an SSE extension — strict OpenAI clients ignore
   it; the stream still ends with `[DONE]`, so the status stays 200.** On
   trigger the filter emits `event: anti_loop` (payload `span` truncated to 80
   runes) followed by `data: [DONE]`; the response status was already set by
   the upstream copy path and is never altered. The trailing `[DONE]` is what
   lets conforming clients finish cleanly.
   (`internal/proxy/stream.go` `writeTrigger`)

7. **Malformed JSON in `data:` lines is logged at debug and skipped — never
   break the stream on a bad chunk.** The line is still forwarded
   byte-for-byte; only the detector feed is skipped. Fail-open on parse
   errors is a design goal; do not turn this into a hard failure.
   (`internal/proxy/stream.go` `process`)

8. **Config precedence is env > file > default; a missing config file is not
   an error.** `config.Load()` overlays defaults → YAML (first existing of
   `ANTI_LOOP_CONFIG_FILE`, `/etc/anti-loop-proxy/config.yaml`, `./config.yaml`
   when the env var is unset) → env vars (each non-empty env var wins per
   key). Only validation failures (missing/invalid `upstream`, out-of-range
   thresholds, bad `log_level`, malformed `listen`) are startup errors.
   (`internal/config/config.go` `Load`/`validate`)

## Authoritative docs (read on demand)

- `README.md` — quickstart, configuration table, the `anti_loop` event
  contract, operational notes, caveats.
- `CHANGELOG.md` — release history (Keep-a-Changelog format; the release
  workflow uses it as the GitHub Release body).
- `.github/workflows/tests.yml` — CI jobs and gates (source tracking,
  formatting, lint, tests + 70% coverage + race, static analysis, strict
  build, container build).
- `.github/workflows/release.yml` — release on `v*` tags: linux/amd64 +
  arm64 binaries with checksums, container image pushed to `ghcr.io`, GitHub
  Release with `CHANGELOG.md` as body.
- `scripts/local-ci.sh` — exact local parity for the CI jobs.
