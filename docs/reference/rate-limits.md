# Rate limits

Three independent mechanisms sit between you and a successful `add`. Only the
first one is usually visible.

Measurements below were taken on 2026-09-07 from a single residential IP,
outside a registration window. Each row is a real run, not an estimate.

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

`globalGap` is 1100ms today, which loses roughly one request in seven. That
would be a fair trade if a rejection were cheap, but in the current scheduler a
`429` backs off **every** course by 7 seconds. Paying an extra 200ms per
request to avoid a 1-in-7 chance of a 7 second stall is a large win:

- at 1.10s, an expected ~14% of requests cost 7s each
- at 1.30s, every request costs 200ms more and nothing stalls

With ten courses the last one's first attempt moves from 9.9s to 11.7s after
the window opens, against removing the risk of a 7 second global freeze in the
first few seconds. Raising `globalGap` to about 1300ms looks strictly better.
Worth re-measuring from the campus network before changing it, since the jitter
is what drives the result and it will differ from the link these numbers came
from.

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
of 1.1s leaves 2.1s between the warm up `GET` and the first `POST`, which is
comfortable.

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
is a concurrency guard, which is the reason the scheduler holds a single global
token rather than running requests in parallel.

---

## 5. The WebSocket is not rate limited

It costs one connection and pushes a delta about every 60 seconds for free.
Anything that only needs to *observe* state belongs there rather than on HTTP.
See [websocket.md](websocket.md).
