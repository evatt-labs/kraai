## What this changes

<!-- What the change is, and why. If it fixes an issue, link it. -->

## What you rejected

<!--
The alternative you considered and did not take, and why. This project
records rejected alternatives alongside decisions; skipping this is the most
common review comment.
-->

## How you verified it

<!--
For anything load-bearing: break the behaviour, watch the test fail, restore,
confirm green — and quote the real failure output here. A test that has never
been seen to fail is not evidence.

If this touches a provider, say what happened when you ran it against a real
manifest.
-->

## Checklist

- [ ] `go test ./... -race` passes
- [ ] `golangci-lint run ./...` is clean
- [ ] Doc comments explain *why*, not just *what*
- [ ] No AI attribution, no emoji
- [ ] Anything I found that contradicts the issue or the docs is called out rather than worked around
