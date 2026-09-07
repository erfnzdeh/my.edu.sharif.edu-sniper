# Contributing

Thanks for looking at this. The tool runs once a semester, under time
pressure, on someone's actual registration, so the bar for changes in the hot
path is higher than the size of this codebase suggests. What follows is mostly
about that.

Start with the [README](README.md) for what the tool does, and with
[docs/reference/server-contract.md](docs/reference/server-contract.md) for the
portal behaviours that every design decision is reacting to.

---

## Getting set up

You need Go 1.22 or newer, and nothing else. There are no dependencies and no
build tooling:

```
go build -o sniper ./cmd/sniper
```

Regenerating the catalogue additionally needs Node 22 or newer, for the global
`WebSocket`:

```
MYEDU_TOKEN='<Authorization header>' node tools/catalogue/dump.mjs
```

Before you push:

```
gofmt -l ./cmd ./tools
go vet ./...
go build ./...
```

`gofmt -l` should print nothing. There is no test suite yet; the behaviour
that matters is timing against a live server, which is hard to fake and
impossible to reach outside a registration window. If you add something with
pure logic in it, a `_test.go` next to it is very welcome.

---

## Things worth knowing before you change anything

**`cmd/sniper` is one file, standard library only.** That is deliberate: the
binary has to be readable end to end by someone who is about to trust it with
their registration, and it has to build anywhere with a plain `go build`.
Please do not add a dependency. If you think one is genuinely needed, open an
issue first and say what it buys.

**The timing constants are measurements, not preferences.** `globalGap`, the
per course cooldown, the `429` backoff and the 100ms late bias each come from
observed behaviour, written up in
[docs/reference/rate-limits.md](docs/reference/rate-limits.md). Changing one
means bringing new numbers: how you measured, from which network, and over how
many requests. A change that only makes the tool feel faster is a change that
loses seats.

**Arriving early is expensive.** A request 100ms before the window is rejected
and burns that course's five second cooldown. A request 100ms late costs
100ms. Any change to the fire time calculation has to keep that asymmetry.

**Do not add load.** No parallel bursts, no polling loops toward the window, no
retry storms. The edge limits one request per second per client and the tool
stays inside that. Patches that try to win by sending more will not be merged.

**Never commit a token or a transcript.** Transcripts record requests and
responses verbatim, including your `Authorization` header, which is why
`snipe-*.log` is gitignored. The catalogue dumper copies four whitelisted
fields per course and refuses to write output containing a token or a student
identifier. Keep it that way. If you paste a log into an issue, redact the
token first.

**The catalogue is data, not code.** `docs/api/courses.json` is regenerated
once a semester from the portal. Do not hand edit it; rerun `dump.mjs` and
commit the result on its own, so the diff stays reviewable.

---

## Pull requests

- One change per pull request. A behaviour change and a refactor in the same
  diff are hard to review and worse to revert.
- Commit messages: imperative mood, sentence case, no prefix, explaining the
  change rather than the file touched. For example "Make a manual run a dry
  run instead of a failed release".
- Update the README or `docs/reference` in the same pull request when
  behaviour changes. The documentation is the point of this repository as much
  as the binary is.
- Say how you verified it. "Built and ran against a live window" and "logic
  only, not exercised against the server" are both fine answers; leaving it
  unsaid is not.

Unfinished work is welcome as a draft pull request. Say what you are unsure
about and it will get looked at.

---

## Reporting things

Bugs and ideas both go to
[issues](https://github.com/erfnzdeh/my.edu.sharif.edu-sniper/issues). For a
bug, include the version from the banner or `sniper -version`, your platform,
and the relevant transcript lines with the token removed.

Security issues go through [SECURITY.md](SECURITY.md) instead, not the issue
tracker.

By contributing, you agree that your contributions are licensed under the
[MIT License](LICENSE), and you are expected to follow the
[Code of Conduct](CODE_OF_CONDUCT.md).
