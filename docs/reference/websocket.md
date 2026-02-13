# The WebSocket

```
wss://my.edu.sharif.edu/api/ws?token=<urlencoded jwt>
```

The only source of course data. There is no REST equivalent, which is why
`tools/catalogue/dump.mjs` exists.

The client never sends a frame. Everything is server push, shaped as
`{"type": ..., "message": ...}`.

| `type` | Payload | Notes |
| --- | --- | --- |
| `userState` | object | on connect, and after every state change |
| `courseList` | array | full list. Declared in the client; in practice the server sends `listUpdate` instead |
| `listUpdate` | array | **delta**, only courses whose fields changed |
| `logout` | none | server forces the client to log out |

The client's handler for an unknown type calls `undefined`, so a new message
type would throw rather than be ignored.

## Cadence

Captured over 7 minutes:

```
[+0.9s]   OPEN
[+1.0s]   userState    5102 B
[+1.9s]   listUpdate   1752 entries, 651832 B   <- the full list, on connect
[+1.9s]   listUpdate      0 entries,     34 B
[+62.2s]  listUpdate      1 entry,      391 B
[+120.4s] listUpdate      2 entries,    755 B
[+182.6s] listUpdate      0 entries,     34 B
[+242.7s] listUpdate      1 entry,      364 B
[+300.1s] listUpdate      2 entries,    724 B
[+361.8s] listUpdate      1 entry,      394 B
```

A push arrives about every 60 seconds without fail. An empty `listUpdate` (34
bytes) is the heartbeat. The full list comes once, on connect, as a
`listUpdate` rather than the `courseList` the client also handles.

**This 60 second tick is the hard floor on seat detection.** No client can
learn that a seat freed sooner than the next push, which is why a sniper aimed
at a contested course should already be attempting rather than waiting to be
told.

## `userState`

```json
{
  "remainingActions": 12,
  "courses": [],
  "units": 0,
  "data": { "id": "401110999", "degree": 1, "campus": 1,
            "department": 40, "hasPermission": 1, "unitsLimit": 20 },
  "jobs": [ { "id": "...", "action": "add", "courseId": "30004-1",
              "units": 1, "result": "NO_REGISTRATION_TIME" } ],
  "favorites": ["40215-1", "40634-1"],
  "registrationTime": 1786163400000,
  "time": 1788783716094
}
```

- `time` is **server epoch milliseconds**, and is the basis for clock sync. The
  machine used here was **46 seconds fast**, which on its own is enough to miss
  a window opening.
- `registrationTime` is your personal slot. It goes stale, as the README warns:
  on this account it pointed at 2026-08-08 08:00, a month in the past.
- `jobs` is newest first and accumulates for the whole session. On this account
  it grew from 22 to 41 entries during a single afternoon.
- `remainingActions` counts **only `remove` and `move`**. See below.
- `courses` is your enrolled list, empty here.

### `remainingActions` counts removals, not additions

The UI labels it "عملیات حذف یا تغییر گروه باقی مانده", which is *remaining
remove or group-change operations*.

Confirmed empirically: the account accumulated **41 `add` jobs while
`remainingActions` stayed at 12**. Retrying `add` is quota free and bounded
only by the rate limiter. `remove` and `move` are capped at 12 for the term,
and `NO_REMAINED_ACTION` is the error when they run out.

This matters for the README's TODO about supporting `remove` and `move`: those
two need a budget check that `add` does not.

## Course object

All 19 fields were present on all 1752 courses.

```json
{ "id": "53615-1", "number": "53615", "group": 1,
  "title": "مکانیک سیالات", "level": 0, "instructors": "شاهین فقیری",
  "location": "", "schedule": [ {"day": 5, "start": 7.5, "end": 9} ],
  "examDate": " ", "description": "...", "warning": "",
  "units": 3, "isVariable": 0, "type": 0,
  "capacity": 35, "department": 53, "campus": 2,
  "count": 0, "reserve": false }
```

- `id` is always `number + "-" + group`, true for all 1752. Twenty ids are not
  numeric (`22TA0-1`, `40TA0-1` and similar), which are teaching assistant
  slots, so do not assume `\d{5}-\d+` when parsing.
- `schedule[].day` is 0 to 5, 0 being Saturday. Thursday and Friday are sparse
  (155 and 57 slots against 529 to 607 for Saturday through Wednesday).
- `start` and `end` are decimal hours, so `7.5` is 07:30. Range 7.0 to 19.5.
- `capacity` of `-1` renders as "بدون ظرفیت", `0` renders as "نامحدود".
- `reserve` means the course has a waitlist. 1094 of 1752 do. The UI colours
  the capacity bar `count < capacity ? blue : (reserve ? yellow : red)`.
- Enrolled courses carry an extra `reserveOrder`: `0` means "اخذ شده", you have
  the seat, anything else is your waitlist position, displayed in groups of
  five.
- `campus` is 1 (1530 courses) or 2 (222). `department` has 28 distinct values,
  40 being Computer Engineering. `level` is 0 to 3. `type` has 18 distinct
  values.

### `count` is only meaningful during a window

Outside a registration window `count` reads 0 for essentially every course:
1751 of 1752 in this capture. Capacity telemetry is therefore useless for
testing between windows, and any logic that waits for `count < capacity` cannot
be validated until registration opens.

### The list is not fixed size

It was 1752 courses at 12:14 and 1754 about half an hour later. Departments add
groups during the term, so a catalogue dump is a snapshot, and a course id
absent from `courses.json` is not necessarily invalid.
