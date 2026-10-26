package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// gcdeck reads GitHub through its REST API rather than one `gh` process per question. Every response is kept with its
// ETag and asked for again with If-None-Match: GitHub answers 304 when nothing changed, and a 304 does not count against
// the hourly rate limit, so polling a quiet repo costs next to nothing. Questions asked at the same moment (the
// dashboard and the pipeline both want the deploy runs) share one request. When the limit is reached anyway, gcdeck
// stops asking until GitHub says it resets, instead of failing every few seconds and making it worse. The token is
// gh's own (`gh auth token`) and is only ever sent to the API host.

const (
	ghFresh      = 3 * time.Second  // an answer this new is reused without asking again
	ghLowFresh   = time.Minute      // the same, while the hourly budget is running low
	ghLowBudget  = 300              // requests left that count as running low
	ghMaxBody    = 16 << 20         // the largest response read
	ghMaxEntries = 400              // responses kept for revalidation
	ghTimeout    = 20 * time.Second // one request
)

type ghEntry struct {
	etag string
	body []byte
	at   time.Time
}

type ghFlight struct {
	done chan struct{}
	body []byte
	err  error
}

type ghClient struct {
	base string
	http *http.Client

	mu        sync.Mutex
	token     string
	cache     map[string]*ghEntry
	inflight  map[string]*ghFlight
	limit     int
	remaining int
	reset     time.Time
	paused    time.Time // no requests before this
}

var gh = newGHClient()

func newGHClient() *ghClient {
	base := "https://api.github.com"
	if h := os.Getenv("GH_HOST"); h != "" && h != "github.com" {
		base = "https://" + h + "/api/v3"
	}
	return &ghClient{
		base:      base,
		http:      &http.Client{Timeout: ghTimeout},
		cache:     map[string]*ghEntry{},
		inflight:  map[string]*ghFlight{},
		remaining: -1,
	}
}

// rateLimitError says GitHub's limit was reached and when gcdeck will ask again.
type rateLimitError struct{ until time.Time }

func (e rateLimitError) Error() string {
	return "GitHub rate limit reached; gcdeck pauses GitHub until " + e.until.Local().Format("15:04")
}

// ghGet fetches an API path (no leading slash) and decodes it into v.
func ghGet(path string, v any) error {
	body, err := gh.get(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func (c *ghClient) get(path string) ([]byte, error) {
	c.mu.Lock()
	e := c.cache[path]
	if e != nil {
		fresh := ghFresh
		if c.remaining >= 0 && c.remaining < ghLowBudget {
			fresh = ghLowFresh
		}
		if time.Since(e.at) < fresh {
			c.mu.Unlock()
			return e.body, nil
		}
	}
	if time.Now().Before(c.paused) {
		until := c.paused
		c.mu.Unlock()
		return nil, rateLimitError{until}
	}
	if f := c.inflight[path]; f != nil {
		c.mu.Unlock()
		<-f.done
		return f.body, f.err
	}
	f := &ghFlight{done: make(chan struct{})}
	c.inflight[path] = f
	c.mu.Unlock()

	f.body, f.err = c.fetch(path, e)

	c.mu.Lock()
	delete(c.inflight, path)
	c.mu.Unlock()
	close(f.done)
	return f.body, f.err
}

func (c *ghClient) fetch(path string, cached *ghEntry) ([]byte, error) {
	token, err := c.authToken(false)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(path, token, cached)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		// The token may have been refreshed by `gh auth login` since gcdeck read it.
		resp.Body.Close()
		if token, err = c.authToken(true); err != nil {
			return nil, err
		}
		resp, err = c.do(path, token, cached)
	}
	if err != nil {
		return nil, fmt.Errorf("GitHub: %s", errText(err))
	}
	defer resp.Body.Close()
	c.noteLimits(resp)

	switch {
	case resp.StatusCode == http.StatusNotModified && cached != nil:
		c.mu.Lock()
		cached.at = time.Now()
		c.mu.Unlock()
		return cached.body, nil
	case resp.StatusCode == http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, ghMaxBody))
		if err != nil {
			return nil, fmt.Errorf("GitHub: %s", errText(err))
		}
		c.store(path, resp.Header.Get("ETag"), body)
		return body, nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var apiErr struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &apiErr)
	msg := clean(apiErr.Message)
	if until, limited := c.limitedUntil(resp, msg); limited {
		return nil, rateLimitError{until}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("gh: %w", errAuth)
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return nil, fmt.Errorf("GitHub: %d %s", resp.StatusCode, msg)
}

func (c *ghClient) do(path, token string, cached *ghEntry) (*http.Response, error) {
	req, err := http.NewRequest("GET", c.base+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gcdeck")
	if cached != nil && cached.etag != "" {
		req.Header.Set("If-None-Match", cached.etag)
	}
	return c.http.Do(req)
}

// authToken asks gh for its token once, and again when GitHub refuses it.
func (c *ghClient) authToken(renew bool) (string, error) {
	c.mu.Lock()
	t := c.token
	c.mu.Unlock()
	if t != "" && !renew {
		return t, nil
	}
	out, err := run("gh", "auth", "token")
	if err != nil {
		if errors.Is(err, errAuth) || strings.Contains(err.Error(), "not logged") {
			return "", fmt.Errorf("gh: %w", errAuth)
		}
		return "", err
	}
	t = strings.TrimSpace(string(out))
	if t == "" {
		return "", fmt.Errorf("gh: %w", errAuth)
	}
	c.mu.Lock()
	c.token = t
	c.mu.Unlock()
	return t, nil
}

func (c *ghClient) store(path, etag string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= ghMaxEntries {
		var oldest string
		for k, e := range c.cache {
			if oldest == "" || e.at.Before(c.cache[oldest].at) {
				oldest = k
			}
		}
		delete(c.cache, oldest)
	}
	c.cache[path] = &ghEntry{etag: etag, body: body, at: time.Now()}
}

func (c *ghClient) noteLimits(resp *http.Response) {
	rem, err1 := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	lim, err2 := strconv.Atoi(resp.Header.Get("X-RateLimit-Limit"))
	reset, err3 := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err1 == nil {
		c.remaining = rem
	}
	if err2 == nil {
		c.limit = lim
	}
	if err3 == nil {
		c.reset = time.Unix(reset, 0)
	}
}

// limitedUntil recognizes both of GitHub's limits: the hourly one (no requests left, wait for the reset) and the
// secondary one for bursts (Retry-After, or a minute when it does not say).
func (c *ghClient) limitedUntil(resp *http.Response, msg string) (time.Time, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var until time.Time
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		until = time.Now().Add(time.Duration(s) * time.Second)
	} else if c.remaining == 0 && c.reset.After(time.Now()) {
		until = c.reset
	} else if strings.Contains(strings.ToLower(msg), "rate limit") {
		until = time.Now().Add(time.Minute)
	} else {
		return time.Time{}, false
	}
	// A moment past the reset, so the first request after it is not refused again.
	until = until.Add(2 * time.Second)
	if until.After(c.paused) {
		c.paused = until
	}
	return c.paused, true
}

// ghBudget describes the hourly budget for the footer: empty while it is healthy.
func ghBudget() string {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	switch {
	case time.Now().Before(gh.paused):
		return "GitHub paused until " + gh.paused.Local().Format("15:04") + " (rate limit)"
	case gh.remaining >= 0 && gh.remaining < ghLowBudget:
		return fmt.Sprintf("GitHub: %d requests left until %s, refreshing less often", gh.remaining, gh.reset.Local().Format("15:04"))
	}
	return ""
}
