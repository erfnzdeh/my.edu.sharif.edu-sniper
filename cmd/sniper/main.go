// Command sniper registers courses on my.edu.sharif.edu the moment the
// registration window opens.
//
// The portal's edge allows exactly one request per second with no burst, so
// the scheduler spends one request token at a time on the highest priority
// course that is off its own cooldown. List order is priority order.
package main

import (
	"bufio"
	"bytes"
	"context"
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
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Measured against the live API. See README for how these were established.
const (
	regEndpoint   = "https://my.edu.sharif.edu/api/reg"
	portalOrigin  = "https://my.edu.sharif.edu"
	portalReferer = "https://my.edu.sharif.edu/courses/offered"

	// The edge limiter permits one request per second and rejects anything
	// faster outright, so leave a little headroom on the boundary.
	globalGap = 1100 * time.Millisecond
	// The portal also enforces five seconds between two requests for the
	// same course. With five or more courses the global gap already covers
	// this, but with fewer it is the binding constraint.
	courseCooldown = 5200 * time.Millisecond
	// Backoff applied to every course when the edge returns 429.
	rateLimitBackoff = 7 * time.Second
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

// Result codes, taken from the portal frontend bundle.
const (
	resultOK        = "OK"
	resultDuplicate = "COURSE_DUPLICATE"
	resultAuthError = "AUTHORIZATION"
)

// permanentFailures lists the results that can never succeed on a retry. Courses are
// still retried, because the operator watches the log and decides, but these
// are called out loudly so a typo or a clash is obvious at a glance.
var permanentFailures = map[string]string{
	"INVALID_COURSE":          "no such course, check the code and group",
	"INCORRECT_UNIT_NUMBER":   "wrong unit count for this course",
	"UNITS_LIMIT":             "this would exceed your total unit limit",
	"CLASS_OVERLAP":           "class time clashes with another course",
	"EXAM_OVERLAP":            "exam time clashes with another course",
	"COURSE_TAKEN_BEFORE":     "already passed this course",
	"MAAREF_COURSES_LIMIT":    "hit the limit on maaref courses",
	"INCOMPATIBLE_CAMPUS":     "wrong campus",
	"INCOMPATIBLE_GENDER":     "not open to your gender",
	"UNSUPPORTED_COURSE_TYPE": "this course type cannot be added here",
	"NO_REMAINED_ACTION":      "no registration actions left",
	"NO_PERMISSION":           "no permission to register for this course",
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
	id       string
	units    int32
	title    string
	attempts int
	next     time.Time // not eligible to be retried before this
	done     bool
	landed   time.Time
	last     string // most recent result seen
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
	)
	flag.Parse()

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

	cl := &client{
		http:  &http.Client{Timeout: httpTimeout},
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

	ui.waitUntil(ctx, fireAt)
	if ctx.Err() != nil {
		return report(ui, courses)
	}

	cl.warmUp(ui, fireAt)
	authFailed := fireWindow(ctx, ui, cl, courses)
	code := report(ui, courses)
	if authFailed {
		return 2
	}
	return code
}

// ---------------------------------------------------------------- scheduling

// fireWindow spends one request token at a time on the highest priority course
// that is off cooldown, until every course has landed or the run is stopped.
// fireWindow returns true when it stopped because the token was rejected.
func fireWindow(ctx context.Context, ui *ui, cl *client, courses []*course) bool {
	ui.rule("window open, firing")
	nextGlobal := time.Now()

	for ctx.Err() == nil {
		if allDone(courses) {
			return false
		}
		now := time.Now()

		var pick *course
		earliest := time.Time{}
		for _, c := range courses {
			if c.done {
				continue
			}
			if !c.next.After(now) {
				pick = c
				break // courses are already in priority order
			}
			if earliest.IsZero() || c.next.Before(earliest) {
				earliest = c.next
			}
		}

		if pick == nil {
			if !sleepCtx(ctx, time.Until(earliest)) {
				return false
			}
			continue
		}
		if wait := time.Until(nextGlobal); wait > 0 {
			if !sleepCtx(ctx, wait) {
				return false
			}
		}

		sent := time.Now()
		nextGlobal = sent.Add(globalGap)
		pick.attempts++
		resp, rtt, err := cl.add(pick)

		switch {
		case errors.Is(err, errRateLimited):
			pick.last = "429"
			ui.attempt(pick, "429 RATE LIMITED", fmt.Sprintf("backing off %s", rateLimitBackoff), rtt, kindWarn)
			nextGlobal = sent.Add(rateLimitBackoff)
			for _, c := range courses {
				if !c.done && c.next.Before(nextGlobal) {
					c.next = nextGlobal
				}
			}
		case errors.Is(err, errAuth):
			ui.fatal("token rejected or expired, log in again and rerun")
			return true
		case err != nil:
			pick.last = "error"
			pick.next = sent.Add(courseCooldown)
			ui.attempt(pick, "REQUEST FAILED", err.Error(), rtt, kindWarn)
		default:
			pick.next = sent.Add(courseCooldown)
			applyJobs(ui, courses, pick, resp, rtt)
			ui.setRemaining(resp.RemainingActions)
		}
	}
	return false
}

// applyJobs reads the newest result for the course just attempted, then
// harvests successes for any other course. jobs is newest first and
// accumulates across the whole session, so the first match is the latest.
func applyJobs(ui *ui, courses []*course, pick *course, resp *regResponse, rtt time.Duration) {
	seen := map[string]string{}
	for _, j := range resp.Jobs {
		if _, ok := seen[j.CourseID]; !ok && j.Result != "" {
			seen[j.CourseID] = j.Result
		}
	}

	res, ok := seen[pick.id]
	switch {
	case !ok:
		pick.last = "QUEUED"
		ui.attempt(pick, "QUEUED", "server has not judged it yet", rtt, kindWarn)
	case res == resultOK || res == resultDuplicate:
		land(ui, pick, res, rtt)
	default:
		pick.last = res
		note := fmt.Sprintf("retry at %s", pick.next.Format("15:04:05.000"))
		if why, bad := permanentFailures[res]; bad {
			note = why + ", will not succeed on retry"
		}
		ui.attempt(pick, res, note, rtt, kindBad)
	}

	// A response describes every job, so another course may have landed
	// without ever spending a request of its own. Adopt successes only,
	// since a stale failure must never stall a course still worth trying.
	for _, c := range courses {
		if c.done || c == pick {
			continue
		}
		if r, ok := seen[c.id]; ok && (r == resultOK || r == resultDuplicate) {
			land(ui, c, r, 0)
		}
	}
}

func land(ui *ui, c *course, res string, rtt time.Duration) {
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
	ui.attempt(c, res, note, rtt, kindGood)
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
		ui.warn("stopping, letting the request in flight finish. press Ctrl-C again to force.")
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
	http     *http.Client
	token    string
	tr       *transcript
	lastCall time.Time
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

	reused := false
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused },
	}))

	c.tr.write("REQUEST  %s", buf)
	start := time.Now()
	res, err := c.http.Do(req)
	rtt := time.Since(start)
	c.lastCall = time.Now()
	if err != nil {
		c.tr.write("ERROR    %v", err)
		return nil, rtt, err
	}
	defer res.Body.Close()

	raw, readErr := io.ReadAll(res.Body)
	c.tr.write("RESPONSE status=%d rtt=%s connReused=%t body=%s", res.StatusCode, rtt, reused, raw)
	if readErr != nil {
		return nil, rtt, readErr
	}
	if res.StatusCode == http.StatusTooManyRequests {
		return nil, rtt, errRateLimited
	}
	if len(raw) == 0 {
		return nil, rtt, fmt.Errorf("empty response body (status %d)", res.StatusCode)
	}
	if raw[0] != '{' {
		return nil, rtt, fmt.Errorf("non-JSON response (status %d): %.60s", res.StatusCode, raw)
	}

	var out regResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, rtt, err
	}
	// Auth failure arrives as HTTP 200 with an error field, never a 401.
	if out.Error == resultAuthError {
		return nil, rtt, errAuth
	}
	if out.Error != "" {
		return nil, rtt, fmt.Errorf("%s", out.Error)
	}
	return &out, rtt, nil
}

// warmUp opens a connection shortly before the window so the first real
// request does not pay a TLS handshake, which measured about 650ms. The
// GET spends a rate limit token, so it must not run inside the last second.
func (c *client) warmUp(ui *ui, fireAt time.Time) {
	if time.Since(c.lastCall) < warmupSkipIfNewerThan {
		ui.info("warm up  skipped, connection pooled %s ago", time.Since(c.lastCall).Truncate(time.Second))
		return
	}
	lead := time.Until(fireAt) - warmupLead
	if lead > 0 {
		time.Sleep(lead)
	}
	req, err := http.NewRequest(http.MethodGet, portalOrigin+"/", nil)
	if err != nil {
		return
	}
	start := time.Now()
	res, err := c.http.Do(req)
	c.lastCall = time.Now()
	if err != nil {
		ui.warn("warm up failed: %v", err)
		return
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	ui.info("warm up  connection opened in %dms, %s before the window",
		time.Since(start).Milliseconds(), time.Until(fireAt).Truncate(time.Millisecond))
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

// fireTime keeps the formula the original script has always used. The margin
// is deliberately biased late: arriving early is rejected and costs a full
// cooldown, while arriving late costs only the delay itself.
func (s *clockSync) fireTime(target time.Time) time.Time {
	delay := target.Sub(s.server) + s.rtt + 100*time.Millisecond
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
		out = append(out, &course{id: id, units: units, title: entry.Title})
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
		ui.plain("     %d registration actions remaining", s.remaining)
	}
	ui.plain("  window opens  %s", target.Format("2006-01-02 15:04:05.000"))
	ui.plain("  firing at     %s (in %s)", fireAt.Format("15:04:05.000"), time.Until(fireAt).Truncate(time.Millisecond))
	ui.plain("  pacing        one request every %s, %s per course", globalGap, courseCooldown)
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
			ui.good("  registered   %-9s %-30s at %s after %d attempt(s)",
				c.id, trimTitle(c.title, 30), c.landed.Format("15:04:05.000"), c.attempts)
			continue
		}
		outstanding++
		last := c.last
		if last == "" {
			last = "no attempt"
		}
		ui.bad("  outstanding  %-9s %-30s %d attempt(s), last %s",
			c.id, trimTitle(c.title, 30), c.attempts, last)
	}
	if outstanding == 0 {
		ui.good("  everything landed")
		return 0
	}
	return 1
}

// ---------------------------------------------------------------- transcript

type transcript struct {
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
	fmt.Fprintf(t.f, "%s %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}

func (t *transcript) Close() error {
	if t == nil || t.f == nil {
		return nil
	}
	t.write("SESSION  ended")
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
	u.emit(u.paint(cBold, "my.edu.sharif.edu sniper"))
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
	if n != 0 && n != u.remaining {
		if u.remaining != 0 {
			u.info("registration actions remaining: %d", n)
		}
		u.remaining = n
	}
}

// attempt prints one attempt line:
//
//	16:00:00.412  #1  22034-2  OK  -> registered in 412ms
func (u *ui) attempt(c *course, result, note string, rtt time.Duration, k kind) {
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
		u.paint(cDim, fmt.Sprintf("#%d", c.attempts)),
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
	u.plain("  fire = received + (window - serverClock) + roundTrip + 100ms")
	u.plain("       = %s + %s + %s + 100ms",
		s.recv.Format("15:04:05.000"), gap.Truncate(time.Millisecond), s.rtt.Truncate(time.Millisecond))
	u.plain("       = %s", u.paint(cBold, fireAt.Format("15:04:05.000")))
	u.plain("  %s", u.paint(cDim, "the margin is biased late on purpose: arriving early is rejected"))
	u.plain("  %s", u.paint(cDim, "and costs a full cooldown, arriving late costs only the delay"))
}

// waitUntil logs a heartbeat that tightens as the window approaches, then
// counts down the final minute one line per second.
func (u *ui) waitUntil(ctx context.Context, fireAt time.Time) {
	d := time.Until(fireAt)
	if d <= 0 {
		u.warn("the window opened %s ago, firing immediately", (-d).Truncate(time.Second))
		return
	}
	u.rule("waiting")
	u.info("the sniper will fire in %s, at %s", d.Truncate(time.Second), fireAt.Format("15:04:05.000"))
	u.info("press Ctrl-C to abort")

	for {
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
