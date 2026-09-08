# Error codes

The complete set, extracted from the portal frontend bundle.

Note that `COURSE_NOT_FOUND` does not exist. The real code is `INVALID_COURSE`.

Only some codes carry user facing text. The rest are either template strings
that interpolate a course name, or codes the UI never renders.

## Triage

How the sniper treats each code. This is the operational view; the tables
below are the full reference.

| Retryable | Permanent for that course | System or timing |
| --- | --- | --- |
| `CAPACITY_EXCEEDED` | `INVALID_COURSE` | `NO_REGISTRATION_TIME` |
| `REPEATED_REQUEST` | `INCORRECT_UNIT_NUMBER` | `REGISTRATION_TIME_LIMIT` |
| `ALREADY_IN_QUEUE` | `UNITS_LIMIT` | `LOGIN_TIME_RESTRICTION` |
| `PLEASE_WAIT` | `CLASS_OVERLAP` | `TOO_MANY_REQUESTS` |
| `CONNECTION_ERROR` | `EXAM_OVERLAP` | `AUTHORIZATION` |
| `DATABASE_QUERY_ERROR` | `COURSE_DUPLICATE` | `NO_REMAINED_ACTION` |
| | `COURSE_TAKEN_BEFORE` | `INVALID_ACTION` |
| | `MAAREF_COURSES_LIMIT` | `CONSTRAINTS_VIOLATED` |
| | `INCOMPATIBLE_CAMPUS` | `CANNOT_ADD_COURSE_IN_TARMIM` |
| | `INCOMPATIBLE_GENDER` | `CANNOT_REMOVE_COURSE_IN_TARMIM` |
| | `UNSUPPORTED_COURSE_TYPE` | `CANNOT_REMOVE_COURSE` |
| | `NO_PERMISSION` | `INVALID_MOVE`, `INVALID_REMOVE` |
| | `REGISTER_IN_EDU` | `HAS_INCOMPLETE_PROJECT` |
| | `PROJECT_FIRST_REGISTRATION` | |

Every course is retried regardless, because you are watching and can judge
better than a rule can, and because dropping the course a clash is against
turns a permanent failure into a retryable one. But a permanent failure is
parked: it waits 45 seconds between tries and sits behind every course that can
still land, so it cannot take turns from them. On 2026-09-08 a `CLASS_OVERLAP`
course took nine attempts and roughly twenty eight seconds of scheduler time
under the old rule. Permanent failures are called out in the log with a plain
explanation so they are obvious at a glance.

---

## Full reference

With the Persian text the portal shows and what each code actually means.

### Authentication and session

| Code | Persian | Meaning |
| --- | --- | --- |
| `AUTHORIZATION` | (none, client force-logs-out) | token missing, invalid or expired |
| `WRONG_USERNAME` | شماره‌ی دانش‌جویی اشتباه است. | bad student number |
| `WRONG_PASSWORD` | رمز عبور اشتباه است. | bad password |
| `INVALID_CAPTCHA` | کد امنیتی اشتباه است. | captcha wrong, checked before credentials |
| `INVALID_STUDENT` | شماره‌ی دانش‌جویی واردشده مجاز به ورود به سامانه نیست. | this student number may not use the system |
| `LOGIN_TIME_RESTRICTION` | اکنون زمان ورود شما به سامانه نیست. | not your login window |

### Timing and availability

| Code | Persian | Meaning |
| --- | --- | --- |
| `NO_REGISTRATION_TIME` | اکنون زمان انتخاب واحد نیست. | registration is not open at all |
| `NOT_LOGIN_TIME` | اکنون زمان انتخاب واحد شما نیست. | not your slot |
| `REGISTRATION_TIME_LIMIT` | اکنون زمان انتخاب واحد شما نیست. | not your slot |
| `EDU_TIME` | این سامانه تنها در ساعات ۸ الی ۱۲ فعال است. | system only live 08:00 to 12:00 |
| `CLOSED_INTERVAL` | به دستور معاونت آموزشی، سامانه در این ساعت از دسترس خارج است. | closed by order of the academic vice-chancellor |
| `REGISTER_IN_EDU` | برای ثبت‌نام به سامانه‌ی آموزش مراجعه کنید. | use the main آموزش system instead |
| `NO_PERMISSION` | مجوز ثبت‌نام برای شما صادر نشده است. | no registration permission issued |

`NO_REGISTRATION_TIME` takes precedence over course validation, which is why a
course code cannot be checked against `/api/reg` before the window.

### Rate limiting and queueing

| Code | Persian | Meaning |
| --- | --- | --- |
| `BLOCKED` | شماره‌ی دانش‌جویی شما به علت ارسال درخواست بیش از حد مجاز محدود شده است. لطفا دقایقی دیگر مراجعه نمایید. | student ID restricted for excess requests |
| `TOO_MANY_REQUESTS` | تعداد درخواست‌های همزمان بیش از حد مجاز بوده است. لطفا بعدا تلاش کنید. | too many concurrent requests |
| `PLEASE_WAIT` | مشکل در سامانه‌ی آموزش، لطفا منتظر بمانید. | upstream آموزش problem |
| `REPEATED_REQUEST` | (template) | duplicate request |
| `ALREADY_IN_QUEUE` | درخواست ... | a job for this course is already queued |
| `NO_REMAINED_ACTION` | (none) | `remainingActions` exhausted |

`BLOCKED` is keyed on the student ID rather than the IP. See
[rate-limits.md](rate-limits.md).

### Registration rejections

| Code | Persian | Meaning |
| --- | --- | --- |
| `CAPACITY_EXCEEDED` | (none) | course is full |
| `CLASS_OVERLAP` | (none) | class time clashes |
| `EXAM_OVERLAP` | (none) | exam time clashes |
| `COURSE_DUPLICATE` | (none) | already enrolled |
| `COURSE_TAKEN_BEFORE` | (none) | already passed |
| `COURSE_NOT_IN_CHART` | (none) | not in your study chart |
| `UNITS_LIMIT` | (none) | would exceed your unit ceiling |
| `INCORRECT_UNIT_NUMBER` | (none) | unit count not valid for this course |
| `VARIABLE_UNITS_EXCEEDED` | (none) | units above the variable-unit range |
| `ZERO_UNITS_NOT_POSSIBLE` | (none) | zero units not allowed here |
| `INCOMPATIBLE_CAMPUS` | (none) | wrong campus |
| `INCOMPATIBLE_GENDER` | (none) | not open to your gender |
| `MAAREF_COURSES_LIMIT` | (none) | maaref course limit reached |
| `CONSTRAINTS_VIOLATED` | (none) | a study-plan constraint failed |
| `UNSUPPORTED_COURSE_TYPE` | امکان ثبت ... | this course type cannot be registered here |
| `HAS_INCOMPLETE_PROJECT` | شما درس پروژه، پایان‌نامه یا رساله‌ی ناتمام دارید. باید ابتدا در آن درس ثبت‌نام کنید. | unfinished project or thesis, register that first |
| `PROJECT_FIRST_REGISTRATION` | (none) | project must be registered first |
| `INVALID_COURSE` | درس موردنظر وجود ندارد. | no such course. **There is no `COURSE_NOT_FOUND`** |
| `INVALID_ACTION` | درخواست نامعتبر است. | action not one of add / remove / move |
| `INVALID_MOVE` | خطا در تغییر به ... | group change failed |
| `INVALID_REMOVE` | خطا در حذف ... | removal failed |
| `CANNOT_REMOVE_COURSE` | (none) | this course cannot be dropped |
| `CANNOT_ADD_COURSE_IN_TARMIM` | (none) | cannot add during ترمیم (add/drop) |
| `CANNOT_REMOVE_COURSE_IN_TARMIM` | (none) | cannot drop during ترمیم |

### System

| Code | Persian | Meaning |
| --- | --- | --- |
| `DATABASE_QUERY_ERROR` | (none) | backend database error |
| `CONNECTION_ERROR` | (none) | backend connectivity error |
| `UNKNOWN` | خطای نامشخص رخ داده است. | unspecified error |
| `LATE_JOBRANI` | (none) | undocumented. Named after the comedian, so presumably an easter egg for arriving late |

---

## Gaps against `permanentFailures`

`permanentFailures` in `cmd/sniper/main.go` held 12 codes. These looked like
they belonged there too:

| Code | Why it looks permanent |
| --- | --- |
| `COURSE_NOT_IN_CHART` | the course is not in your study chart, retrying cannot change that |
| `VARIABLE_UNITS_EXCEEDED` | same family as `INCORRECT_UNIT_NUMBER`, which is already listed |
| `ZERO_UNITS_NOT_POSSIBLE` | same family |
| `CONSTRAINTS_VIOLATED` | a study-plan rule failed |
| `HAS_INCOMPLETE_PROJECT` | clears only once you register the project course, so not permanent within a session, but retrying this course will not fix it |
| `PROJECT_FIRST_REGISTRATION` | same |

All six have since been added to `permanentFailures`, along with
`REGISTER_IN_EDU`. `PROJECT_FIRST_REGISTRATION` was seen live on 2026-09-08 and
was retried eight times before the run was stopped by hand.

`REPEATED_REQUEST` and `ALREADY_IN_QUEUE` are handled separately again. They
are not failures: the portal is holding a job with that exact course, unit
count and action, and will judge it on its own. Both transcripts from that
window show a course landing from a job that had answered `REPEATED_REQUEST`
moments earlier. `REPEATED_REQUEST` also arrives as a bare JSON string rather
than an object, so it carries no `jobs` array to harvest:

```
"REPEATED_REQUEST 40012345630004-11add"
```

The key is the student id, the course, the units and the action run together.
