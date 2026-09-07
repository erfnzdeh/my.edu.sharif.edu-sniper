# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Report it privately through GitHub's
[private vulnerability reporting](https://github.com/erfnzdeh/my.edu.sharif.edu-sniper/security/advisories/new),
or by email to erfnzdeh@gmail.com with "sniper security" in the subject.

Include what an attacker can do, the version from the banner or
`sniper -version`, your platform, and the steps to reproduce it. Please redact
your `Authorization` token from anything you attach.

You should get an acknowledgement within a few days. Fixes are released as a
new tagged version, and the advisory is published once the fix is out.

## Supported versions

Only the [latest release](https://github.com/erfnzdeh/my.edu.sharif.edu-sniper/releases/latest)
is supported. Older binaries are not patched; upgrade instead.

## What is in scope

This is a single binary that talks to one API with one credential, so the
interesting surface is small and mostly about that credential:

- anything that leaks the `Authorization` token somewhere it should not go, in
  particular to a host other than `my.edu.sharif.edu`
- the catalogue loader, which will fetch and parse JSON from a URL, including
  a URL you pass with `-catalogue`
- the catalogue dumper's redaction check in `tools/catalogue/dump.mjs`, which
  is meant to make committing a token or a student identifier impossible
- the release workflow and the published binaries

## What is not in scope

- **The portal itself.** Weaknesses in my.edu.sharif.edu belong to Sharif
  University, not to this repository. Report those to the university, not
  here.
- **Transcripts containing your token.** That is deliberate and documented:
  the transcript is a verbatim record so a failed run can be diagnosed. They
  are gitignored, and you should delete them when you are done.
- **The token living in your shell history or environment.** Passing a
  credential with `-token` or `MYEDU_TOKEN` has the usual consequences.
- **Unsigned binaries.** Releases are unsigned, which is why macOS needs
  `xattr -c` and Windows shows a SmartScreen warning. Verify the download
  against `SHA256SUMS` on the release page.

## Handling your token

Sessions last roughly an hour, so the fastest fix for a token you think has
leaked is to wait it out or log in again. The token is a bearer credential for
your student account: treat a transcript the same way you would treat the
password.
