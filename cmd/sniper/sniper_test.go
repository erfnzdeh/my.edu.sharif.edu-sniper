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
	defer swap(&maxInflight, 3)()

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
	// Serially this is six courses times a 300ms answer. Overlapping them has
	// to beat that clearly, or the concurrency is not doing anything.
	if p.maxAtOnce < 2 {
		t.Errorf("never had more than %d request in flight, requests are not overlapping", p.maxAtOnce)
	}
	if took > 1500*time.Millisecond {
		t.Errorf("took %s, want well under the 1.8s a serial run would need", took)
	}
	if p.maxAtOnce > maxInflight {
		t.Errorf("%d requests were in flight at once, the cap is %d", p.maxAtOnce, maxInflight)
	}
	assertSpacing(t, p.arrivals, globalGap)
}

// TestFireWindowKeepsTheGapWhenAnswersAreInstant guards the case the edge
// actually rejects: two requests less than a token apart.
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

func swap[T any](p *T, v T) func() {
	old := *p
	*p = v
	return func() { *p = old }
}
