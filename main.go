package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type VahedRequest struct {
	Action string `json:"action"`
	Course string `json:"course"`
	Units  int32  `json:"units"`
}

type VahedResponse struct {
	Error             string              `json:"error"`
	RemainingActions  int                 `json:"remainingActions"`
	Jobs              []*VahedJobResponse `json:"jobs"`
	RegisterationTime int64               `json:"registrationTime"`
	Time              int64               `json:"time"`
}

type VahedJobResponse struct {
	ID     string `json:"courseId"`
	Result string `json:"result"`
}

// jobResult carries one course outcome back to main, so the shared course
// list is only ever mutated from a single goroutine.
type jobResult struct {
	course      string
	registered  bool
	rateLimited bool
}

const EduUrl = "https://my.edu.sharif.edu/api/reg"
const AuthToken = "" // take from headers after login.

// Result codes returned by the portal. Verified against the frontend bundle.
const (
	ResultOK              = "OK"
	ResultDuplicate       = "COURSE_DUPLICATE"
	ResultInvalidCourse   = "INVALID_COURSE"
	ResultRepeatedRequest = "REPEATED_REQUEST"
	ErrAuthorization      = "AUTHORIZATION"
)

var wg sync.WaitGroup
var mu sync.Mutex
var vaheds = []*VahedRequest{
	{
		Action: "add",
		Course: "22034-2", // [CODE]-[GROUP]
		Units:  3,
	},
	{
		Action: "add",
		Course: "40441-1",
		Units:  3,
	},
} // fill with your courses in the above format.

func main() {
	client := &http.Client{Timeout: 10 * time.Second}
	delay, err := findTimeDiff(client)
	if err != nil {
		fmt.Println(err)
		return
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	for len(vaheds) > 0 {
		results := make(chan jobResult, len(vaheds))
		for _, vahed := range vaheds {
			wg.Add(1)
			go reqToEdu(client, vahed, results)
		}
		wg.Wait()
		close(results)

		waitCount := 5 * time.Second
		for r := range results {
			if r.rateLimited {
				waitCount = 7 * time.Second
			}
			if r.registered {
				for j := len(vaheds) - 1; j >= 0; j-- {
					if vaheds[j].Course == r.course {
						vaheds = append(vaheds[:j], vaheds[j+1:]...)
					}
				}
			}
		}
		if len(vaheds) == 0 {
			break
		}
		time.Sleep(waitCount)
	}
}

func findTimeDiff(client *http.Client) (time.Duration, error) {
	req := initRequest(vaheds[0])
	time_start := time.Now()
	res, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	time_end := time.Now()
	resp, err := parseResponse(res)
	if err != nil {
		return 0, err
	}
	server_time := time.Unix(resp.Time/1000, (resp.Time%1000)*1000000)
	time_diff := time_end.Sub(time_start)
	register_time := time.Unix(resp.RegisterationTime/1000, (resp.RegisterationTime%1000)*1000000)
	if server_time.After(register_time.Add(time.Hour)) {
		register_time = time.Date(server_time.Year(), server_time.Month(), server_time.Day(), 16, 0, 0, 0, server_time.Location())
	}
	delay := register_time.Sub(server_time) + time_diff + time.Millisecond*100 // 100 ms for net lag
	if delay > 0 {
		fmt.Println("Wait Time Until Start", delay)
	}
	return delay, nil
}

func reqToEdu(client *http.Client, request *VahedRequest, results chan<- jobResult) {
	defer wg.Done()
	out := jobResult{course: request.Course}
	req := initRequest(request)

	mu.Lock() // remove if requests are slowed by the server. (currently it is.)
	res, err := client.Do(req)
	mu.Unlock() // must not be deferred past this point, but must always run.

	if err != nil {
		fmt.Println(request.Course, err)
		results <- out
		return
	}
	resp, err := parseResponse(res)
	if err != nil {
		fmt.Println(request.Course, err)
		if err.Error() == "TOO_MANY_REQUESTS" {
			out.rateLimited = true
		}
		results <- out
		return
	}
	// jobs is newest first and accumulates across requests, so scan forward
	// and stop at the first entry for this course to read its latest result.
	for _, job := range resp.Jobs {
		if job.ID != request.Course {
			continue
		}
		if job.Result == "" {
			fmt.Println(job.ID, "QUEUED")
			break
		}
		fmt.Println(job.ID, job.Result)
		if job.Result == ResultOK || job.Result == ResultDuplicate {
			out.registered = true
		}
		break
	}
	results <- out
}

func initRequest(request *VahedRequest) *http.Request {
	payloadBuf := new(bytes.Buffer)
	json.NewEncoder(payloadBuf).Encode(request)
	req, _ := http.NewRequest("POST", EduUrl, payloadBuf)
	req.Header.Set("Authorization", AuthToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://my.edu.sharif.edu")
	req.Header.Set("Referer", "https://my.edu.sharif.edu/courses/offered")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:104.0) Gecko/20100101 Firefox/104.0") // change to your own browser agent if you like.
	return req
}

func parseResponse(res *http.Response) (*VahedResponse, error) {
	defer res.Body.Close()

	// The edge rate limiter answers with a real 429 and an HTML body.
	if res.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("TOO_MANY_REQUESTS")
	}

	responseBuf := new(bytes.Buffer)
	if _, err := io.Copy(responseBuf, res.Body); err != nil {
		return nil, err
	}
	body := responseBuf.Bytes()
	if len(body) == 0 {
		return nil, fmt.Errorf("empty response body (status %d)", res.StatusCode)
	}
	if body[0] != '{' {
		return nil, fmt.Errorf("non-JSON response (status %d): %.60s", res.StatusCode, body)
	}

	var resp VahedResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	// Auth failures arrive as HTTP 200 with an error field, never as a 401.
	if resp.Error != "" {
		if resp.Error == ErrAuthorization {
			return nil, fmt.Errorf("AUTHORIZATION: token rejected or expired, log in again")
		}
		return nil, fmt.Errorf("%s", resp.Error)
	}
	return &resp, nil
}
