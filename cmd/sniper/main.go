// Command sniper registers courses on my.edu.sharif.edu the moment the
// registration window opens.
//
// The portal's edge rejects a request that follows the previous one too
// closely, and everything measured so far fits about one per second with no
// burst, so the scheduler spends one request token at a time. Every course gets its
// first attempt before any course gets its second, and within the same
// attempt count list order is priority order. Answers take seconds to come
// back, so a few requests may be waiting at once: the token spacing is what
// the edge cares about, not how many answers are outstanding.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The live API. These are variables rather than constants only so the tests
// can point the scheduler at a local stand in.
var (
	regEndpoint   = "https://my.edu.sharif.edu/api/reg"
	portalOrigin  = "https://my.edu.sharif.edu"
	portalReferer = "https://my.edu.sharif.edu/courses/offered"
)

// Measured against the live API. See README for how these were established.
const (
	// The portal enforces five seconds between two requests for the same
	// course, measured from when the request was sent.
	courseCooldown = 5200 * time.Millisecond
	// Backoff after the edge returns a 429. The edge allows one request per
	// second, so a rejection only says the last one was too close. A long
	// freeze costs far more than the rejection did: on 2026-09-08 two 429s
	// cost fourteen seconds of the most contested part of the window.
	rateLimitBackoff = 2 * time.Second
	// How long to hold every request after the portal reports BLOCKED. Its
	// own text says minutes, and the block follows the student id rather than
	// the connection, so there is nothing to gain by probing sooner.
	blockedBackoff = 30 * time.Second
	// How long to hold back a course whose result cannot change on a retry.
	// It is still retried, because the operator may drop the course it
	// clashes with, but it must not crowd out a course that can still land.
	parkedBackoff = 45 * time.Second
	// How far ahead of the window to open a connection, so the first real
	// request does not pay a TLS handshake. It must not be closer than the
	// global gap, because the warm up itself spends a request token.
	warmupLead = 2 * time.Second
	// Skip the warm up when a request was made recently enough that the
	// pooled connection is certainly still open.
	warmupSkipIfNewerThan = 60 * time.Second

	catalogueURL = "https://erfnzdeh.github.io/my.edu.sharif.edu-sniper/api/courses.json"
	httpTimeout  = 10 * time.Second
)

// Pacing, overridable from the command line because the right values depend
// on the link and both cost something when they are wrong.
var (
	// globalGap is the minimum spacing between two requests. The edge looks
	// like about one per second, and counts every request to the host,
	// including the warm up. A 1.1s gap lost about one request in seven to
	// round trip jitter, which is a bad trade now that a rejection is cheap
	// but not free. A working default from small samples, hence the flag.
	// See docs/reference/rate-limits.md.
	globalGap = 1300 * time.Millisecond
	// maxInflight caps how many requests may be waiting for an answer at
	// once. Zero, the default, means no cap, because the scheduler is
	// already bounded twice over: a course with a request in flight is never
	// picked again, so there is at most one per course, and no request can
	// outlive httpTimeout. The real ceiling is the smaller of the list
	// length and httpTimeout/globalGap, which is about eight at the
	// defaults.
	//
	// A cap below that ceiling throttles sending rather than answering. At
	// the five second answers measured inside the window, a cap of three
	// lets a request leave only every 1.7s, which is slower than the gap the
	// edge actually allows, so the cap and not the limiter sets the pace.
	// It stays as an escape hatch for TOO_MANY_REQUESTS, the portal's own
	// concurrency guard, which has never been seen live.
	maxInflight = 0
)

// Result codes, taken from the portal frontend bundle.
const (
	resultOK        = "OK"
	resultDuplicate = "COURSE_DUPLICATE"
	resultAuthError = "AUTHORIZATION"
)

// permanentFailures lists the results that can never succeed on a retry.
// Courses are still retried, because the operator watches the log and may drop
// whatever a course clashes with, but they go to the back of the queue and
// wait parkedBackoff between tries so they cannot crowd out a course that can
// still land. See docs/reference/error-codes.md.
var permanentFailures = map[string]string{
	"INVALID_COURSE":             "no such course, check the code and group",
	"INCORRECT_UNIT_NUMBER":      "wrong unit count for this course",
	"VARIABLE_UNITS_EXCEEDED":    "units above this course's variable range",
	"ZERO_UNITS_NOT_POSSIBLE":    "this course cannot be taken for zero units",
	"UNITS_LIMIT":                "this would exceed your total unit limit",
	"CLASS_OVERLAP":              "class time clashes with another course",
	"EXAM_OVERLAP":               "exam time clashes with another course",
	"COURSE_TAKEN_BEFORE":        "already passed this course",
	"COURSE_NOT_IN_CHART":        "not in your study chart",
	"CONSTRAINTS_VIOLATED":       "a study plan constraint rejected it",
	"MAAREF_COURSES_LIMIT":       "hit the limit on maaref courses",
	"INCOMPATIBLE_CAMPUS":        "wrong campus",
	"INCOMPATIBLE_GENDER":        "not open to your gender",
	"UNSUPPORTED_COURSE_TYPE":    "this course type cannot be added here",
	"NO_REMAINED_ACTION":         "no registration actions left",
	"NO_PERMISSION":              "no permission to register for this course",
	"REGISTER_IN_EDU":            "register for this one in the main edu system",
	"HAS_INCOMPLETE_PROJECT":     "register your unfinished project or thesis first",
	"PROJECT_FIRST_REGISTRATION": "the project course has to be registered first",
}

// queuedResults are not failures. The portal already holds a job for this
// exact course, units and action, and will judge it on its own. Both
// transcripts from 2026-09-08 show a course landing from a job that had
// answered REPEATED_REQUEST moments earlier, so the worst thing to do is
// spend another token on it right away.
var queuedResults = map[string]string{
	"REPEATED_REQUEST": "the portal already has this exact job queued",
	"ALREADY_IN_QUEUE": "a job for this course is already queued",
}

// timingResults mean the backend refused to look at the course at all, so
// they are not verdicts on it and do not count against it in the ordering.
var timingResults = map[string]string{
	"NO_REGISTRATION_TIME":    "registration is not open",
	"REGISTRATION_TIME_LIMIT": "not your registration slot",
	"NOT_LOGIN_TIME":          "not your registration slot",
	"LOGIN_TIME_RESTRICTION":  "not your login window",
	"EDU_TIME":                "the portal is only live 08:00 to 12:00",
	"CLOSED_INTERVAL":         "the portal is closed at this hour",
}

// holdResults are the portal pushing back on the client as a whole rather
// than judging a course, so they hold the shared token the way a 429 does.
// None has been seen live. BLOCKED says "try again in a few minutes", and it
// is keyed on the student id, so hammering through it can only prolong it.
var holdResults = map[string]struct {
	why  string
	hold time.Duration
}{
	"TOO_MANY_REQUESTS": {"too many requests outstanding, lower -inflight if this repeats", rateLimitBackoff},
	"PLEASE_WAIT":       {"the portal's upstream is struggling", rateLimitBackoff},
	"BLOCKED":           {"your student id is restricted for excess requests", blockedBackoff},
}

type catalogueEntry struct {
	Units    int32  `json:"u"`
	Variable int    `json:"v"` // 1 when the student picks the unit count
	Title    string `json:"t"`
	Capacity int    `json:"c"` // snapshot at dump time, goes stale quickly
}

// variableUnits reports whether the unit count can be chosen. The wire format
// carries 0 and 1 rather than a JSON boolean, and released binaries already
// parse it as a number, so the field stays an int and this hides that.
func (e catalogueEntry) variableUnits() bool { return e.Variable != 0 }

type regRequest struct {
	Action string `json:"action"`
	Course string `json:"course"`
	Units  int32  `json:"units"`
}

type job struct {
	CourseID string `json:"courseId"`
	Result   string `json:"result"`
}

type regResponse struct {
	Error            string `json:"error"`
	RemainingActions int    `json:"remainingActions"`
	Jobs             []job  `json:"jobs"`
	RegistrationTime int64  `json:"registrationTime"`
	Time             int64  `json:"time"`
}

type course struct {
	id    string
	units int32
	title string
	pri   int // position in the list the operator gave, 0 is highest

	attempts int       // requests sent for this course
	judged   int       // requests the backend actually answered
	next     time.Time // not eligible to be retried before this
	pending  bool      // a request is in flight right now
	parked   bool      // last result cannot change on a retry
	early    int       // timing rejections so far, only the first is free
	done     bool
	landed   time.Time
	last     string // most recent result seen
}

// version is the release tag. It is injected at build time with
//
//	go build -ldflags "-X main.version=v1.2.3"
//
// and is empty in a build that was not stamped, where buildVersion falls back
// to the revision the toolchain records in the binary.
var version = ""

// buildVersion is what the banner, the transcript and -version report. It is
// the first thing to ask for in a bug report, so an unstamped local build
// still has to answer it usefully.
func buildVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return "dev-" + rev
}

func main() {
	os.Exit(run())
}

func run() int {
	var (
		fToken      = flag.String("token", "", "Authorization header value from a logged in browser session")
		fCourses    = flag.String("courses", "", "comma separated course list in priority order, for example 22034-2,40441-1:1")
		fAt         = flag.String("at", "", "window opening time as HH:MM, used when the server's registrationTime is stale")
		fYes        = flag.Bool("y", false, "skip the confirmation prompt, requires -token and -courses")
		fCatalogue  = flag.String("catalogue", "", "path or URL of courses.json, overrides the default lookup")
		fTranscript = flag.String("transcript", "", "transcript file path, defaults to snipe-<timestamp>.log")
		fGap        = flag.Duration("gap", globalGap, "minimum spacing between two requests, the edge allows about one per second")
		fInflight   = flag.Int("inflight", maxInflight, "cap on requests waiting for an answer at once, 0 for no cap")
		fVersion    = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	globalGap = *fGap
	if globalGap < 100*time.Millisecond {
		globalGap = 100 * time.Millisecond
	}
	maxInflight = *fInflight
	if maxInflight < 0 {
		maxInflight = 0
	}

	if *fVersion {
		fmt.Printf("sniper %s %s/%s %s\n",
			buildVersion(), runtime.GOOS, runtime.GOARCH, runtime.Version())
		return 0
	}

	ui := newUI()
	ui.banner()

	cat, catSrc, err := loadCatalogue(*fCatalogue)
	if err != nil {
		ui.fatal("catalogue: %v", err)
		return 2
	}
	ui.info("catalogue  %d courses from %s", len(cat), catSrc)

	token, err := resolveToken(ui, *fToken, *fYes)
	if err != nil {
		ui.fatal("%v", err)
		return 2
	}

	courses, err := resolveCourses(ui, cat, *fCourses, *fYes)
	if err != nil {
		ui.fatal("%v", err)
		return 2
	}

	tr, err := newTranscript(*fTranscript)
	if err != nil {
		ui.fatal("transcript: %v", err)
		return 2
	}
	defer tr.Close()
	ui.tr = tr
	ui.info("transcript %s", tr.path)
	tr.write("BUILD    %s %s/%s %s", buildVersion(), runtime.GOOS, runtime.GOARCH, runtime.Version())
	tr.write("PACING   gap=%s cooldown=%s inflight=%s rateLimitBackoff=%s parkedBackoff=%s timeout=%s",
		globalGap, courseCooldown, inflightNote(len(courses)), rateLimitBackoff, parkedBackoff, httpTimeout)

	// The default transport keeps two idle connections per host, so with
	// answers overlapping, the third concurrent request onwards would find an
	// empty pool and pay a fresh TLS handshake, measured at about 650ms, every
	// time it went out. Keep one warm connection per course instead. The
	// portal negotiates HTTP/2, checked on 2026-09-08, so one connection
	// carries them all and this only matters on a fallback to HTTP/1.1: the
	// transcript's proto= field says which happened.
	pool := http.DefaultTransport.(*http.Transport).Clone()
	pool.MaxIdleConns = 4 * len(courses)
	pool.MaxIdleConnsPerHost = 2 * len(courses)
	cl := &client{
		http:  &http.Client{Timeout: httpTimeout, Transport: pool},
		token: token,
		tr:    tr,
	}

	// The clock probe doubles as the token check. Spend it on the lowest
	// priority course, whose cooldown expires long before the window.
	probe := courses[len(courses)-1]
	clk, err := cl.syncClock(probe)
	if err != nil {
		ui.fatal("%v", err)
		return 2
	}
	ui.showClockSync(clk)

	target, err := resolveWindow(ui, clk, *fAt, *fYes)
	if err != nil {
		ui.fatal("%v", err)
		return 2
	}

	fireAt := clk.fireTime(target)
	ui.showFireTime(clk, target, fireAt)
	if !confirm(ui, courses, clk, target, fireAt, *fYes) {
		ui.info("cancelled")
		return 2
	}

	ctx, stop := signalContext(ui)
	defer stop()

	// Stop short of the window so the warm up lands inside its own lead. It
	// used to run after the wait, which made the lead negative: the warm up
	// GET then went out at the fire time, the first POST followed it within
	// milliseconds, and the edge rejected it. Both transcripts from
	// 2026-09-08 lost their opening shot exactly that way.
	ui.waitUntil(ctx, fireAt.Add(-warmupLead), fireAt)
	if ctx.Err() != nil {
		return report(ui, courses)
	}

	cl.warmUp(ui, fireAt)
	if !sleepCtx(ctx, time.Until(fireAt)) {
		return report(ui, courses)
	}
	tr.write("FIRE     planned %s, actually armed %s, %s late",
		fireAt.Format("15:04:05.000"), time.Now().Format("15:04:05.000"),
		time.Since(fireAt).Truncate(time.Millisecond))
	authFailed := fireWindow(ctx, ui, cl, courses)
	code := report(ui, courses)
	if authFailed {
		return 2
	}
	return code
}

// ---------------------------------------------------------------- scheduling

// fireWindow spends request tokens until every course has landed or the run
// is stopped. It returns true when it stopped because the token was rejected.
//
// Two rules decide who gets the next token. A course the backend has never
// judged outranks one it has, so the first pass covers the whole list before
// anything is retried: under strict priority order on 2026-09-08 the first
// course absorbed four attempts and thirty seconds while the last two courses
// never got a single request. Within the same count, list order wins.
func fireWindow(parent context.Context, ui *ui, cl *client, courses []*course) bool {
	ui.rule("window open, firing")

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		authBad  bool
		inflight int
		// nextSend is the earliest the next request may leave. The warm up
		// spends a token like any other request, so start from the last call
		// the client made rather than from now.
		nextSend = cl.called().Add(globalGap)
	)
	slots := make(chan struct{}, inflightCap(len(courses)))
	wake := make(chan struct{}, 1)
	notify := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}

	for ctx.Err() == nil {
		mu.Lock()
		// The client records when each request was actually written, which
		// is later than the scheduler's send time on a cold connection, and
		// the warm up writes a request the scheduler never sent at all.
		if ns := cl.called().Add(globalGap); ns.After(nextSend) {
			nextSend = ns
		}
		finished, wait := allDone(courses), time.Until(nextSend)
		mu.Unlock()
		if finished {
			break
		}
		// Loop rather than fall through after sleeping: an answer arriving
		// meanwhile can land the last course or push the token further out.
		if wait > 0 {
			if !sleepCtx(ctx, wait) {
				break
			}
			continue
		}

		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			continue
		}

		mu.Lock()
		pick, earliest := choose(courses, time.Now())
		if pick == nil {
			ui.tr.write("SCHED    idle, %d in flight, nothing eligible%s", inflight, freeNote(earliest))
			mu.Unlock()
			<-slots
			waitForWake(ctx, wake, earliest)
			continue
		}
		sent := time.Now()
		pick.attempts++
		pick.pending = true
		pick.next = sent.Add(courseCooldown)
		attempt := pick.attempts
		nextSend = sent.Add(globalGap)
		inflight++
		ui.tr.write("SCHED    send %s attempt %d, %d in flight, next token at %s",
			pick.id, attempt, inflight, nextSend.Format("15:04:05.000"))
		ui.tr.write("SCHED    board %s", board(courses))
		mu.Unlock()

		wg.Add(1)
		go func(c *course, attempt int, sent time.Time) {
			defer wg.Done()
			defer func() { <-slots }()

			resp, rtt, err := cl.add(c)
			var str *stringResult

			mu.Lock()
			c.pending = false
			inflight--
			switch {
			case errors.Is(err, errRateLimited):
				// The edge answered on its own and the backend never saw the
				// request, so this is not a judgement on the course and it
				// keeps its place in the queue. Only the shared token moves,
				// because the limiter is keyed on the IP, not on the course.
				c.last = "429"
				c.next = sent
				if back := time.Now().Add(rateLimitBackoff); back.After(nextSend) {
					nextSend = back
				}
				ui.attempt(c, attempt, "429 RATE LIMITED", fmt.Sprintf("edge rejected it, next token in %s", rateLimitBackoff), rtt, kindWarn)
			case errors.Is(err, errAuth):
				authBad = true
				ui.fatal("token rejected or expired, log in again and rerun")
				cancel()
			case errors.As(err, &str) && holdResults[str.code].hold > 0:
				// The portal is pushing back on the client as a whole, not on
				// this course, so hold the shared token and let the course
				// keep its place, exactly as for a 429.
				h := holdResults[str.code]
				c.last = str.code
				c.next = sent
				if back := time.Now().Add(h.hold); back.After(nextSend) {
					nextSend = back
				}
				ui.attempt(c, attempt, str.code, fmt.Sprintf("%s, next token in %s", h.why, h.hold), rtt, kindWarn)
			case errors.As(err, &str):
				applyCode(ui, c, attempt, str.code, rtt)
			case err != nil:
				// A request that gave up client side may still have been
				// carried out: on 2026-09-08 two that timed out registered the
				// course anyway. Count it as judged so the scheduler moves on
				// and lets the next answer report what really happened.
				c.judged++
				c.last = "error"
				ui.attempt(c, attempt, "REQUEST FAILED", err.Error()+" (it may still have been carried out)", rtt, kindWarn)
			default:
				applyJobs(ui, courses, c, attempt, resp, rtt)
				ui.setRemaining(resp.RemainingActions)
			}
			mu.Unlock()
			notify()
		}(pick, attempt, sent)
	}

	wg.Wait()
	return authBad
}

// inflightCap is how many answers may be outstanding. A course with a request
// in flight is never picked again, so the length of the list is a hard ceiling
// and an uncapped run simply uses it.
func inflightCap(courses int) int {
	if maxInflight <= 0 || maxInflight > courses {
		return courses
	}
	return maxInflight
}

func inflightNote(courses int) string {
	if maxInflight <= 0 {
		return fmt.Sprintf("uncapped, ceiling %d", inflightCap(courses))
	}
	return strconv.Itoa(maxInflight)
}

// choose returns the course that should get the next token, and the soonest
// time some course comes off cooldown when none is eligible right now.
func choose(courses []*course, now time.Time) (*course, time.Time) {
	var pick *course
	var earliest time.Time
	for _, c := range courses {
		if c.done || c.pending {
			continue
		}
		if c.next.After(now) {
			if earliest.IsZero() || c.next.Before(earliest) {
				earliest = c.next
			}
			continue
		}
		if pick == nil || betterPick(c, pick) {
			pick = c
		}
	}
	return pick, earliest
}

// betterPick ranks two eligible courses. A parked course goes last whatever
// its position, then the one the backend has judged fewer times, then list
// order, which is the priority the operator gave.
func betterPick(a, b *course) bool {
	if a.parked != b.parked {
		return b.parked
	}
	if a.judged != b.judged {
		return a.judged < b.judged
	}
	return a.pri < b.pri
}

// waitForWake blocks until an answer lands, until a course comes off cooldown,
// or until the run is stopped. earliest may be zero, which means every course
// still outstanding has a request in flight and only an answer can help.
func waitForWake(ctx context.Context, wake <-chan struct{}, earliest time.Time) {
	if earliest.IsZero() {
		select {
		case <-wake:
		case <-ctx.Done():
		}
		return
	}
	t := time.NewTimer(time.Until(earliest))
	defer t.Stop()
	select {
	case <-t.C:
	case <-wake:
	case <-ctx.Done():
	}
}

func freeNote(earliest time.Time) string {
	if earliest.IsZero() {
		return ", waiting on an answer"
	}
	return fmt.Sprintf(", next free at %s", earliest.Format("15:04:05.000"))
}

// board renders every course on one transcript line, so a scheduling decision
// can be read back against the state it was made from.
func board(courses []*course) string {
	var b strings.Builder
	now := time.Now()
	for i, c := range courses {
		if i > 0 {
			b.WriteString(" | ")
		}
		fmt.Fprintf(&b, "%s %s att=%d judged=%d last=%s", c.id, courseState(c, now), c.attempts, c.judged, orDash(c.last))
		if !c.done && !c.pending && c.next.After(now) {
			fmt.Fprintf(&b, " free=+%s", c.next.Sub(now).Truncate(time.Millisecond))
		}
	}
	return b.String()
}

func courseState(c *course, now time.Time) string {
	switch {
	case c.done:
		return "done"
	case c.pending:
		return "inflight"
	case c.parked:
		return "parked"
	case c.next.After(now):
		return "cooling"
	default:
		return "ready"
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// applyJobs reads the newest result for the course just attempted, then
// harvests successes for any other course. jobs is newest first and
// accumulates across the whole session, so the first match is the latest.
func applyJobs(ui *ui, courses []*course, pick *course, attempt int, resp *regResponse, rtt time.Duration) {
	// The newest job per course, judged or not. Skipping an unjudged job
	// would fall through to an older one: on 2026-09-08 that read a pre
	// window NO_REGISTRATION_TIME as the verdict on a course whose real job
	// was still queued. Had the stale result been a permanent failure the
	// course would have been parked for nothing.
	seen := map[string]string{}
	for _, j := range resp.Jobs {
		if _, ok := seen[j.CourseID]; !ok {
			seen[j.CourseID] = j.Result
		}
	}

	if res, ok := seen[pick.id]; ok && res != "" {
		applyCode(ui, pick, attempt, res, rtt)
	} else if !pick.done {
		pick.judged++
		pick.last = "QUEUED"
		ui.attempt(pick, attempt, "QUEUED", "server has not judged it yet", rtt, kindWarn)
	}

	// A response describes every job, so another course may have landed
	// without ever spending a request of its own. Adopt successes only,
	// since a stale failure must never stall a course still worth trying.
	for _, c := range courses {
		if c.done || c == pick {
			continue
		}
		if r, ok := seen[c.id]; ok && (r == resultOK || r == resultDuplicate) {
			land(ui, c, c.attempts, r, 0)
		}
	}
}

// applyCode records one result code against the course it was returned for.
func applyCode(ui *ui, c *course, attempt int, res string, rtt time.Duration) {
	if c.done {
		// The course landed from another response while this request was in
		// flight. There is nothing left to record, but the transcript should
		// still show that the answer arrived.
		ui.tr.write("SCHED    late answer for %s attempt %d: %s, it had already landed", c.id, attempt, res)
		return
	}
	if res == resultOK || res == resultDuplicate {
		land(ui, c, attempt, res, rtt)
		return
	}
	c.last = res
	note := fmt.Sprintf("retry at %s", c.next.Format("15:04:05.000"))
	k := kindBad
	if why, timing := timingResults[res]; timing {
		c.early++
		if c.early == 1 {
			// The backend refused to look at the course, so this is not a
			// verdict on it. It keeps its rank rather than falling behind
			// every course tried after it, which matters most for the first
			// course on the list when the opening request lands a moment
			// early. Only the first one is free: a course the portal keeps
			// refusing on timing grounds while the others get real verdicts
			// would otherwise sit at judged 0 and outrank all of them for
			// the rest of the run.
			ui.attempt(c, attempt, res, why+", keeps its place", rtt, kindWarn)
			return
		}
		note = why + ", " + note
	}
	c.judged++
	if why, queued := queuedResults[res]; queued {
		note = why + ", waiting for its verdict instead of resending"
		k = kindWarn
	}
	if why, bad := permanentFailures[res]; bad {
		c.parked = true
		c.next = time.Now().Add(parkedBackoff)
		note = why + ", parked until " + c.next.Format("15:04:05")
	}
	ui.attempt(c, attempt, res, note, rtt, k)
}

func land(ui *ui, c *course, attempt int, res string, rtt time.Duration) {
	c.done = true
	c.landed = time.Now()
	c.last = res
	note := "registered"
	if res == resultDuplicate {
		note = "already registered"
	}
	if rtt > 0 {
		note += fmt.Sprintf(" in %dms", rtt.Milliseconds())
	} else {
		note += " (seen in another response)"
	}
	ui.attempt(c, attempt, res, note, rtt, kindGood)
	ui.bell()
}

func allDone(courses []*course) bool {
	for _, c := range courses {
		if !c.done {
			return false
		}
	}
	return true
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func signalContext(ui *ui) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		ui.warn("stopping, letting the requests in flight finish. press Ctrl-C again to force.")
		cancel()
		<-ch
		ui.warn("forced")
		os.Exit(1)
	}()
	return ctx, func() { signal.Stop(ch); cancel() }
}

// -------------------------------------------------------------------- client

var (
	errRateLimited = errors.New("rate limited")
	errAuth        = errors.New("authorization")
)

type client struct {
	http  *http.Client
	token string
	tr    *transcript

	mu       sync.Mutex
	seq      int
	lastCall time.Time
}

// stringResult is a result code that arrived without a jobs array: either a
// bare quoted string body, like
//
//	"REPEATED_REQUEST 40111099930004-11add"
//
// which is what the portal answers when it declines to queue the job at all,
// or the error field of an object. There is nothing to harvest from either,
// and the only way to learn a queued job's verdict is a later response.
type stringResult struct {
	code   string
	detail string
}

func (e *stringResult) Error() string {
	if e.detail == "" {
		return e.code
	}
	return e.code + " " + e.detail
}

func (c *client) nextSeq() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	return c.seq
}

func (c *client) called() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastCall
}

func (c *client) add(co *course) (*regResponse, time.Duration, error) {
	return c.post(regRequest{Action: "add", Course: co.id, Units: co.units})
}

func (c *client) post(body regRequest) (*regResponse, time.Duration, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(http.MethodPost, regEndpoint, bytes.NewReader(buf))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", portalOrigin)
	req.Header.Set("Referer", portalReferer)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36")

	seq := c.nextSeq()
	c.tr.write("REQUEST  #%d POST %s %s", seq, regEndpoint, buf)

	res, raw, rtt, err := c.do(req, seq)
	if err != nil {
		return nil, rtt, err
	}
	if res.StatusCode == http.StatusTooManyRequests {
		return nil, rtt, errRateLimited
	}
	if len(raw) == 0 {
		return nil, rtt, fmt.Errorf("empty response body (status %d)", res.StatusCode)
	}

	switch raw[0] {
	case '{':
	case '"':
		var msg string
		if err := json.Unmarshal(raw, &msg); err != nil {
			return nil, rtt, fmt.Errorf("unreadable string response (status %d): %.60s", res.StatusCode, raw)
		}
		code, detail, _ := strings.Cut(msg, " ")
		return nil, rtt, &stringResult{code: code, detail: detail}
	default:
		return nil, rtt, fmt.Errorf("non-JSON response (status %d): %.60s", res.StatusCode, raw)
	}

	var out regResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, rtt, err
	}
	if out.Time > 0 {
		server := msToTime(out.Time)
		c.tr.write("CLOCK    #%d server=%s local=%s offset=%s (negative means the local clock is behind)",
			seq, server.Format("15:04:05.000"), time.Now().Format("15:04:05.000"),
			time.Since(server).Truncate(time.Millisecond))
	}
	// Auth failure arrives as HTTP 200 with an error field, never a 401.
	if out.Error == resultAuthError {
		return nil, rtt, errAuth
	}
	if out.Error != "" {
		// An error field is a result code too, BLOCKED or NO_REMAINED_ACTION
		// for instance, so it takes the same path as a bare string body.
		return nil, rtt, &stringResult{code: out.Error}
	}
	return &out, rtt, nil
}

// do sends one request and writes the whole exchange to the transcript: the
// status and round trip, the phase timings behind that round trip, every
// response header, and the body. None of it reaches the terminal. The point is
// that a run can be taken apart afterwards without guessing, which is how the
// warm up and the 429s of 2026-09-08 stayed invisible for as long as they did.
func (c *client) do(req *http.Request, seq int) (*http.Response, []byte, time.Duration, error) {
	var (
		reused, dialed          bool
		writes                  int
		start                   time.Time
		dnsAt, connAt, tlsAt    time.Time
		dns, connect, handshake time.Duration
		ttfb                    time.Duration
	)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn:           func(i httptrace.GotConnInfo) { reused = i.Reused },
		DNSStart:          func(httptrace.DNSStartInfo) { dnsAt = time.Now() },
		DNSDone:           func(httptrace.DNSDoneInfo) { dns = time.Since(dnsAt) },
		ConnectStart:      func(string, string) { dialed = true; connAt = time.Now() },
		ConnectDone:       func(string, string, error) { connect = time.Since(connAt) },
		TLSHandshakeStart: func() { tlsAt = time.Now() },
		TLSHandshakeDone:  func(tls.ConnectionState, error) { handshake = time.Since(tlsAt) },
		// More than one write means net/http replayed the request on a fresh
		// connection, which for an add would queue the job twice. The write
		// is also the moment the edge starts counting, so it is what the
		// token is measured from.
		WroteRequest: func(httptrace.WroteRequestInfo) {
			writes++
			c.mu.Lock()
			c.lastCall = time.Now()
			c.mu.Unlock()
		},
		GotFirstResponseByte: func() { ttfb = time.Since(start) },
	}))

	start = time.Now()
	res, err := c.http.Do(req)
	rtt := time.Since(start)

	if err != nil {
		c.tr.write("ERROR    #%d after %s: %v", seq, rtt.Truncate(time.Millisecond), err)
		c.tr.write("TIMING   #%d reused=%t dialed=%t dns=%s connect=%s tls=%s ttfb=%s writes=%d",
			seq, reused, dialed, dns.Truncate(time.Millisecond), connect.Truncate(time.Millisecond),
			handshake.Truncate(time.Millisecond), ttfb.Truncate(time.Millisecond), writes)
		return nil, nil, rtt, err
	}
	defer res.Body.Close()
	raw, readErr := io.ReadAll(res.Body)

	c.tr.write("RESPONSE #%d status=%d rtt=%s proto=%s bytes=%d", seq, res.StatusCode, rtt, res.Proto, len(raw))
	c.tr.write("TIMING   #%d reused=%t dialed=%t dns=%s connect=%s tls=%s ttfb=%s writes=%d",
		seq, reused, dialed, dns.Truncate(time.Millisecond), connect.Truncate(time.Millisecond),
		handshake.Truncate(time.Millisecond), ttfb.Truncate(time.Millisecond), writes)
	c.tr.write("HEADERS  #%d %s", seq, headerLine(res.Header))
	c.tr.write("BODY     #%d %s", seq, clip(raw, 32<<10))
	if writes > 1 {
		c.tr.write("WARNING  #%d the request was written %d times, so the job may have been queued more than once", seq, writes)
	}
	if readErr != nil {
		return res, nil, rtt, readErr
	}
	return res, raw, rtt, nil
}

// headerLine renders the response headers in a stable order on one line. The
// interesting ones are retry-after and x-ratelimit-*, which the edge is not
// documented to send but which cost nothing to record in case it starts.
func headerLine(h http.Header) string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		v := strings.Join(h[k], ", ")
		if strings.EqualFold(k, "Set-Cookie") {
			v = fmt.Sprintf("<%d bytes>", len(v))
		}
		fmt.Fprintf(&b, "%s=%q", strings.ToLower(k), v)
	}
	return b.String()
}

func clip(raw []byte, n int) string {
	if len(raw) <= n {
		return string(raw)
	}
	return fmt.Sprintf("%s... (%d bytes total)", raw[:n], len(raw))
}

// warmUp opens a connection shortly before the window so the first real
// request does not pay a TLS handshake, which measured about 650ms. The GET
// spends a rate limit token like any other request to the host, so it has to
// go out a full gap before the window rather than inside it.
//
// It does not wait for the answer. The token is spent when the request is
// written, and a GET that is slow to come back must not hold the window: the
// portal speaks HTTP/2, so the connection carries the first POST while the GET
// is still outstanding, and if the connection never came up the first POST
// dials one, which is all it would have done without a warm up.
func (c *client) warmUp(ui *ui, fireAt time.Time) {
	if since := time.Since(c.called()); since < warmupSkipIfNewerThan {
		ui.info("warm up  skipped, connection pooled %s ago", since.Truncate(time.Second))
		c.tr.write("WARMUP   skipped, last call %s ago", since.Truncate(time.Millisecond))
		return
	}
	if lead := time.Until(fireAt) - warmupLead; lead > 0 {
		time.Sleep(lead)
	}
	req, err := http.NewRequest(http.MethodGet, portalOrigin+"/", nil)
	if err != nil {
		return
	}
	seq := c.nextSeq()
	c.tr.write("WARMUP   #%d GET %s/ at %s, %s before the window",
		seq, portalOrigin, time.Now().Format("15:04:05.000"), time.Until(fireAt).Truncate(time.Millisecond))
	start := time.Now()
	// Count the token from now, in case the request is never written. The
	// trace moves it to the actual write once that happens.
	c.mu.Lock()
	c.lastCall = start
	c.mu.Unlock()
	go func() {
		if _, _, _, err := c.do(req, seq); err != nil {
			ui.warn("warm up failed: %v, the first request will open its own connection", err)
			return
		}
		took := time.Since(start).Milliseconds()
		if left := time.Until(fireAt); left > 0 {
			ui.info("warm up  connection opened in %dms, %s before the window", took, left.Truncate(time.Millisecond))
		} else {
			ui.warn("warm up  answered in %dms, %s after the window opened", took, (-left).Truncate(time.Millisecond))
		}
	}()
}

// ---------------------------------------------------------------- clock sync

type clockSync struct {
	sent         time.Time
	recv         time.Time
	rtt          time.Duration
	server       time.Time
	registration time.Time
	stale        bool
	remaining    int
}

func (c *client) syncClock(co *course) (*clockSync, error) {
	sent := time.Now()
	resp, rtt, err := c.post(regRequest{Action: "add", Course: co.id, Units: co.units})
	recv := time.Now()
	if err != nil {
		if errors.Is(err, errAuth) {
			return nil, fmt.Errorf("token rejected. log in again, copy a fresh Authorization header, and rerun")
		}
		if errors.Is(err, errRateLimited) {
			return nil, fmt.Errorf("rate limited on the very first request. wait a few seconds and rerun")
		}
		return nil, fmt.Errorf("clock probe failed: %w", err)
	}
	s := &clockSync{
		sent:         sent,
		recv:         recv,
		rtt:          rtt,
		server:       msToTime(resp.Time),
		registration: msToTime(resp.RegistrationTime),
		remaining:    resp.RemainingActions,
	}
	s.stale = s.server.After(s.registration.Add(time.Hour))
	return s, nil
}

// fireTime is when the first request leaves. The server stamps its clock as
// it answers, so at recv the server is already half a round trip past that
// stamp, and the request spends the other half on the way in: sending at
// recv + (window - server) lands a whole network round trip after the window
// opens. The 100ms on top keeps it late through clock granularity and any
// asymmetry, because arriving early is rejected while arriving late costs
// only the delay itself.
//
// The formula used to add the probe's round trip as well, and the probe runs
// on a cold connection, so that included a TLS handshake: one run on
// 2026-09-08 fired 1.2s after the window for nothing.
func (s *clockSync) fireTime(target time.Time) time.Time {
	delay := target.Sub(s.server) + 100*time.Millisecond
	return s.recv.Add(delay)
}

func msToTime(ms int64) time.Time {
	return time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond))
}

// ------------------------------------------------------------------- windows

func resolveWindow(ui *ui, s *clockSync, at string, yes bool) (time.Time, error) {
	if at != "" {
		t, err := parseAt(at, s.server)
		if err != nil {
			return time.Time{}, err
		}
		ui.info("window     %s (from -at)", t.Format("2006-01-02 15:04:05"))
		return t, nil
	}
	if !s.stale {
		ui.info("window     %s (from the server)", s.registration.Format("2006-01-02 15:04:05"))
		return s.registration, nil
	}

	ui.warn("the server's registrationTime is %s, which passed %s ago.",
		s.registration.Format("2006-01-02 15:04:05"),
		s.server.Sub(s.registration).Truncate(time.Minute))
	if yes {
		return time.Time{}, errors.New("window is ambiguous and -y was given, pass -at HH:MM to say which window you mean")
	}
	ui.plain("  which window are you targeting?")
	ui.plain("    1) 08:00")
	ui.plain("    2) 16:00")
	ui.plain("    3) another time")
	for {
		switch strings.TrimSpace(ui.ask("  choice [1/2/3]: ")) {
		case "1":
			return parseAt("08:00", s.server)
		case "2":
			return parseAt("16:00", s.server)
		case "3":
			t, err := parseAt(strings.TrimSpace(ui.ask("  time as HH:MM: ")), s.server)
			if err != nil {
				ui.warn("%v", err)
				continue
			}
			return t, nil
		default:
			ui.warn("enter 1, 2 or 3")
		}
	}
}

// parseAt builds HH:MM on the server's current date. Both clocks read the
// same wall time because the portal is only reachable from inside Iran.
func parseAt(at string, server time.Time) (time.Time, error) {
	parts := strings.Split(strings.TrimSpace(at), ":")
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("cannot read %q as HH:MM", at)
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return time.Time{}, fmt.Errorf("cannot read %q as HH:MM", at)
	}
	return time.Date(server.Year(), server.Month(), server.Day(), h, m, 0, 0, server.Location()), nil
}

// ----------------------------------------------------------------- catalogue

// loadCatalogue resolves courses.json from the explicit override, then the
// copy committed alongside the source, then the published static API. The
// local copy means the tool works with no network at all.
func loadCatalogue(override string) (map[string]catalogueEntry, string, error) {
	var candidates []string
	if override != "" {
		candidates = []string{override}
	} else {
		candidates = append(candidates, localCatalogueCandidates()...)
		candidates = append(candidates, catalogueURL)
	}

	var last error
	for _, src := range candidates {
		raw, err := readSource(src)
		if err != nil {
			last = err
			continue
		}
		var cat map[string]catalogueEntry
		if err := json.Unmarshal(raw, &cat); err != nil {
			last = fmt.Errorf("%s: %w", src, err)
			continue
		}
		if len(cat) == 0 {
			last = fmt.Errorf("%s: no courses", src)
			continue
		}
		return cat, src, nil
	}
	if last == nil {
		last = errors.New("no catalogue source found")
	}
	return nil, "", last
}

func localCatalogueCandidates() []string {
	rel := filepath.Join("docs", "api", "courses.json")
	out := []string{rel}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		out = append(out, filepath.Join(dir, rel), filepath.Join(dir, "..", rel))
	}
	return out
}

func readSource(src string) ([]byte, error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		cl := &http.Client{Timeout: httpTimeout}
		res, err := cl.Get(src)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: status %d", src, res.StatusCode)
		}
		return io.ReadAll(res.Body)
	}
	return os.ReadFile(src)
}

// --------------------------------------------------------------------- setup

func resolveToken(ui *ui, flagVal string, yes bool) (string, error) {
	tok := cleanToken(flagVal)
	if tok == "" {
		tok = cleanToken(os.Getenv("MYEDU_TOKEN"))
	}
	if tok != "" {
		ui.info("token      %s (%s)", mask(tok), plural(len(tok), "char"))
		return tok, nil
	}
	if yes {
		return "", errors.New("-y needs a token, pass -token or set MYEDU_TOKEN")
	}
	ui.rule("token")
	ui.plain("  Log in at https://my.edu.sharif.edu, open the network tab, and copy")
	ui.plain("  the Authorization request header from any API call.")
	for i := 0; i < 3; i++ {
		tok = cleanToken(ui.ask("  token: "))
		if tok != "" {
			ui.good("  accepted %s (%s)", mask(tok), plural(len(tok), "char"))
			return tok, nil
		}
		ui.warn("  empty, try again")
	}
	return "", errors.New("no token given")
}

// cleanToken absorbs the usual copy and paste damage.
func cleanToken(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'")
	s = strings.TrimSpace(strings.TrimPrefix(s, "Bearer "))
	s = strings.TrimSpace(strings.TrimPrefix(s, "bearer "))
	return strings.TrimSpace(strings.Trim(s, "\"'"))
}

func mask(s string) string {
	if len(s) <= 14 {
		return "..."
	}
	return s[:8] + "..." + s[len(s)-4:]
}

func resolveCourses(ui *ui, cat map[string]catalogueEntry, spec string, yes bool) ([]*course, error) {
	if spec != "" {
		return parseCourses(cat, strings.Split(spec, ","))
	}
	if yes {
		return nil, errors.New("-y needs a course list, pass -courses")
	}
	ui.rule("courses, most contested first")
	ui.plain("  One per line as CODE-GROUP, for example 22034-2.")
	ui.plain("  Units come from the catalogue. Append :N to override, for example 40760-1:2.")
	ui.plain("  Order is priority: the first course gets the first request at the window.")
	ui.plain("  Press Enter on an empty line when the list is complete.")
	for {
		var raw []string
		for {
			line := strings.TrimSpace(ui.ask(fmt.Sprintf("  %d > ", len(raw)+1)))
			if line == "" {
				break
			}
			one, err := parseCourses(cat, []string{line})
			if err != nil {
				ui.warn("      %v", err)
				continue
			}
			c := one[0]
			ui.good("      %s  %s  %s, capacity %d", c.id, c.title, plural(int(c.units), "unit"), cat[c.id].Capacity)
			raw = append(raw, line)
		}
		if len(raw) == 0 {
			ui.warn("  the list is empty, add at least one course")
			continue
		}
		return parseCourses(cat, raw)
	}
}

func parseCourses(cat map[string]catalogueEntry, specs []string) ([]*course, error) {
	var out []*course
	seen := map[string]bool{}
	for _, s := range specs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		id := s
		var override int32 = -1
		if i := strings.LastIndex(s, ":"); i >= 0 {
			id = strings.TrimSpace(s[:i])
			n, err := strconv.Atoi(strings.TrimSpace(s[i+1:]))
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%q: units override must be a number", s)
			}
			override = int32(n)
		}
		entry, ok := cat[id]
		if !ok {
			return nil, fmt.Errorf("%q is not in the catalogue, check the code and group", id)
		}
		if seen[id] {
			return nil, fmt.Errorf("%q is listed twice", id)
		}
		seen[id] = true

		units := entry.Units
		if override >= 0 {
			if !entry.variableUnits() && override != entry.Units {
				return nil, fmt.Errorf("%s takes exactly %s and is not variable", id, plural(int(entry.Units), "unit"))
			}
			if override > entry.Units {
				return nil, fmt.Errorf("%s allows at most %s", id, plural(int(entry.Units), "unit"))
			}
			units = override
		}
		out = append(out, &course{id: id, units: units, title: entry.Title, pri: len(out)})
	}
	if len(out) == 0 {
		return nil, errors.New("no courses given")
	}
	return out, nil
}

func confirm(ui *ui, courses []*course, s *clockSync, target, fireAt time.Time, yes bool) bool {
	ui.rule("ready")
	var total int32
	for i, c := range courses {
		total += c.units
		ui.plain("  %d. %-9s %-34s %s", i+1, c.id, trimTitle(c.title, 34), plural(int(c.units), "unit"))
	}
	ui.plain("     %s, %s total", plural(len(courses), "course"), plural(int(total), "unit"))
	if s.remaining > 0 {
		ui.plain("     %d remove or group change actions remaining", s.remaining)
	}
	ui.plain("  window opens  %s", target.Format("2006-01-02 15:04:05.000"))
	ui.plain("  firing at     %s (in %s)", fireAt.Format("15:04:05.000"), time.Until(fireAt).Truncate(time.Millisecond))
	ui.plain("  pacing        one request every %s, %s per course, up to %d in flight", globalGap, courseCooldown, inflightCap(len(courses)))
	if yes {
		return true
	}
	a := strings.ToLower(strings.TrimSpace(ui.ask("  start? [Y/n]: ")))
	return a == "" || a == "y" || a == "yes"
}

func trimTitle(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func report(ui *ui, courses []*course) int {
	ui.rule("summary")
	outstanding := 0
	for _, c := range courses {
		if c.done {
			ui.good("  registered   %-9s %-30s at %s after %s",
				c.id, trimTitle(c.title, 30), c.landed.Format("15:04:05.000"), plural(c.attempts, "attempt"))
			continue
		}
		outstanding++
		last := c.last
		if last == "" {
			last = "no attempt"
		}
		ui.bad("  outstanding  %-9s %-30s %s, last %s",
			c.id, trimTitle(c.title, 30), plural(c.attempts, "attempt"), last)
	}
	ui.tr.write("BOARD    %s", board(courses))
	if outstanding == 0 {
		ui.good("  everything landed")
		return 0
	}
	return 1
}

// ---------------------------------------------------------------- transcript

type transcript struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

func newTranscript(path string) (*transcript, error) {
	if path == "" {
		path = fmt.Sprintf("snipe-%s.log", time.Now().Format("20060102-150405"))
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	t := &transcript{f: f, path: path}
	t.write("SESSION  started %s", time.Now().Format(time.RFC3339Nano))
	return t, nil
}

func (t *transcript) write(format string, args ...any) {
	if t == nil || t.f == nil {
		return
	}
	line := fmt.Sprintf(format, args...)
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(t.f, "%s %s\n", time.Now().Format("15:04:05.000"), line)
}

func (t *transcript) Close() error {
	if t == nil || t.f == nil {
		return nil
	}
	t.write("SESSION  ended")
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.f.Close()
}

// ------------------------------------------------------------------------ ui

type kind int

const (
	kindGood kind = iota
	kindBad
	kindWarn
)

type ui struct {
	color     bool
	in        *bufio.Reader
	tr        *transcript
	remaining int
}

func newUI() *ui {
	u := &ui{in: bufio.NewReader(os.Stdin)}
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		u.color = enableANSI(os.Stdout.Fd())
	}
	return u
}

func (u *ui) paint(c string, s string) string {
	if !u.color {
		return s
	}
	return c + s + "\033[0m"
}

const (
	cGreen = "\033[32m"
	cRed   = "\033[31m"
	cYell  = "\033[33m"
	cDim   = "\033[2m"
	cBold  = "\033[1m"
)

func stamp() string { return time.Now().Format("15:04:05.000") }

func (u *ui) emit(line string) {
	fmt.Println(line)
	u.tr.write("UI       %s", stripANSI(line))
}

func stripANSI(s string) string {
	for {
		i := strings.Index(s, "\033[")
		if i < 0 {
			return s
		}
		j := strings.IndexByte(s[i:], 'm')
		if j < 0 {
			return s
		}
		s = s[:i] + s[i+j+1:]
	}
}

func (u *ui) plain(f string, a ...any) { u.emit(fmt.Sprintf(f, a...)) }
func (u *ui) info(f string, a ...any) {
	u.emit(u.paint(cDim, stamp()) + "  " + fmt.Sprintf(f, a...))
}
func (u *ui) good(f string, a ...any) { u.emit(u.paint(cGreen, fmt.Sprintf(f, a...))) }
func (u *ui) bad(f string, a ...any)  { u.emit(u.paint(cRed, fmt.Sprintf(f, a...))) }
func (u *ui) warn(f string, a ...any) { u.emit(u.paint(cYell, fmt.Sprintf(f, a...))) }
func (u *ui) fatal(f string, a ...any) {
	u.emit(u.paint(cRed, "error: "+fmt.Sprintf(f, a...)))
}

func (u *ui) banner() {
	u.emit(u.paint(cBold, "my.edu.sharif.edu sniper "+buildVersion()))
}

func (u *ui) rule(title string) {
	u.emit("")
	u.emit(u.paint(cBold, "== "+title+" "+strings.Repeat("=", max(0, 56-len(title)))))
}

func (u *ui) ask(prompt string) string {
	fmt.Print(prompt)
	line, err := u.in.ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	u.tr.write("INPUT    %s%s", prompt, strings.TrimSpace(line))
	return strings.TrimRight(line, "\r\n")
}

func (u *ui) bell() {
	if u.color {
		fmt.Print("\a")
	}
}

func (u *ui) setRemaining(n int) {
	if n == 0 || n == u.remaining {
		return
	}
	if u.remaining != 0 {
		u.info("remove or group change actions remaining: %d", n)
	}
	u.remaining = n
}

// attempt prints one attempt line:
//
//	16:00:00.412  #1  22034-2  OK  -> registered in 412ms
//
// n is the attempt this line reports, which is not always the course's current
// count now that several requests can be in flight at once.
func (u *ui) attempt(c *course, n int, result, note string, rtt time.Duration, k kind) {
	col := cDim
	switch k {
	case kindGood:
		col = cGreen
	case kindBad:
		col = cRed
	case kindWarn:
		col = cYell
	}
	line := fmt.Sprintf("%s  %s  %-9s  %s -> %s",
		u.paint(cDim, stamp()),
		u.paint(cDim, fmt.Sprintf("#%d", n)),
		c.id,
		u.paint(col, fmt.Sprintf("%-19s", result)),
		note)
	u.emit(line)
}

func (u *ui) showClockSync(s *clockSync) {
	u.rule("clock sync")
	drift := s.recv.Sub(s.server)
	dir := "ahead of"
	if drift < 0 {
		drift, dir = -drift, "behind"
	}
	u.plain("  probe sent        %s  (local)", s.sent.Format("15:04:05.000"))
	u.plain("  reply received    %s  (local)", s.recv.Format("15:04:05.000"))
	u.plain("  round trip        %s", s.rtt.Truncate(time.Millisecond))
	u.plain("  server clock      %s  (server)", s.server.Format("15:04:05.000"))
	u.plain("  your clock is     %s %s the server", drift.Truncate(time.Millisecond), dir)
	reg := s.registration.Format("2006-01-02 15:04:05")
	if s.stale {
		u.plain("  registrationTime  %s  %s", reg,
			u.paint(cYell, fmt.Sprintf("(stale, passed %s ago)", humanDur(s.server.Sub(s.registration)))))
	} else {
		u.plain("  registrationTime  %s", reg)
	}
}

func (u *ui) showFireTime(s *clockSync, target, fireAt time.Time) {
	gap := target.Sub(s.server)
	u.plain("")
	u.plain("  fire = received + (window - serverClock) + 100ms")
	u.plain("       = %s + %s + 100ms",
		s.recv.Format("15:04:05.000"), gap.Truncate(time.Millisecond))
	u.plain("  %s", u.paint(cDim, fmt.Sprintf("the request lands a network round trip after that, about %s here",
		s.rtt.Truncate(time.Millisecond))))
	u.plain("       = %s", u.paint(cBold, fireAt.Format("15:04:05.000")))
	u.plain("  %s", u.paint(cDim, "the margin is biased late on purpose: arriving early is rejected"))
	u.plain("  %s", u.paint(cDim, "and costs a full cooldown, arriving late costs only the delay"))
}

// waitUntil logs a heartbeat that tightens as the window approaches, then
// counts down the final minute one line per second. It returns at until, which
// is where the warm up goes, while the countdown it prints is always the time
// left to fireAt.
func (u *ui) waitUntil(ctx context.Context, until, fireAt time.Time) {
	d := time.Until(fireAt)
	if d <= 0 {
		u.warn("the window opened %s ago, firing immediately", (-d).Truncate(time.Second))
		return
	}
	u.rule("waiting")
	u.info("the sniper will fire in %s, at %s", d.Truncate(time.Second), fireAt.Format("15:04:05.000"))
	u.info("press Ctrl-C to abort")

	for {
		if !time.Now().Before(until) {
			return
		}
		rem := time.Until(fireAt)
		if rem <= 0 {
			return
		}
		var step time.Duration
		switch {
		case rem > 10*time.Minute:
			step = 10 * time.Minute
		case rem > time.Minute:
			step = time.Minute
		default:
			step = time.Second
		}
		wait := rem % step
		if wait == 0 {
			wait = step
		}
		if !sleepCtx(ctx, wait) {
			return
		}
		rem = time.Until(fireAt)
		if rem <= 0 {
			return
		}
		secs := int((rem + 100*time.Millisecond).Seconds())
		if rem <= time.Minute {
			col := cDim
			if secs <= 10 {
				col = cBold
			}
			u.emit(u.paint(cDim, stamp()) + "  " + u.paint(col, strconv.Itoa(secs)))
		} else {
			u.info("%s to go", rem.Round(time.Second))
		}
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// humanDur renders a long gap as days and hours rather than raw hours.
func humanDur(d time.Duration) string {
	if d < time.Hour {
		return d.Truncate(time.Minute).String()
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	if days == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%s %dh", plural(days, "day"), hours)
}
