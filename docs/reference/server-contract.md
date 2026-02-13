# The server contract

The measured behaviours the sniper is built around, and what each one forces
the client to do. Established by measuring the live endpoint and reading the
portal's own frontend bundle. None of it is documented by the university.

This is the summary. The rest of this directory is the detail behind it:
[http-api.md](http-api.md), [websocket.md](websocket.md),
[rate-limits.md](rate-limits.md), [auth.md](auth.md) and
[error-codes.md](error-codes.md).

| Measured behaviour | Consequence for the client |
| --- | --- |
| The edge allows **exactly one request per second, with no burst**, and rejects the rest with a real HTTP `429` and an HTML body. | A parallel burst throws most of its requests away. Pacing is the single most important thing the client does. |
| **Every** request to the host counts, including a plain `GET /`. | Connection warm up must happen well before the window, never immediately before it. |
| Application errors arrive as **HTTP 200** with an `error` field in the JSON body. | Status codes cannot be used to detect failure, apart from `429`. |
| Auth failure is `{"error":"AUTHORIZATION"}`, never a `401`. | The token check has to read the body. |
| Tokens carry **no `exp` claim** and expire server side after roughly an hour. | Expiry cannot be predicted, only observed. Capture the token shortly before you need it. |
| A request before your window is rejected with `NO_REGISTRATION_TIME`, and that takes precedence over course validation. | Course codes cannot be checked against `/api/reg` ahead of time. |
| The response carries `time` and `registrationTime` as Unix milliseconds. | The client can measure its clock offset without trusting its own. |
| `registrationTime` **goes stale** and can point at a window weeks past. | The target time often has to come from you. There are two daily windows, 08:00 and 16:00. |
| `jobs` is ordered **newest first** and accumulates for the whole session. | Reading it backwards returns the oldest result for a course and misses a later success. |
| A job with **no `result`** is still queued server side. | Absence of a result is not failure. |
| The response describes **every** job, not just the one you asked about. | One request can reveal that a different course already landed. |
| `units` is **range checked** against the course, and a mismatch fails with `INCORRECT_UNIT_NUMBER`. | Units cannot be guessed. See the catalogue below. |
| `remainingActions` is a real quota, with its own `NO_REMAINED_ACTION` code. | Worth watching during add and drop. |
| The course catalogue is served **only over a WebSocket**, never REST. | Hence the separate catalogue tool. |
| A cold TLS handshake costs about 650ms against 170ms on a pooled connection. | Worth warming, at the right moment. |

Both `time` and `registrationTime` are Unix timestamps, so comparing them is
timezone independent. The portal is only reachable from inside Iran, so local

Both `time` and `registrationTime` are Unix timestamps, so comparing them is
timezone independent. The portal is only reachable from inside Iran, so local
time and server time agree.

## Where each row is covered in detail

| Row | Detail |
| --- | --- |
| one request per second, `429` on the rest | [rate-limits.md](rate-limits.md), including a re-measurement on a warm pooled connection |
| every request counts, including `GET /` | [rate-limits.md](rate-limits.md#2-which-requests-count-and-which-get-rejected), where counting and rejection turn out to be different sets |
| errors arrive as HTTP 200 | [http-api.md](http-api.md#conventions) |
| auth failure is a body, not a `401` | [auth.md](auth.md) |
| no `exp` claim, expires after roughly an hour | [auth.md](auth.md#lifetime-measured) |
| `NO_REGISTRATION_TIME` precedes course validation | [error-codes.md](error-codes.md#timing-and-availability) |
| `time` and `registrationTime` in milliseconds | [websocket.md](websocket.md#userstate) |
| `registrationTime` goes stale | [websocket.md](websocket.md#userstate) |
| `jobs` newest first, accumulates | [websocket.md](websocket.md#userstate) |
| a job with no `result` is still queued | [http-api.md](http-api.md#post-reg) |
| the response describes every job | [http-api.md](http-api.md#post-reg) |
| `units` is range checked | [websocket.md](websocket.md#course-object) |
| `remainingActions` is a real quota | [websocket.md](websocket.md#remainingactions-counts-removals-not-additions), which narrows it to `remove` and `move` only |
| catalogue is WebSocket only | [websocket.md](websocket.md) |
