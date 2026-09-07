## What this changes

<!-- One change per pull request. Say what it does and why. -->

## Why

<!-- The problem it solves. Link the issue if there is one. -->

## How it was verified

<!--
Be specific and honest. All of these are acceptable answers:
  built and ran against a live registration window
  ran against the API outside a window, so only the clock sync path
  logic only, not exercised against the server
-->

## If it touches timing

<!--
globalGap, the per course cooldown, the 429 backoff and the late bias are
measurements. Delete this section if you did not touch them; otherwise give
the numbers, the network you measured from, and the sample size.
-->

## Checklist

- [ ] `gofmt -l ./cmd ./tools` prints nothing
- [ ] `go vet ./...` and `go build ./...` are clean
- [ ] No new dependency, and `cmd/sniper` is still standard library only
- [ ] No token, transcript or student identifier in the diff
- [ ] README or `docs/reference` updated if behaviour changed
