# Rate limits

Three independent mechanisms sit between you and a successful `add`. Only the
first one is usually visible.

Measurements in the numbered sections were taken on 2026-09-07 from a single
residential IP, outside a registration window. Each row is a real run, not an
estimate. The section that follows is from a live window, where the numbers are
very different.

---

## What the 2026-09-08 window actually showed

Two transcripts from the same 08:00 window, two different machines and links.
Both lost their opening shot the same way, and neither loss was the portal's
doing.

| | run A | run B |
| --- | --- | --- |
| warm up `GET` finished | 08:00:00.538 | 07:59:57.804 |
| first `POST` sent | 08:00:00.538 | 07:59:57.804 |
| result | `429` in 40ms | `429` in 7ms |

The warm up was called after the wait for the window rather than inside its own
lead, so it ran at the fire time and the first `POST` followed it in the same
millisecond. Since every request to the host increments the counter, the
opening request was guaranteed to be rejected. Both runs then froze for 7
seconds, and run A repeated the whole sequence at 08:00:08, so 16 of the first
17 seconds of the window produced nothing.

Round trip time inside the window is nothing like the quiet-hour measurements
above:

| | outside the window | inside it |
| --- | --- | --- |
| typical | 46ms to 2037ms | 2.1s to 5.1s |
| worst seen | 2037ms | timed out at 10s, twice in a row |

Both of run B's timeouts had in fact registered the course. A request that
gives up client side may still have been carried out, so the answer to a
timeout is to let the next response tell you, not to resend.

Run B also took a second `429` at 08:00:35, a full 5 seconds after its previous
request and in the same millisecond as the answer to it. Nothing the sniper
sent explains that. The limiter is keyed on the IP, so the traffic came from
alongside it: a click in the browser, or another student behind the same NAT.
The scheduler now treats a `429` as a 2 second hold on the shared token with
the course keeping its place, which is the right response either way, but a
browser tab on the portal during the window is a cost you can avoid.

Both runs also got `REPEATED_REQUEST` on a course that then landed from a job
the sniper had not knowingly queued. The job ids sit where a request sent a
second or two before the sniper's would sit, which again points at a click in
the browser, and it is why `REPEATED_REQUEST` is now read as "the portal has
it" rather than as a failure.

---

## 1. The edge limiter on `/api/reg`

nginx rejects with a genuine HTTP `429` and an HTML body, before Express sees
the request. There is no `Retry-After` and no `x-ratelimit-*` header on that
response, so a `429` tells you nothing beyond the fact that it happened.

### Rate, measured on a fresh connection per request

| Interval | Passed |
| --- | --- |
| back to back (~0.3s) | 4 / 12 |
| 1.2s | 10 / 12 |
| 1.5s | 10 / 12 |
| 2.0s | 10 / 10 |

### Rate, measured on a warm pooled connection

This is the configuration the sniper actually runs in, so these are the numbers
that matter.

| Interval | Passed | Notes |
| --- | --- | --- |
| 1.05s | 11 / 14 | |
| **1.10s** | **12 / 14** | the gap `globalGap` currently uses |
| 1.30s | 14 / 14 | clean across the whole run |

The underlying limit really is one request per second, as the README says. The
losses at 1.05s and 1.10s are not a different limit, they are jitter: measured
round trip time on the same pooled connection ranged from 46ms to 2037ms in a
single run, so a client gap of 1.10s regularly lands two requests less than a
second apart at the edge.

### Consequence for `globalGap`

`globalGap` was 1100ms, which loses roughly one request in seven. That would be
a fair trade if a rejection were cheap, but the scheduler then backed off
**every** course by 7 seconds. Paying an extra 200ms per request to avoid a
1-in-7 chance of a 7 second stall is a large win:

- at 1.10s, an expected ~14% of requests cost 7s each
- at 1.30s, every request costs 200ms more and nothing stalls

With ten courses the last one's first attempt moves from 9.9s to 11.7s after
the window opens, against removing the risk of a global freeze in the first few
seconds. `globalGap` is now 1300ms and `rateLimitBackoff` is 2s, and the gap is
exposed as `-gap` because the jitter that drives it differs by link. Still
worth re-measuring from the campus network.

---

## 2. Which requests count, and which get rejected

These are not the same set, which is easy to get wrong.

A back to back burst of 12 requests to `/api/zzz`, a path that does not exist,
returned **12 / 12 = 200**. The same burst against `/api/reg` returned
**4 / 12**. So rejection is scoped to `/api/reg`.

But counting is not. Interleaving a plain `GET /` with a `POST /api/reg` sent
150ms later, with pairs 1.35s apart:

```
GET / = 200,  reg = 429
GET / = 200,  reg = 429
GET / = 200,  reg = 429
GET / = 200,  reg = 429      (8 pairs, all identical)
```

Every pair rejected the registration call. So:

- **every request to the host increments the counter**, including `GET /`
- **only `/api/reg` is rejected** when the counter is over

This confirms the README's warning that the warm up spends a token, and that it
must not run inside the last second. `warmupLead` of 2s against a `globalGap`
of 1.3s leaves 2s between the warm up `GET` and the first `POST`, which is
enough. It is also exactly what the 2026-09-08 runs failed to do: see above.

It also means the limiter is keyed on **IP, not on token**. Alternating
authenticated and unauthenticated requests shared a single budget (11/12 at
1.5s, the same as token-only traffic). Rotating tokens does not buy throughput,
and on a shared campus NAT you may be sharing this budget with other students.

---

## 3. The Express limiter

Some responses carry:

```
x-ratelimit-limit: 200
x-ratelimit-remaining: 194
x-ratelimit-reset: 1788783689
```

200 requests per window, `reset` in epoch seconds. These headers appear
inconsistently, notably vanishing once the token is rejected at the auth layer,
so treat them as advisory. Read them when present and slow down as `remaining`
falls.

---

## 4. Application level blocking

Not probed deliberately, since tripping it during a live window would be
expensive. The frontend ships user facing strings for all of it:

| Code | Persian text | Meaning |
| --- | --- | --- |
| `BLOCKED` | شماره‌ی دانش‌جویی شما به علت ارسال درخواست بیش از حد مجاز محدود شده است. لطفا دقایقی دیگر مراجعه نمایید. | your student ID is restricted for excess requests, try again in a few minutes |
| `TOO_MANY_REQUESTS` | تعداد درخواست‌های همزمان بیش از حد مجاز بوده است. | too many concurrent requests |
| `PLEASE_WAIT` | مشکل در سامانه‌ی آموزش، لطفا منتظر بمانید. | upstream آموزش problem, wait |

`BLOCKED` is keyed on the **student ID**, so unlike the edge limiter it follows
you across networks and cannot be escaped by reconnecting. `TOO_MANY_REQUESTS`
is a concurrency guard. The scheduler still sends on a single global token, so
it never exceeds one request per gap however many answers are outstanding, and
the number outstanding is capped anyway at one per course. Neither code has been
seen live. If one shows up, `-inflight` sets a hard cap and `-inflight 1`
restores serial behaviour.

---

## 5. The WebSocket is not rate limited

It costs one connection and pushes a delta about every 60 seconds for free.
Anything that only needs to *observe* state belongs there rather than on HTTP.
See [websocket.md](websocket.md).
