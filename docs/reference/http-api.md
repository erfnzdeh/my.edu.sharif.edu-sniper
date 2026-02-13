# HTTP API

Base `https://my.edu.sharif.edu/api`. Reverse engineered from the production
frontend bundle (`/static/js/main.098a80f7.chunk.js`, build dated
2026-02-18) and confirmed against the live server.

**There are exactly five routes.** Four HTTP, one WebSocket upgrade.

| Method | Path | Auth | Body |
| --- | --- | --- | --- |
| `GET` | `/auth/captcha` | no | none |
| `POST` | `/auth/login` | no | `{username, password, challenge, captcha}` |
| `POST` | `/reg` | yes | `{action, course, units}` |
| `POST` | `/user/favorite` | yes | `{course, marked}` |
| `GET` (upgrade) | `/ws?token=<urlencoded jwt>` | query string | none |

## Conventions

- Auth is the raw JWT in an `authorization` header, with **no `Bearer` prefix**.
- No cookies, no CSRF token. `access-control-allow-origin: *` and
  `access-control-allow-headers: *`.
- Application errors are **HTTP 200** with an `error` field. The only real
  status code you will see is nginx's `429`.
- Unknown paths under `/api` return HTTP 200 with the literal body `NOT FOUND!`.

## `/courses/*` are not endpoints

`/courses/offered`, `/courses/marked` and `/courses/enrolled` are react-router
client routes. Requesting them under `/api/` returns `NOT FOUND!`. This is the
most common wrong turn when reading the bundle, because the strings sit right
next to the real API paths.

The course list is available **only over the WebSocket**.

---

## `GET /auth/captcha`

```json
{ "challenge": "1b0532c7-f7c5-4c94-8c9f-b9760f15ae0f",
  "data": "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"150\" height=\"50\">..." }
```

Unauthenticated. Returns an inline SVG captcha of about 12.8 KB and a UUID
challenge id, which is submitted back to `/auth/login` as `challenge`.

The image is built from `<path>` elements rather than text, so it is not
trivially OCR-able.

## `POST /auth/login`

```json
{"username":"401110918","password":"...","challenge":"<uuid>","captcha":"<typed text>"}
```

Returns `{"token": "<jwt>"}`, or `{"error": "..."}`.

An empty submission returns `{"error":"INVALID_CAPTCHA"}`, so the captcha is
validated **before** the credentials. That ordering makes credential
brute-forcing through this endpoint impractical, and it also means a captcha
failure tells you nothing about whether the username or password were right.

## `POST /reg`

```json
{"action":"add"|"remove"|"move", "course":"40404-1", "units":1}
```

Note the field is `course`, not `courseId`. The frontend helper takes
`courseId` internally and renames it on the wire:

```js
L = function(e){ var t=e.action, n=e.courseId, r=e.units;
                 return A("/reg", {action:t, course:n, units:r}) }
```

The three actions map to `enrollCourse`, `disenrollCourse` and
`changeEnrolledCourseGroup` in the bundle.

**This endpoint is asynchronous.** The call enqueues a job. The authoritative
result arrives later in `userState.jobs[]` over the WebSocket, keyed by a UUID:

```json
{"id":"-d6d43492-...","action":"add","courseId":"30004-1","units":1,"result":"NO_REGISTRATION_TIME"}
```

A job whose `result` is `null` is still queued. The response body also echoes
the job list, which is why one request can reveal that a different course
already landed.

## `POST /user/favorite`

```json
{"course":"40404-1","marked":true}
```

Toggles the star in the UI. Cosmetic only, it reserves nothing. The starred set
comes back as `userState.favorites`, an array of course ids.

---

## Stack

| Layer | What |
| --- | --- |
| Edge | nginx, TLS, HTTP/2, serves the static build |
| App | Node / Express (`x-powered-by: Express`) |
| Frontend | create-react-app, Redux, Semantic UI React, `lang="fa" dir="rtl"` |
| Realtime | native WebSocket at `/api/ws` |
| Origin | 81.31.168.91 |

Static assets are hash-busted (`main.098a80f7.chunk.js`,
`2.b4891056.chunk.js`, `main.bd7b9ca0.chunk.css`). If those hashes change, the
bundle has been rebuilt and everything documented here is worth re-checking.
