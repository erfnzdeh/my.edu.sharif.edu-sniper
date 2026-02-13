# API reference

Reverse engineering notes for my.edu.sharif.edu, kept separate from the main
[README](../../README.md) so that document can stay about the sniper itself,
how to run it and why it is built the way it is, rather than the portal's API
surface.

Start with [server-contract.md](server-contract.md) for the one page version.

Gathered on 2026-09-07 from the production frontend bundle plus live probing
with a student token. None of it is documented by the university, and all of it
can change without notice if the portal is rebuilt.

| Document | Covers |
| --- | --- |
| [server-contract.md](server-contract.md) | the short version: every measured behaviour and what it forces the client to do |
| [http-api.md](http-api.md) | the five endpoints, request and response shapes, why `/courses/*` are not endpoints |
| [websocket.md](websocket.md) | the only source of course data: message types, 60 second cadence, `userState` and course schemas |
| [rate-limits.md](rate-limits.md) | all three limiters, measured, and what `globalGap` should probably be |
| [auth.md](auth.md) | JWT shape, the roughly one hour lifetime, and why the WebSocket ignores it |
| [error-codes.md](error-codes.md) | how the sniper triages each code, plus every code with its Persian text and meaning |

## Findings worth knowing before reading the code

1. **The API is five routes.** Everything under `/courses/*` is react-router.
2. **Course data exists only on the WebSocket**, pushed as a delta about every
   60 seconds. That tick is the floor on how fast a freed seat can be noticed.
3. **Every request to the host spends a rate limit token, but only `/api/reg`
   is rejected** when the budget is gone. The limiter is keyed on IP.
4. **The token lasts about an hour over HTTP; the WebSocket does not check its
   age at all.** Watchers can run indefinitely, only firing needs a fresh one.
5. **`add` is quota free.** `remainingActions` counts only `remove` and `move`.
6. **`/api/reg` is asynchronous.** The real outcome appears in
   `userState.jobs[]`, not in the HTTP response.

## Verification status

Everything here was observed live except where noted. Two things were not
exercised:

- **The firing path against an open window.** The account's `registrationTime`
  (2026-08-08 08:00) had passed, so `add` returns `NO_REGISTRATION_TIME`.
  Anything about behaviour once registration is open is inference.
- **The `BLOCKED` threshold.** Not probed on purpose, since tripping it locks
  the student ID out for minutes.

No `add`, `remove` or `move` was executed against a real course. Probes used
either an invalid action with a non-existent course id, or an expired token, so
they were rejected before reaching the registration logic. `remainingActions`
read 12 before and 12 after.
