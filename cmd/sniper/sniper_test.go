package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// newTestUI returns a ui that writes nowhere useful but is safe to call.
func newTestUI() *ui { return &ui{} }

func mkCourses(ids ...string) []*course {
	out := make([]*course, 0, len(ids))
	for i, id := range ids {
		out = append(out, &course{id: id, units: 1, title: id, pri: i})
	}
	return out
}

func TestChooseCoversEveryCourseBeforeRetrying(t *testing.T) {
	courses := mkCourses("a", "b", "c")
	now := time.Now()

	// The first course has already been judged once, the others never. The
	// untried courses have to come first, in list order.
	courses[0].judged = 1

	pick, _ := choose(courses, now)
	if pick.id != "b" {
		t.Fatalf("first pick = %s, want b", pick.id)
	}
	pick.judged = 1
	if pick, _ = choose(courses, now); pick.id != "c" {
		t.Fatalf("second pick = %s, want c", pick.id)
	}
	pick.judged = 1
	if pick, _ = choose(courses, now); pick.id != "a" {
		t.Fatalf("third pick = %s, want a, the highest priority course", pick.id)
	}
}

func TestChooseSkipsPendingDoneAndCoolingCourses(t *testing.T) {
	courses := mkCourses("a", "b", "c")
	now := time.Now()
	courses[0].done = true
	courses[1].pending = true
	courses[2].next = now.Add(3 * time.Second)

	pick, earliest := choose(courses, now)
	if pick != nil {
		t.Fatalf("pick = %s, want nothing eligible", pick.id)
	}
	if !earliest.Equal(courses[2].next) {
		t.Fatalf("earliest = %s, want %s", earliest, courses[2].next)
	}
}

func TestChooseLeavesParkedCoursesLast(t *testing.T) {
	courses := mkCourses("a", "b")
	courses[0].parked = true // highest priority, but it cannot land
	pick, _ := choose(courses, time.Now())
	if pick.id != "b" {
		t.Fatalf("pick = %s, want b", pick.id)
	}
}

func TestApplyCodeParksPermanentFailureAndQueuedIsNotOne(t *testing.T) {
	c := &course{id: "40124-1", next: time.Now().Add(courseCooldown)}
	applyCode(newTestUI(), c, 1, "CLASS_OVERLAP", 0)
	if !c.parked {
		t.Fatal("CLASS_OVERLAP did not park the course")
	}
	if time.Until(c.next) < parkedBackoff-time.Second {
		t.Fatalf("parked course comes back in %s, want about %s", time.Until(c.next), parkedBackoff)
	}

	q := &course{id: "30004-1", next: time.Now().Add(courseCooldown)}
	applyCode(newTestUI(), q, 1, "REPEATED_REQUEST", 0)
	if q.parked || q.done {
		t.Fatalf("REPEATED_REQUEST parked=%t done=%t, want neither", q.parked, q.done)
	}

	ok := &course{id: "40416-1"}
	applyCode(newTestUI(), ok, 1, resultDuplicate, 0)
	if !ok.done {
		t.Fatal("COURSE_DUPLICATE did not land the course")
	}
}

// portal is a stand in for /api/reg. It answers slowly, so a scheduler that
// waits for each answer before sending the next request cannot finish in time.
type portal struct {
	mu         sync.Mutex
	delay      time.Duration
	seen       []string
	registered map[string]bool
	arrivals   []time.Time
	maxAtOnce  int
	atOnce     int
}

func (p *portal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body regRequest
	json.NewDecoder(r.Body).Decode(&body)

	p.mu.Lock()
	p.arrivals = append(p.arrivals, time.Now())
	p.seen = append(p.seen, body.Course)
	p.atOnce++
	if p.atOnce > p.maxAtOnce {
		p.maxAtOnce = p.atOnce
	}
	p.mu.Unlock()

	time.Sleep(p.delay)

	p.mu.Lock()
	p.atOnce--
	p.registered[body.Course] = true
	jobs := make([]job, 0, len(p.registered))
	for id := range p.registered {
		jobs = append(jobs, job{CourseID: id, Result: resultOK})
	}
	p.mu.Unlock()

	json.NewEncoder(w).Encode(regResponse{Jobs: jobs, Time: time.Now().UnixMilli()})
}

func TestFireWindowLandsEveryCourseWhileAnswersAreSlow(t *testing.T) {
	p := &portal{delay: 300 * time.Millisecond, registered: map[string]bool{}}
	srv := httptest.NewServer(p)
	defer srv.Close()

	defer swap(&regEndpoint, srv.URL)()
	defer swap(&globalGap, 40*time.Millisecond)()
	defer swap(&maxInflight, 3)() // pinned, this test is about the cap holding

	courses := mkCourses("a", "b", "c", "d", "e", "f")
	cl := &client{http: &http.Client{Timeout: 5 * time.Second}}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	start := time.Now()
	if fireWindow(ctx, newTestUI(), cl, courses) {
		t.Fatal("fireWindow reported an auth failure")
	}
	took := time.Since(start)

	for _, c := range courses {
		if !c.done {
			t.Errorf("%s never landed", c.id)
		}
	}
	// The structural check is the real one: requests have to overlap at all.
	if p.maxAtOnce < 2 {
		t.Errorf("never had more than %d request in flight, requests are not overlapping", p.maxAtOnce)
	}
	// The timing check is bounded by what a serial run would cost rather than
	// by a fixed number, so a loaded machine cannot fail it on its own.
	if serial := time.Duration(len(courses)) * p.delay; took >= serial {
		t.Errorf("took %s, a serial run would have been %s, so nothing was gained", took, serial)
	}
	if p.maxAtOnce > maxInflight {
		t.Errorf("%d requests were in flight at once, the cap is %d", p.maxAtOnce, maxInflight)
	}
	assertSpacing(t, p.arrivals, globalGap)
}

// TestFireWindowKeepsTheGapWhenAnswersAreInstant guards the case the edge
// actually rejects: two requests less than a token apart.
// TestFireWindowUncappedUsesTheWholeList is the default shape: no cap, so the
// only ceiling is one request per course. It has to overlap more than the old
// fixed cap of three would have allowed.
func TestFireWindowUncappedUsesTheWholeList(t *testing.T) {
	p := &portal{delay: 700 * time.Millisecond, registered: map[string]bool{}}
	srv := httptest.NewServer(p)
	defer srv.Close()

	defer swap(&regEndpoint, srv.URL)()
	defer swap(&globalGap, 40*time.Millisecond)()
	defer swap(&maxInflight, 0)() // no cap

	courses := mkCourses("a", "b", "c", "d", "e", "f")
	cl := &client{http: &http.Client{Timeout: 5 * time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	fireWindow(ctx, newTestUI(), cl, courses)

	for _, c := range courses {
		if !c.done {
			t.Errorf("%s never landed", c.id)
		}
	}
	if p.maxAtOnce <= 3 {
		t.Errorf("peaked at %d in flight, an uncapped run with 700ms answers should beat the old cap of 3", p.maxAtOnce)
	}
	if p.maxAtOnce > len(courses) {
		t.Errorf("%d in flight for %d courses, a course must never have two requests out at once", p.maxAtOnce, len(courses))
	}
	assertSpacing(t, p.arrivals, globalGap)
}

// TestFireWindowInflightOneIsSerial checks the escape hatch still works, since
// it is what someone would reach for if TOO_MANY_REQUESTS ever showed up.
func TestFireWindowInflightOneIsSerial(t *testing.T) {
	p := &portal{delay: 200 * time.Millisecond, registered: map[string]bool{}}
	srv := httptest.NewServer(p)
	defer srv.Close()

	defer swap(&regEndpoint, srv.URL)()
	defer swap(&globalGap, 20*time.Millisecond)()
	defer swap(&maxInflight, 1)()

	courses := mkCourses("a", "b", "c")
	cl := &client{http: &http.Client{Timeout: 5 * time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	fireWindow(ctx, newTestUI(), cl, courses)
	if p.maxAtOnce != 1 {
		t.Errorf("peaked at %d in flight, -inflight 1 must stay serial", p.maxAtOnce)
	}
}

func TestInflightCapNeverExceedsTheCourseCount(t *testing.T) {
	defer swap(&maxInflight, 0)()
	if got := inflightCap(6); got != 6 {
		t.Errorf("uncapped with 6 courses = %d, want 6", got)
	}
	swap(&maxInflight, 99)
	if got := inflightCap(6); got != 6 {
		t.Errorf("cap of 99 with 6 courses = %d, want 6", got)
	}
	swap(&maxInflight, 2)
	if got := inflightCap(6); got != 2 {
		t.Errorf("cap of 2 with 6 courses = %d, want 2", got)
	}
}

func TestFireWindowKeepsTheGapWhenAnswersAreInstant(t *testing.T) {
	p := &portal{delay: 0, registered: map[string]bool{}}
	srv := httptest.NewServer(p)
	defer srv.Close()

	defer swap(&regEndpoint, srv.URL)()
	defer swap(&globalGap, 80*time.Millisecond)()
	defer swap(&maxInflight, 3)()

	courses := mkCourses("a", "b", "c", "d")
	cl := &client{http: &http.Client{Timeout: 5 * time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	fireWindow(ctx, newTestUI(), cl, courses)
	assertSpacing(t, p.arrivals, globalGap)
}

func assertSpacing(t *testing.T, arrivals []time.Time, gap time.Duration) {
	t.Helper()
	// A little slack for the hop through the loopback.
	slack := 2 * time.Millisecond
	for i := 1; i < len(arrivals); i++ {
		if d := arrivals[i].Sub(arrivals[i-1]); d+slack < gap {
			t.Errorf("requests %d and %d arrived %s apart, the token is %s", i-1, i, d, gap)
		}
	}
}

func TestPostReadsABareStringResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `"REPEATED_REQUEST 40111099930004-11add"`)
	}))
	defer srv.Close()
	defer swap(&regEndpoint, srv.URL)()

	cl := &client{http: &http.Client{Timeout: time.Second}}
	_, _, err := cl.add(&course{id: "30004-1", units: 1})

	var str *stringResult
	if !errors.As(err, &str) {
		t.Fatalf("err = %v, want a stringResult", err)
	}
	if str.code != "REPEATED_REQUEST" {
		t.Errorf("code = %q, want REPEATED_REQUEST", str.code)
	}
	if str.detail != "40111099930004-11add" {
		t.Errorf("detail = %q, want the job key", str.detail)
	}
}

func TestPostReportsRateLimiting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "<html><body>429</body></html>")
	}))
	defer srv.Close()
	defer swap(&regEndpoint, srv.URL)()

	cl := &client{http: &http.Client{Timeout: time.Second}}
	if _, _, err := cl.add(&course{id: "30004-1", units: 1}); !errors.Is(err, errRateLimited) {
		t.Fatalf("err = %v, want errRateLimited", err)
	}
}

// TestApplyJobsReadsTheNewestJobEvenBeforeItIsJudged pins the run A shape of
// 2026-09-08: the course's own job is still queued, and an older job for the
// same course carries a stale verdict. The stale one must not be read.
func TestApplyJobsReadsTheNewestJobEvenBeforeItIsJudged(t *testing.T) {
	courses := mkCourses("37514-2", "40416-1")
	pick := courses[0]
	pick.next = time.Now().Add(courseCooldown)
	resp := &regResponse{Jobs: []job{
		{CourseID: "37514-2", Result: ""},              // this attempt, not judged yet
		{CourseID: "37514-2", Result: "CLASS_OVERLAP"}, // an earlier attempt
		{CourseID: "40416-1", Result: resultOK},
	}}
	applyJobs(newTestUI(), courses, pick, 1, resp, 0)

	if pick.last != "QUEUED" {
		t.Fatalf("last = %q, want QUEUED, the stale verdict was read instead", pick.last)
	}
	if pick.parked {
		t.Fatal("a stale CLASS_OVERLAP parked a course whose real job is still queued")
	}
	if pick.judged != 1 {
		t.Fatalf("judged = %d, want 1, the backend did see the request", pick.judged)
	}
	if !courses[1].done {
		t.Fatal("an OK for another course was not harvested")
	}
}

// TestFireTimeDoesNotAddTheProbeRoundTrip: the probe runs on a cold
// connection, so its round trip includes a TLS handshake that the request at
// the window will never pay. Sending at recv + (window - server) already lands
// a network round trip late, and only the fixed 100ms goes on top.
func TestFireTimeDoesNotAddTheProbeRoundTrip(t *testing.T) {
	recv := time.Date(2026, 9, 8, 7, 53, 13, 153e6, time.Local)
	s := &clockSync{
		recv:   recv,
		rtt:    582 * time.Millisecond,
		server: recv.Add(541 * time.Millisecond),
	}
	window := time.Date(2026, 9, 8, 8, 0, 0, 0, time.Local)
	got := s.fireTime(window)
	want := recv.Add(window.Sub(s.server) + 100*time.Millisecond)
	if !got.Equal(want) {
		t.Fatalf("fireTime = %s, want %s", got.Format("15:04:05.000"), want.Format("15:04:05.000"))
	}
	// The local clock is 541ms behind the server, so add that to read the
	// fire time on the server's clock.
	if late := got.Add(541 * time.Millisecond).Sub(window); late != 100*time.Millisecond {
		t.Fatalf("fires %s after the window by the server's clock, want 100ms", late)
	}
}

func TestApplyCodeTimingRejectionKeepsThePlace(t *testing.T) {
	c := &course{id: "30004-1", next: time.Now().Add(courseCooldown)}
	applyCode(newTestUI(), c, 1, "NO_REGISTRATION_TIME", 0)
	if c.judged != 0 || c.parked || c.done {
		t.Fatalf("judged=%d parked=%t done=%t after NO_REGISTRATION_TIME, want 0 false false", c.judged, c.parked, c.done)
	}
	if c.last != "NO_REGISTRATION_TIME" {
		t.Fatalf("last = %q, want the code recorded", c.last)
	}
	applyCode(newTestUI(), c, 2, "CAPACITY_EXCEEDED", 0)
	if c.judged != 1 {
		t.Fatalf("judged = %d after a real verdict, want 1", c.judged)
	}
}

// TestTimingRejectionIsOnlyFreeOnce: a partial window, where the portal keeps
// refusing one course on timing grounds while the others get real verdicts.
// After its free first rejection the refused course has to count like any
// other, or it sits at judged 0 and takes the token ahead of every course that
// can still land, every time it comes off cooldown, for the rest of the run.
func TestTimingRejectionIsOnlyFreeOnce(t *testing.T) {
	courses := mkCourses("40760-1", "40634-1")
	refused, judged := courses[0], courses[1]
	now := time.Now()

	applyCode(newTestUI(), refused, 1, "REGISTRATION_TIME_LIMIT", 0)
	applyCode(newTestUI(), judged, 1, "CAPACITY_EXCEEDED", 0)
	if pick, _ := choose(courses, now); pick != refused {
		t.Fatalf("after one timing rejection pick = %s, want the refused course to keep its place", pick.id)
	}

	applyCode(newTestUI(), refused, 2, "REGISTRATION_TIME_LIMIT", 0)
	if refused.judged != 1 {
		t.Fatalf("judged = %d after a second timing rejection, want 1", refused.judged)
	}
	if refused.parked || refused.done {
		t.Fatalf("parked=%t done=%t, a timing code is neither permanent nor a success", refused.parked, refused.done)
	}
	// Both are now judged once, so list order decides again, and a third
	// rejection puts the refused course behind the one with a real verdict.
	applyCode(newTestUI(), refused, 3, "REGISTRATION_TIME_LIMIT", 0)
	if pick, _ := choose(courses, now); pick != judged {
		t.Fatalf("after three timing rejections pick = %s, want the course with a real verdict", pick.id)
	}
}

func TestPostTurnsAnErrorFieldIntoAResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":"TOO_MANY_REQUESTS"}`)
	}))
	defer srv.Close()
	defer swap(&regEndpoint, srv.URL)()

	cl := &client{http: &http.Client{Timeout: time.Second}}
	_, _, err := cl.add(&course{id: "30004-1", units: 1})

	var str *stringResult
	if !errors.As(err, &str) {
		t.Fatalf("err = %v, want a stringResult", err)
	}
	if str.code != "TOO_MANY_REQUESTS" {
		t.Errorf("code = %q, want TOO_MANY_REQUESTS", str.code)
	}
	if holdResults[str.code].hold == 0 {
		t.Errorf("TOO_MANY_REQUESTS does not hold the shared token")
	}
}

// TestWarmUpDoesNotHoldTheWindow: the warm up GET is sent and forgotten. A
// portal that sits on it must not delay the first real request, and the token
// still counts from the moment it went out.
func TestWarmUpDoesNotHoldTheWindow(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release) // runs before srv.Close, which waits for handlers
	defer swap(&portalOrigin, srv.URL)()

	cl := &client{http: &http.Client{Timeout: 5 * time.Second}}
	before := time.Now()
	cl.warmUp(newTestUI(), before.Add(50*time.Millisecond))
	if took := time.Since(before); took > 500*time.Millisecond {
		t.Fatalf("warmUp blocked for %s waiting on an answer that never came", took)
	}
	if called := cl.called(); called.Before(before) {
		t.Fatalf("the token does not count from the warm up: last call %s, warm up at %s", called, before)
	}
}

func swap[T any](p *T, v T) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

// TestRejectionDoesNotAdvanceTheAcceptedClock pins the finding the pacing now
// rests on: the edge counts what it accepts, so a 429 must leave that clock
// alone. Measured 2026-09-08, 98 of 98 requests sent under a second after the
// last accepted one were rejected, while a rejected request never reset it.
func TestRejectionDoesNotAdvanceTheAcceptedClock(t *testing.T) {
	var reject bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reject {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, "<html>429</html>")
			return
		}
		json.NewEncoder(w).Encode(regResponse{Time: time.Now().UnixMilli()})
	}))
	defer srv.Close()
	defer swap(&regEndpoint, srv.URL)()

	cl := &client{http: &http.Client{Timeout: 2 * time.Second}}

	reject = true
	if _, _, err := cl.add(&course{id: "a", units: 1}); !errors.Is(err, errRateLimited) {
		t.Fatalf("err = %v, want errRateLimited", err)
	}
	if !cl.accepted().IsZero() {
		t.Error("a rejected request advanced the accepted clock, so pacing would wait for a token that was never spent")
	}
	if cl.called().IsZero() {
		t.Error("a rejected request should still count as a call, it did leave the machine")
	}

	reject = false
	if _, _, err := cl.add(&course{id: "a", units: 1}); err != nil {
		t.Fatalf("second request: %v", err)
	}
	if cl.accepted().IsZero() {
		t.Error("an accepted request did not advance the accepted clock")
	}
}

// TestFireWindowRetriesSoonAfterARejection is the change that matters at a
// window. A rejection was never counted, so the next attempt is due a token
// after the last ACCEPTED request, not a fixed backoff after the rejection.
// The old code waited rateLimitBackoff, two seconds, from the rejection.
func TestFireWindowRetriesSoonAfterARejection(t *testing.T) {
	var (
		mu       sync.Mutex
		arrivals []time.Time
		rejected []bool
		first    = true
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body regRequest
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		rej := first
		first = false
		rejected = append(rejected, rej)
		mu.Unlock()

		if rej {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, "<html>429</html>")
			return
		}
		json.NewEncoder(w).Encode(regResponse{
			Jobs: []job{{CourseID: body.Course, Result: resultOK}}, Time: time.Now().UnixMilli(),
		})
	}))
	defer srv.Close()

	defer swap(&regEndpoint, srv.URL)()
	defer swap(&globalGap, 300*time.Millisecond)()
	defer swap(&maxInflight, 0)()

	courses := mkCourses("a", "b")
	cl := &client{http: &http.Client{Timeout: 3 * time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fireWindow(ctx, newTestUI(), cl, courses)

	mu.Lock()
	defer mu.Unlock()
	if len(arrivals) < 2 || !rejected[0] {
		t.Fatalf("expected a rejection followed by a retry, got %d requests", len(arrivals))
	}
	recovery := arrivals[1].Sub(arrivals[0])
	// Nothing had been accepted yet, so the retry is due one gap after the
	// zero clock, which is immediately, plus the poll delay. Either way it
	// must be nowhere near the two second backoff this replaced.
	if recovery > time.Second {
		t.Errorf("waited %s after a rejection before retrying, want well under the 2s the old fixed backoff cost", recovery)
	}
	for _, c := range courses {
		if !c.done {
			t.Errorf("%s never landed", c.id)
		}
	}
}
