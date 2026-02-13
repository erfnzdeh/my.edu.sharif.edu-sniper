# Tokens and authentication

## Token shape

HS256 JWT, with **no `exp` claim**:

```json
{"studentId":"401110999","createdAt":1788780209869,"superUser":false,"iat":1788780209}
```

`createdAt` is milliseconds, `iat` is seconds. Expiry is enforced server side
against `createdAt`, so it cannot be read off the token, only observed.

There is a `superUser` boolean, which is `false` for a student account.

## Lifetime, measured

The token used for this investigation was **accepted over HTTP at about 58
minutes of age and rejected at about 61 minutes**, returning
`{"error":"AUTHORIZATION"}`. So the lifetime is roughly an hour, bounded by
observation rather than read from the server. The true value is somewhere in
that 58 to 61 minute range.

A rejected token is not a `401`. It is HTTP 200 with an `error` field, and the
frontend reacts by dispatching `LOGOUT`.

## The WebSocket ignores token age

This is the one surprising thing in the auth model, and it is useful.

Once HTTP had begun rejecting the token, a **brand new WebSocket handshake with
that same expired token was still accepted**, and streamed a full `userState`
and the entire course list. Repeated across several connections.

It is not that the WebSocket skips auth. A missing or malformed token is
rejected: the socket opens and then closes immediately with code 1006. So the
signature is verified and the age is not.

An already established socket also survives its token expiring, since auth is
only checked at the handshake.

**Consequence:** anything that only observes (catalogue dumps, capacity
watching, reading job results) can run indefinitely on one token. Only
`POST /api/reg` needs a token under about an hour old.

## Getting a token

From the browser console on my.edu.sharif.edu:

```js
copy(JSON.parse(localStorage.getItem('persist:root') || '{}').token)
```

Or copy the `Authorization` request header from any API call in the network
tab, which is what the README recommends.

## Why full automation stops here

Refreshing a token means `POST /auth/login`, which requires solving the SVG
captcha, and the captcha is checked before the credentials. There is no refresh
token and no other way to mint one.

So any run longer than an hour needs a human to paste a fresh token. For the
sniper this is a non-issue, because a registration window is over in seconds
and you capture the token shortly before it. It only bites long-lived watchers,
and those can stay on the WebSocket, which does not care.
