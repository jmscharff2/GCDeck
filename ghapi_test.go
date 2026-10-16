package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.HandlerFunc) *ghClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := newGHClient()
	c.base, c.token = srv.URL, "test-token"
	return c
}

func TestGHRevalidatesWithETag(t *testing.T) {
	var full, notModified atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("no token sent")
		}
		w.Header().Set("X-RateLimit-Remaining", "4000")
		if r.Header.Get("If-None-Match") == `"v1"` {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		full.Add(1)
		w.Header().Set("ETag", `"v1"`)
		fmt.Fprint(w, `{"n":1}`)
	})
	for i := 0; i < 2; i++ {
		if b, err := c.get("x"); err != nil || string(b) != `{"n":1}` {
			t.Fatalf("get %d: %s %v", i, b, err)
		}
	}
	if full.Load() != 1 || notModified.Load() != 0 {
		t.Fatalf("a fresh answer should be reused: %d full, %d revalidated", full.Load(), notModified.Load())
	}
	c.cache["x"].at = time.Now().Add(-time.Hour)
	if b, err := c.get("x"); err != nil || string(b) != `{"n":1}` {
		t.Fatalf("revalidated: %s %v", b, err)
	}
	if full.Load() != 1 || notModified.Load() != 1 {
		t.Errorf("a stale answer should be revalidated with its ETag: %d full, %d revalidated", full.Load(), notModified.Load())
	}
}

func TestGHSharesConcurrentRequests(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		fmt.Fprint(w, `{}`)
	})
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = c.get("same") }()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if hits.Load() != 1 {
		t.Errorf("five questions at once should be one request, were %d", hits.Load())
	}
}

func TestGHPausesAtRateLimit(t *testing.T) {
	var hits atomic.Int32
	reset := time.Now().Add(20 * time.Minute)
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset.Unix()))
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded for user ID 1."}`)
	})
	_, err := c.get("a")
	var rl rateLimitError
	if !errors.As(err, &rl) || rl.until.Before(reset) {
		t.Fatalf("want a pause until the reset, got %v", err)
	}
	for _, p := range []string{"a", "b", "c"} {
		if _, err := c.get(p); !errors.As(err, &rl) {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("nothing should be asked while paused, asked %d times", hits.Load())
	}
}

func TestGHSecondaryLimitUsesRetryAfter(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit."}`)
	})
	_, err := c.get("a")
	var rl rateLimitError
	if !errors.As(err, &rl) || time.Until(rl.until) > time.Minute || time.Until(rl.until) < 20*time.Second {
		t.Fatalf("want a pause of about 30s, got %v", err)
	}
}

func TestGHOtherErrorsDoNotPause(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "{\"message\":\"Not Found\x1b]52;c;eA==\x07\"}")
	})
	_, err := c.get("a")
	if err == nil || err.Error() != "GitHub: 404 Not Found" {
		t.Fatalf("got %v", err)
	}
	if !c.paused.IsZero() {
		t.Error("a 404 must not pause GitHub")
	}
}
