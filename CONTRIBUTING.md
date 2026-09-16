# Contributing to kraai

Thanks for considering it. This document covers what you need to know before
opening a pull request, and a few standards that are stricter than most Go
projects — stated up front so they are not a surprise in review.

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## Start here

**Read [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) first.** It describes
what kraai actually is today, audited against the code, and keeps
decided-but-unbuilt work in a separate section so you can trust every line of
the main body.

[`docs/BLUEPRINT.md`](docs/BLUEPRINT.md) is the decision record — why each
choice was made and what it replaced. It is deliberately a history, and parts
of it describe things that were never built. Read it for *why*, not for *what
is true*.

## Getting set up

    go build ./...
    go test ./...
    go test ./... -race          # CI runs this as its own job
    golangci-lint run ./...      # v2.13.2

`kraai plan` against a real manifest needs provider credentials. `kraai plan`
never mutates anything and never takes a lock, so it is safe to run against a
real account while you are finding your way around.

## Standards

Most of these exist because something went wrong once.

**Write down the reasoning, not just the code.** Doc comments here explain
*why* a thing is the way it is, and name the alternative that was rejected.
Calibrate against `internal/resource/registry.go` or `internal/plan/doc.go`.
This is the single most common review comment.

**A test that has never been seen to fail is not evidence.** For anything
load-bearing, break the behaviour deliberately, watch the test fail, restore
it, and confirm it passes. Quote the real failure output in the pull request.
Several bugs here shipped with green suites and 100% coverage because the
test could not have failed.

**Run it against reality.** Unit tests have repeatedly cleared code that could
not execute a single successful call. If a change touches a provider, plan
against a real manifest and say what happened.

**Validation must run unconditionally.** Hanging a check off an optional
interface means it only fires when something happens to consult that
interface. That has caused silent failures here more than once.

**No AI attribution.** No `Generated with`, no co-author trailers, no tool
footers in commits or pull request descriptions.

**Conventional commits**, imperative mood, no filler. Breaking changes use
`feat!:`. Commit messages and PR bodies are read by people who were not in
the conversation that produced them.

**No emoji** in code, comments, commits, pull request bodies, or user-facing
output.

**No HashiCorp dependencies, ever** — nothing under `github.com/hashicorp/*`.
kraai competes directly with Terraform. Note that HashiCorp-free is necessary
but not sufficient: a proposed replacement was rejected after inspection for
pulling 148 modules including TLS-fingerprint-evasion machinery.

**Dependencies must earn their place.** There is no cap, but dependency count
is security surface for a CLI that holds cloud credentials. Say what you
checked.

## Boundaries the linter enforces

Only `cmd/kraai` may call `os.Exit`, print to stdout with `fmt.Print*`, or
read environment variables. Everywhere else returns errors and writes to an
injected writer. `internal/env` is the one exception, because centralising
environment access is the point of it.

## Tests

`go test ./... -race` must pass. The race detector runs as its own CI job
because the core is concurrent by design — waves execute in parallel,
credentials cross between them, and registrations can declare mutual-exclusion
scopes.

Performance budgets live behind a build tag, because a wall-clock threshold
inside a parallel suite measures the machine rather than the code:

    go test -tags perf -run TestWarmCallOverheadBudget ./internal/plugin/

Run that on an idle machine when you are looking for a regression, not in CI.

## Pull requests

One concern per pull request. Explain what you changed, what you rejected and
why, and how you verified it. If you found something that contradicts the
issue or the docs, follow the code and say so — the documentation has been
wrong before and saying so is more useful than working around it.

Draft pull requests are welcome for work in progress, and spikes that
deliberately do not merge are a normal outcome. One recently disproved its own
premise; that was a success.

## Finding something to work on

Issues labelled [`good first issue`](https://github.com/evatt-labs/kraai/labels/good%20first%20issue)
are scoped and self-contained.

Other labels worth knowing:

- **`unbuilt`** — decided and reasoned about, no implementation exists.
- **`dead-field`** — a field the manifest parses that no code reads. There
  have been six. They are each small, well-defined, and genuinely useful.
- **`workstream`** — part of a sequenced plan from a document in
  `docs/proposals/`. Read the proposal before starting; the sequencing is
  usually load-bearing.

If you are unsure whether something is wanted, open an issue before writing
code.
