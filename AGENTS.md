# Working in this repository as an agent

For AI agents contributing to kraai. Humans should read
[`CONTRIBUTING.md`](CONTRIBUTING.md), which overlaps but is shorter.

This file exists because agents fail here in specific, repeatable ways. Every
rule below is written down because something went wrong, and most of them cost
real money or shipped a silent bug.

## Orient yourself first

**Start with GitHub Issues, before reading anything else.** Project state
lives there, not in markdown and not in any agent's memory:

```
gh issue list --state open
gh project item-list 1 --owner evatt-labs   # status per issue: Todo/Done
```

Check existing issues and the project board before proposing work or
picking up work.

**Read the [Architecture wiki page](https://github.com/evatt-labs/kraai/wiki/Architecture)
next.** It is the current state, audited against the code.

**Do not treat the [Decision Log](https://github.com/evatt-labs/kraai/wiki/Decision-Log)
as truth.** It is why choices were made, what was rejected, what superseded
what — deliberately a history, and parts of it describe things that were
never built. It has been wrong about the code more than once.

## The rule that matters most

**Verify claims against the code. Including claims in this repository's own
documentation.**

A documented, reasoned, confidently-worded decision is not evidence that the
thing exists. The following were all declared in a manifest or a doc,
described as load-bearing, and read by no code at all:

| | claimed |
|---|---|
| `reservedConcurrency` | the guardrail sizing Lambda against database compute |
| `naming.prefix` | prevents cross-repo collisions in a shared account |
| Neon `region` | which region the database lives in |
| `routes` | "the only door" — a custom domain as a security boundary |
| `hooks` | lifecycle extension points |
| resource imports | adopting resources created outside kraai |

The check is cheap: **grep the field name outside its struct definition.** If
the only hit is the definition, it does nothing.

## Bug classes this codebase produces repeatedly

Check for these by name. Each has bitten more than once.

**Validation that only runs sometimes.** `internal/plan`'s `decide` returns
early when a resource does not exist, *before* it consults optional
interfaces. Validation hung off one of those never ran on a fresh environment
— exactly when a typo must be caught. A typo'd setting planned clean; an
invalid value silently planned 10 resources instead of 12. Ask of any check:
*which code path guarantees this runs?*

**Decorators silently dropping optional interfaces.** `internal/resource`'s
telemetry decorator wraps every registration, so in production nothing holds
an undecorated resource. It has dropped an optional interface **three times**.
Any new optional interface needs a forwarder in `otel.go` and a case in that
package's guard test.

**Absolute thresholds in tests.** A wall-clock budget inside a parallel suite
measures the machine, not the code — observed 15x spreads on unchanged code.
Take the minimum of repeats for a latency floor, or compare two arms in one
run so machine speed cancels. Performance budgets live behind the `perf` build
tag for this reason.

**Tests that defend bugs.** Three separate tests here asserted the buggy
behaviour was correct, so they would have blocked the fix rather than catching
the bug. When a test encodes a shape you are unsure about, check it states the
intent rather than the current behaviour.

## Evidence standards

**A test that has never been seen to fail is not evidence.** Break the
behaviour deliberately, watch the test fail, restore it, confirm green. Quote
the real failure output. Bugs have shipped here with green suites and 100%
coverage because the test could not have failed.

**Unit tests are not sufficient for provider work.** They have cleared code
that could not execute a single successful call — a default path that would
have been rejected by the API on every attempt, and a permission grant whose
ARN matched nothing. If you touch a provider, run `kraai plan` against a real
manifest and report what happened.

**Do not claim a flake without proving it.** "Flaky test" is a diagnosis to
earn. One intermittent failure here was a real defect where the assertion was
right and the code was wrong; loosening it would have preserved a degradation
in operator-facing errors.

## Scope and honesty

**Stay inside the task.** Other agents frequently work in this repo
concurrently. Touching packages outside your brief causes real conflicts.

**If the brief is wrong about the code, follow the code and say so.** Briefs
here have been wrong about import cycles, dependency weights, and which API a
provider exposes. Being corrected is a good outcome; silently working around a
wrong brief is not.

**Report honestly.** If something is unfinished, blocked, or you could not
verify it, say that plainly. A spike that disproves its own premise is a
success. Do not flatter a design.

**Never route around a permission denial.** If an action is blocked, stop and
explain. Do not look for a different door to the same action.

## Mechanics

```
go build ./...
go test ./... -race            # CI runs this as its own job
golangci-lint run ./...        # v2.13.2, must be 0 issues
gofmt -l .                     # must be empty
```

`make check` runs all four plus `go vet` and the coverage floor CI enforces;
`make plan-examples` runs `kraai plan` read-only against every manifest
under `examples/`.

Two of these rules are enforced by hooks in `.claude/settings.json` when
working through Claude Code: `gofmt -w` runs after every edit, and any
`kraai apply`, `kraai destroy` or mutating `aws` CLI verb is denied unless the
session sets `KRAAI_ALLOW_MUTATE=1`. Read-only commands listed there
(`go test`, `kraai plan`, `gh pr view`, `aws sts get-caller-identity` and
similar) run without a permission prompt.

Only `cmd/kraai` may call `os.Exit`, print with `fmt.Print*`, or read
environment variables. `internal/env` is the single exception.

Comments follow Go's convention. A **doc comment** says what the thing does
and what a caller must know — it renders on pkg.go.dev, so keep it to a few
lines. An **inline `//`** explains why, at the line that is genuinely
non-obvious. **Architectural rationale** goes in the
[Architecture wiki page](https://github.com/evatt-labs/kraai/wiki/Architecture)
or a package `doc.go`, never in a function's doc comment.

**Never cite decision numbers** (`D26`, `D13`) in source — they couple code
to a document that moves. State the reason instead.

Much of the existing tree violates this: 43% of non-test lines are comments
and some doc blocks exceed 70 lines. It is being unwound. Do not imitate it,
and do not treat its verbosity as the house style.

Commits: conventional prefix, imperative, no filler, breaking changes use
`feat!:`. **No AI attribution anywhere** — no generated-with lines, no
co-author trailers, in commits or pull request bodies. **No emoji** in code,
comments, commits, or output.

**Monitor every PR you open until it resolves.** Queuing auto-merge
(`gh pr merge --squash --auto --delete-branch`) is not the end of the task —
track it until it actually merges or a required check fails, and report
which happened. Opening a PR and moving on without confirming the outcome
leaves work in an unknown state for whoever picks up next.

## Cloud access

Read-only calls against a real account are encouraged for verification —
`describe-type`, `get-resource`, `list-*`, `sts get-caller-identity`, and
`kraai plan`, which never mutates.

**Never create, update or delete real infrastructure unless the task says so
explicitly.** Never run `kraai apply` or `kraai destroy` on your own
initiative. If you believe a mutation is needed to verify something, stop and
say so instead.
