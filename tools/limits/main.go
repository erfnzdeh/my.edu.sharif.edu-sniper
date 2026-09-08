// Command limits measures what the portal's edge actually rejects.
//
// The sniper's pacing rests on a claim that has never been tested properly:
// that /api/reg allows one request per second. Two live transcripts contradict
// it. On 2026-09-08 a request sent 1.101s after the previous one was rejected
// while one sent 1.102s after passed, and a request sent 5.077s after the
// previous one was rejected anyway. The tables the constants came from do not
// settle it either: 1.2s and 1.5s both passed 10 of 12, and the 12 of 14
// against 14 of 14 that moved globalGap to 1.3s is a two-sided Fisher exact
// p of 0.48, which is noise.
//
// This tool answers three questions the transcripts cannot.
//
//  1. Does the gap between requests change the rejection rate at all, or is
//     rejection independent of how you space them?
//  2. Is there a burst allowance? The portal negotiates HTTP/2, so every
//     request multiplexes onto one connection, and whether k at once are
//     accepted matters far more to a sniper than spacing does.
//  3. Is rejection driven by your own pattern or by load on the portal? Run
//     the same schedule at a quiet hour and again near a busy one and compare.
//
// # Why this is safe to run
//
// The 429 body is nginx's own HTML, so rejection happens at the edge, before
// Express and therefore before authentication. That means the limiter can be
// measured with a deliberately invalid token: the request never establishes a
// student id, so no job is queued, no registration action is spent, and
// BLOCKED, which is keyed on the student id, cannot be tripped. The course id
// is one that cannot exist, so even a misconfigured run has nothing to act on.
//
// That the edge behaves identically for an invalid token is the one assumption
// this rests on. Check it with -verify-with-token once, which sends a small
// paired sample with a real token and compares the two rejection rates.
//
// The tool refuses to run near a registration window, holds a ceiling on total
// requests, and stops immediately if the portal ever answers BLOCKED or
// TOO_MANY_REQUESTS.
//
// # Use
//
//	limits -mode spacing -n 40 -out spacing.csv
//	limits -mode burst -sizes 1,2,3,5,8 -reps 8 -out burst.csv
//
// Read the summary, then read the CSV. Two rates differ only if their
// intervals do not overlap. On a shared campus NAT you are measuring a budget
// you share with everyone else on it, which is worth recording alongside.
package main

import (
	"bytes"
	gocontext "context"
	"crypto/tls"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultTarget = "https://my.edu.sharif.edu/api/reg"
	// probeCourse cannot exist, so no run of this tool can register anything
	// even if a real token is handed to it by mistake.
	probeCourse = "00000-0"
	// invalidToken is syntactically a JWT and verifies against nothing.
	invalidToken = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdHVkZW50SWQiOiJub3QtYS1zdHVkZW50In0.not-a-signature"
	// hardCeiling is the most requests any single run may send, whatever the
	// flags say. A measurement is not worth more traffic than this.
	hardCeiling = 1200
)

// windows are the registration hours the sniper knows about. Measuring near
// one risks both a useless number, since the portal is under load, and a
// rate limit state that follows you into the window itself.
var windows = []string{"08:00", "16:00"}

const (
	windowLeadOut  = 90 * time.Minute
	windowTrailing = 30 * time.Minute
)

type trial struct {
	mode      string
	seq       int
	wantGap   time.Duration // the gap this trial was scheduled with
	burstSize int           // burst mode only
	sentAt    time.Time
	sinceRel  time.Duration // from the start of the run
	realGap   time.Duration // measured gap since the previous send
	status    int
	rtt       time.Duration
	ttfb      time.Duration
	proto     string
	reused    bool
	code      string // portal error code when the body carried one
	retryHdr  string
	rlRemain  string
	err       string
}

func (t trial) rejected() bool { return t.status == http.StatusTooManyRequests }

func main() { os.Exit(run()) }

func run() int {
	var (
		fURL       = flag.String("url", defaultTarget, "endpoint to probe")
		fMode      = flag.String("mode", "spacing", "spacing or burst")
		fIntervals = flag.String("intervals", "0.2,0.5,0.8,1.1,1.4,2.0,3.0", "spacing mode: gaps in seconds")
		fN         = flag.Int("n", 40, "spacing mode: samples per gap")
		fSizes     = flag.String("sizes", "1,2,3,5,8", "burst mode: how many at once")
		fReps      = flag.Int("reps", 8, "burst mode: bursts per size")
		fCool      = flag.Duration("cool", 15*time.Second, "burst mode: quiet time between bursts")
		fOut       = flag.String("out", "", "write the trials to this CSV")
		fToken     = flag.String("verify-with-token", "", "also run a small paired sample with this real token, to check the edge treats both alike")
		fYes       = flag.Bool("y", false, "skip the confirmation prompt")
		fForce     = flag.Bool("force", false, "run even near a registration window")
	)
	flag.Parse()

	if why, near := nearWindow(time.Now()); near && !*fForce {
		fmt.Fprintf(os.Stderr, "refusing to run: %s\n", why)
		fmt.Fprintf(os.Stderr, "measuring under window load tells you about the load, not the limiter.\n")
		fmt.Fprintf(os.Stderr, "pass -force if you really mean it.\n")
		return 2
	}

	plan, err := buildPlan(*fMode, *fIntervals, *fN, *fSizes, *fReps, *fCool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	if plan.requests > hardCeiling {
		fmt.Fprintf(os.Stderr, "refusing to send %d requests, the ceiling is %d. lower -n or -reps.\n",
			plan.requests, hardCeiling)
		return 2
	}

	fmt.Printf("target       %s\n", *fURL)
	fmt.Printf("mode         %s\n", plan.mode)
	fmt.Printf("requests     %d\n", plan.requests)
	fmt.Printf("estimated    %s\n", plan.duration.Round(time.Second))
	fmt.Printf("token        invalid on purpose, so nothing is queued against your account\n")
	fmt.Printf("course       %s, which cannot exist\n", probeCourse)
	if !*fYes && !confirm() {
		return 2
	}

	ctx, stop := signal.NotifyContext(gocontext.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	p := &prober{url: *fURL, client: newClient()}
	p.warm()

	var trials []trial
	switch plan.mode {
	case "spacing":
		trials = p.spacing(ctx, plan)
	case "burst":
		trials = p.burst(ctx, plan)
	}

	if *fToken != "" {
		fmt.Printf("\n== paired check against a real token ===========================\n")
		fmt.Printf("  this one does reach Express, so it queues jobs for %s.\n", probeCourse)
		trials = append(trials, p.paired(ctx, *fToken)...)
	}

	if *fOut != "" {
		if err := writeCSV(*fOut, trials); err != nil {
			fmt.Fprintf(os.Stderr, "csv: %v\n", err)
		} else {
			fmt.Printf("\nwrote %d trials to %s\n", len(trials), *fOut)
		}
	}
	report(plan.mode, trials)
	return 0
}

// ------------------------------------------------------------------- planning

type plan struct {
	mode     string
	gaps     []time.Duration // spacing mode, already shuffled
	sizes    []int           // burst mode
	reps     int
	cool     time.Duration
	requests int
	duration time.Duration
}

// buildPlan lays out the trials. In spacing mode the gaps are shuffled rather
// than run in blocks, so that drift in portal load over the run cannot line up
// with any one gap and masquerade as an effect of it. Each response is
// attributed to the gap that preceded it.
func buildPlan(mode, intervals string, n int, sizes string, reps int, cool time.Duration) (plan, error) {
	switch mode {
	case "spacing":
		vals, err := parseSeconds(intervals)
		if err != nil {
			return plan{}, err
		}
		if n < 1 {
			return plan{}, errors.New("-n must be at least 1")
		}
		var gaps []time.Duration
		var total time.Duration
		for _, v := range vals {
			for i := 0; i < n; i++ {
				gaps = append(gaps, v)
				total += v
			}
		}
		rand.Shuffle(len(gaps), func(i, j int) { gaps[i], gaps[j] = gaps[j], gaps[i] })
		return plan{mode: mode, gaps: gaps, requests: len(gaps), duration: total}, nil
	case "burst":
		vals, err := parseInts(sizes)
		if err != nil {
			return plan{}, err
		}
		if reps < 1 {
			return plan{}, errors.New("-reps must be at least 1")
		}
		count := 0
		for _, k := range vals {
			count += k * reps
		}
		return plan{
			mode: mode, sizes: vals, reps: reps, cool: cool,
			requests: count,
			duration: time.Duration(len(vals)*reps) * cool,
		}, nil
	}
	return plan{}, fmt.Errorf("unknown mode %q, want spacing or burst", mode)
}

func parseSeconds(s string) ([]time.Duration, error) {
	var out []time.Duration
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		v, err := strconv.ParseFloat(f, 64)
		if err != nil || v < 0 {
			return nil, fmt.Errorf("cannot read %q as seconds", f)
		}
		out = append(out, time.Duration(v*float64(time.Second)))
	}
	if len(out) == 0 {
		return nil, errors.New("no intervals given")
	}
	return out, nil
}

func parseInts(s string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		v, err := strconv.Atoi(f)
		if err != nil || v < 1 {
			return nil, fmt.Errorf("cannot read %q as a burst size", f)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, errors.New("no sizes given")
	}
	return out, nil
}

// nearWindow reports whether now is close enough to a registration window that
// a measurement would be about the load rather than about the limiter.
func nearWindow(now time.Time) (string, bool) {
	for _, w := range windows {
		var h, m int
		fmt.Sscanf(w, "%d:%d", &h, &m)
		at := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
		if now.After(at.Add(-windowLeadOut)) && now.Before(at.Add(windowTrailing)) {
			return fmt.Sprintf("it is %s, within %s of the %s window",
				now.Format("15:04"), windowLeadOut, w), true
		}
	}
	return "", false
}

func confirm() bool {
	fmt.Print("\nstart? [y/N]: ")
	var line string
	fmt.Scanln(&line)
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// -------------------------------------------------------------------- probing

type prober struct {
	url    string
	client *http.Client
	seq    int
	last   time.Time
	start  time.Time
	halted string // set when the portal told us to stop
}

// newClient shares one transport so HTTP/2 multiplexing is exercised the same
// way the sniper exercises it. Without that, a burst would measure connection
// setup rather than the limiter.
func newClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = true
	tr.MaxIdleConns = 64
	tr.MaxIdleConnsPerHost = 64
	return &http.Client{Transport: tr, Timeout: 15 * time.Second}
}

// warm opens the connection and then waits, so the first measured trial is not
// paying a TLS handshake and the warm up's own request has left the counter.
func (p *prober) warm() {
	req, err := http.NewRequest(http.MethodGet, origin(p.url)+"/", nil)
	if err != nil {
		return
	}
	start := time.Now()
	res, err := p.client.Do(req)
	if err != nil {
		fmt.Printf("warm up failed: %v\n", err)
		return
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	fmt.Printf("\nwarm up      %s in %dms, settling for 5s so it leaves the counter\n",
		res.Proto, time.Since(start).Milliseconds())
	time.Sleep(5 * time.Second)
	p.start = time.Now()
}

func origin(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		if j := strings.Index(u[i+3:], "/"); j >= 0 {
			return u[:i+3+j]
		}
	}
	return u
}

// spacing walks the shuffled schedule, sleeping the scheduled gap before each
// request and recording the gap actually achieved alongside the outcome.
func (p *prober) spacing(ctx ctxt, pl plan) []trial {
	fmt.Printf("\n== spacing =====================================================\n")
	out := make([]trial, 0, len(pl.gaps))
	for i, gap := range pl.gaps {
		if ctx.Err() != nil || p.halted != "" {
			break
		}
		if i > 0 {
			sleepUntil(ctx, p.last.Add(gap))
		}
		t := p.probe(invalidToken)
		t.mode, t.wantGap = "spacing", gap
		out = append(out, t)
		p.progress(len(out), pl.requests, t)
	}
	return out
}

// burst fires k requests at the same instant and counts how many the edge
// takes, which is the question a spacing test cannot answer.
func (p *prober) burst(ctx ctxt, pl plan) []trial {
	fmt.Printf("\n== burst =======================================================\n")
	var out []trial
	done := 0
	for _, k := range pl.sizes {
		for r := 0; r < pl.reps; r++ {
			if ctx.Err() != nil || p.halted != "" {
				return out
			}
			if done > 0 {
				sleepUntil(ctx, time.Now().Add(pl.cool))
			}
			var (
				wg    sync.WaitGroup
				mu    sync.Mutex
				batch []trial
				gate  = make(chan struct{})
			)
			for j := 0; j < k; j++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-gate // release them together
					t := p.probe(invalidToken)
					t.mode, t.burstSize = "burst", k
					mu.Lock()
					batch = append(batch, t)
					mu.Unlock()
				}()
			}
			close(gate)
			wg.Wait()

			passed := 0
			for _, t := range batch {
				if !t.rejected() && t.err == "" {
					passed++
				}
			}
			out = append(out, batch...)
			done++
			fmt.Printf("  burst of %-2d  %d accepted, %d rejected\n", k, passed, k-passed)
		}
	}
	return out
}

// paired alternates an invalid token with a real one at the same spacing, to
// check the assumption the whole method rests on: that the edge rejects both
// the same way, because it rejects before Express ever reads the token.
func (p *prober) paired(ctx ctxt, token string) []trial {
	var out []trial
	for i := 0; i < 24; i++ {
		if ctx.Err() != nil || p.halted != "" {
			break
		}
		sleepUntil(ctx, p.last.Add(600*time.Millisecond))
		tok, label := invalidToken, "paired-invalid"
		if i%2 == 1 {
			tok, label = token, "paired-real"
		}
		t := p.probe(tok)
		t.mode, t.wantGap = label, 600*time.Millisecond
		out = append(out, t)
	}
	inv, real_ := split(out, "paired-invalid")
	li, hi := wilson(rejects(inv), len(inv))
	lr, hr := wilson(rejects(real_), len(real_))
	fmt.Printf("  invalid token  %d of %d rejected, 95%% interval %.0f%% to %.0f%%\n",
		rejects(inv), len(inv), li*100, hi*100)
	fmt.Printf("  real token     %d of %d rejected, 95%% interval %.0f%% to %.0f%%\n",
		rejects(real_), len(real_), lr*100, hr*100)
	if overlap(li, hi, lr, hr) {
		fmt.Printf("  the intervals overlap, so the invalid token is a fair stand in\n")
	} else {
		fmt.Printf("  THE INTERVALS DO NOT OVERLAP. the edge treats them differently,\n")
		fmt.Printf("  so the main results are not safe to read as being about a real client.\n")
	}
	return out
}

func (p *prober) probe(token string) trial {
	p.seq++
	t := trial{seq: p.seq}

	body := fmt.Sprintf(`{"action":"add","course":%q,"units":0}`, probeCourse)
	req, err := http.NewRequest(http.MethodPost, p.url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.err = err.Error()
		return t
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin(p.url))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36")

	var start time.Time
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn:              func(i httptrace.GotConnInfo) { t.reused = i.Reused },
		GotFirstResponseByte: func() { t.ttfb = time.Since(start) },
		TLSHandshakeDone:     func(tls.ConnectionState, error) {},
	}))

	start = time.Now()
	t.sentAt = start
	if !p.start.IsZero() {
		t.sinceRel = start.Sub(p.start)
	}
	if !p.last.IsZero() {
		t.realGap = start.Sub(p.last)
	}
	p.last = start

	res, err := p.client.Do(req)
	t.rtt = time.Since(start)
	if err != nil {
		t.err = err.Error()
		return t
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))

	t.status = res.StatusCode
	t.proto = res.Proto
	t.retryHdr = res.Header.Get("Retry-After")
	t.rlRemain = res.Header.Get("X-RateLimit-Remaining")
	t.code = codeOf(raw)

	// The portal telling us to stop is the one result worth aborting on.
	if t.code == "BLOCKED" || t.code == "TOO_MANY_REQUESTS" {
		p.halted = t.code
		fmt.Printf("\nSTOPPING: the portal answered %s. that is the application level\n", t.code)
		fmt.Printf("guard, not the edge. no further requests will be sent.\n")
	}
	return t
}

// codeOf pulls the portal's error code out of a body, whether it arrived as an
// object with an error field or as the bare quoted string the portal also uses.
func codeOf(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s[0] == '<' {
		return ""
	}
	if i := strings.Index(s, `"error":"`); i >= 0 {
		rest := s[i+9:]
		if j := strings.IndexByte(rest, '"'); j >= 0 {
			return rest[:j]
		}
	}
	if s[0] == '"' {
		s = strings.Trim(s, `"`)
		if i := strings.IndexByte(s, ' '); i >= 0 {
			return s[:i]
		}
		return s
	}
	return ""
}

func (p *prober) progress(done, total int, t trial) {
	if done%10 != 0 && !t.rejected() && t.err == "" {
		return
	}
	what := strconv.Itoa(t.status)
	if t.err != "" {
		what = "error"
	}
	if t.rejected() {
		what = "429 REJECTED"
	}
	fmt.Printf("  %3d/%d  gap %5.2fs  %s\n", done, total, t.realGap.Seconds(), what)
}

// ------------------------------------------------------------------ reporting

func report(mode string, trials []trial) {
	if len(trials) == 0 {
		fmt.Println("\nno trials completed")
		return
	}
	fmt.Printf("\n== result ======================================================\n")

	switch mode {
	case "spacing":
		reportSpacing(trials)
	case "burst":
		reportBurst(trials)
	}
	reportOverTime(trials)
}

func reportSpacing(trials []trial) {
	byGap := map[time.Duration][]trial{}
	for _, t := range trials {
		if t.mode != "spacing" {
			continue
		}
		byGap[t.wantGap] = append(byGap[t.wantGap], t)
	}
	gaps := make([]time.Duration, 0, len(byGap))
	for g := range byGap {
		gaps = append(gaps, g)
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })

	fmt.Printf("  gap      n    rejected   rate    95%% interval\n")
	var los, his []float64
	for _, g := range gaps {
		ts := byGap[g]
		k := rejects(ts)
		lo, hi := wilson(k, len(ts))
		los, his = append(los, lo), append(his, hi)
		fmt.Printf("  %5.2fs  %-4d %-10d %5.1f%%  %4.1f%% to %4.1f%%\n",
			g.Seconds(), len(ts), k, float64(k)/float64(len(ts))*100, lo*100, hi*100)
	}

	// The question is whether any two gaps differ by more than noise.
	separated := false
	for i := range gaps {
		for j := i + 1; j < len(gaps); j++ {
			if !overlap(los[i], his[i], los[j], his[j]) {
				separated = true
				fmt.Printf("\n  %.2fs and %.2fs do not overlap: spacing changes the outcome there.\n",
					gaps[i].Seconds(), gaps[j].Seconds())
			}
		}
	}
	if !separated {
		fmt.Printf("\n  every interval overlaps every other one. at this sample size the\n")
		fmt.Printf("  data does not show spacing affecting rejection at all. either the\n")
		fmt.Printf("  effect is smaller than %d samples per gap can see, or it is not there.\n",
			len(byGap[gaps[0]]))
	}
}

func reportBurst(trials []trial) {
	bySize := map[int][]trial{}
	for _, t := range trials {
		if t.mode != "burst" {
			continue
		}
		bySize[t.burstSize] = append(bySize[t.burstSize], t)
	}
	sizes := make([]int, 0, len(bySize))
	for k := range bySize {
		sizes = append(sizes, k)
	}
	sort.Ints(sizes)

	fmt.Printf("  at once   requests   rejected   rate    95%% interval\n")
	for _, k := range sizes {
		ts := bySize[k]
		r := rejects(ts)
		lo, hi := wilson(r, len(ts))
		fmt.Printf("  %-9d %-10d %-10d %5.1f%%  %4.1f%% to %4.1f%%\n",
			k, len(ts), r, float64(r)/float64(len(ts))*100, lo*100, hi*100)
	}
	fmt.Printf("\n  a burst allowance shows up as a flat rejection rate that only climbs\n")
	fmt.Printf("  past some size. a strict one-at-a-time limiter rejects everything\n")
	fmt.Printf("  beyond the first of every burst, so the rate would track 1 - 1/k.\n")
}

// reportOverTime is the load-shedding check. If rejections cluster in time
// rather than spreading across the run, the cause is the portal's state, not
// the pattern, and no amount of spacing will help.
func reportOverTime(trials []trial) {
	var first, last time.Duration
	for _, t := range trials {
		if t.sinceRel > last {
			last = t.sinceRel
		}
	}
	if last == 0 {
		return
	}
	const buckets = 10
	width := last / buckets
	if width == 0 {
		return
	}
	counts := make([]int, buckets+1)
	totals := make([]int, buckets+1)
	for _, t := range trials {
		b := int(t.sinceRel / width)
		if b > buckets {
			b = buckets
		}
		totals[b]++
		if t.rejected() {
			counts[b]++
		}
	}
	fmt.Printf("\n  rejections over the run, %s per bucket:\n", width.Round(time.Second))
	any := false
	for i := 0; i <= buckets; i++ {
		if totals[i] == 0 {
			continue
		}
		bar := strings.Repeat("#", counts[i])
		if counts[i] > 0 {
			any = true
		}
		fmt.Printf("    +%-6s %2d/%-3d %s\n",
			(time.Duration(i) * width).Round(time.Second), counts[i], totals[i], bar)
	}
	_ = first
	if !any {
		fmt.Printf("    none. the edge did not reject a single request in this run.\n")
	}
}

func rejects(ts []trial) int {
	n := 0
	for _, t := range ts {
		if t.rejected() {
			n++
		}
	}
	return n
}

func split(ts []trial, mode string) (match, other []trial) {
	for _, t := range ts {
		if t.mode == mode {
			match = append(match, t)
		} else {
			other = append(other, t)
		}
	}
	return
}

// wilson is the score interval for a binomial proportion. It is used rather
// than the normal approximation because the interesting cases here are zero
// rejections out of n, where the normal interval collapses to a point and
// invites exactly the overconfidence that produced the 1.3s gap.
func wilson(k, n int) (float64, float64) {
	if n == 0 {
		return 0, 1
	}
	const z = 1.959963985
	p := float64(k) / float64(n)
	fn := float64(n)
	den := 1 + z*z/fn
	centre := (p + z*z/(2*fn)) / den
	margin := z * math.Sqrt(p*(1-p)/fn+z*z/(4*fn*fn)) / den
	lo, hi := centre-margin, centre+margin
	if lo < 0 {
		lo = 0
	}
	if hi > 1 {
		hi = 1
	}
	return lo, hi
}

func overlap(lo1, hi1, lo2, hi2 float64) bool { return lo1 <= hi2 && lo2 <= hi1 }

func writeCSV(path string, trials []trial) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{
		"seq", "mode", "sent_at", "since_start_s", "scheduled_gap_s", "actual_gap_s",
		"burst_size", "status", "rejected", "rtt_ms", "ttfb_ms", "proto", "conn_reused",
		"portal_code", "retry_after", "ratelimit_remaining", "error",
	}); err != nil {
		return err
	}
	for _, t := range trials {
		if err := w.Write([]string{
			strconv.Itoa(t.seq), t.mode, t.sentAt.Format(time.RFC3339Nano),
			fmt.Sprintf("%.3f", t.sinceRel.Seconds()),
			fmt.Sprintf("%.3f", t.wantGap.Seconds()),
			fmt.Sprintf("%.3f", t.realGap.Seconds()),
			strconv.Itoa(t.burstSize), strconv.Itoa(t.status),
			strconv.FormatBool(t.rejected()),
			strconv.FormatInt(t.rtt.Milliseconds(), 10),
			strconv.FormatInt(t.ttfb.Milliseconds(), 10),
			t.proto, strconv.FormatBool(t.reused),
			t.code, t.retryHdr, t.rlRemain, t.err,
		}); err != nil {
			return err
		}
	}
	return nil
}

type ctxt = gocontext.Context

func sleepUntil(ctx ctxt, at time.Time) {
	d := time.Until(at)
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
