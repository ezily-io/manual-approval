package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-github/v43/github"
)

func TestSearchRetryDelay(t *testing.T) {
	retryAfter := 7 * time.Second

	t.Run("abuse_rate_limit_with_retry_after", func(t *testing.T) {
		err := &github.AbuseRateLimitError{RetryAfter: &retryAfter}
		delay, retryable := searchRetryDelay(err, 0)
		if !retryable {
			t.Fatal("expected an abuse rate limit error to be retryable")
		}
		if delay != retryAfter {
			t.Fatalf("expected delay %s, got %s", retryAfter, delay)
		}
	})

	t.Run("abuse_rate_limit_without_retry_after_falls_back_to_backoff", func(t *testing.T) {
		err := &github.AbuseRateLimitError{}
		delay, retryable := searchRetryDelay(err, 2)
		if !retryable {
			t.Fatal("expected an abuse rate limit error to be retryable")
		}
		if delay != backoffDelay(2) {
			t.Fatalf("expected backoff delay %s, got %s", backoffDelay(2), delay)
		}
	})

	t.Run("rate_limit_error_waits_until_reset", func(t *testing.T) {
		err := &github.RateLimitError{Rate: github.Rate{Reset: github.Timestamp{Time: timeNow().Add(3 * time.Second)}}}
		delay, retryable := searchRetryDelay(err, 0)
		if !retryable {
			t.Fatal("expected a rate limit error to be retryable")
		}
		if delay <= 0 || delay > 3*time.Second {
			t.Fatalf("expected a delay close to 3s, got %s", delay)
		}
	})

	t.Run("rate_limit_error_with_reset_in_the_past_falls_back_to_backoff", func(t *testing.T) {
		err := &github.RateLimitError{Rate: github.Rate{Reset: github.Timestamp{Time: timeNow().Add(-time.Second)}}}
		delay, retryable := searchRetryDelay(err, 1)
		if !retryable {
			t.Fatal("expected a rate limit error to be retryable")
		}
		if delay != backoffDelay(1) {
			t.Fatalf("expected backoff delay %s, got %s", backoffDelay(1), delay)
		}
	})

	t.Run("non_rate_limit_error_is_not_retryable", func(t *testing.T) {
		_, retryable := searchRetryDelay(errors.New("boom"), 0)
		if retryable {
			t.Fatal("expected a generic error not to be retryable")
		}
	})
}

func TestBackoffDelay(t *testing.T) {
	if backoffDelay(0) != searchRetryBaseDelay {
		t.Fatalf("expected attempt 0 to be the base delay %s, got %s", searchRetryBaseDelay, backoffDelay(0))
	}
	if backoffDelay(1) != 2*searchRetryBaseDelay {
		t.Fatalf("expected attempt 1 to double the base delay, got %s", backoffDelay(1))
	}
	if got := backoffDelay(10); got != searchRetryMaxDelay {
		t.Fatalf("expected a large attempt number to cap at %s, got %s", searchRetryMaxDelay, got)
	}
}

// abuseRateLimitHandler writes a 403 response shaped like GitHub's secondary
// rate limit error, with Retry-After set to 0 so tests don't actually wait.
func abuseRateLimitHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Retry-After", "0")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit","documentation_url":"https://docs.github.com/en/rest/overview/resources-in-the-rest-api#secondary-rate-limits"}`)
}

func TestSearchIssuesByTitle(t *testing.T) {
	title := "Production deployment approval - abc123"

	t.Run("retries_on_rate_limit_then_succeeds", func(t *testing.T) {
		calls := 0
		mux := http.NewServeMux()
		mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				abuseRateLimitHandler(w, r)
				return
			}
			writeJSON(t, w, &github.IssuesSearchResult{
				Total:  github.Int(1),
				Issues: []*github.Issue{{Number: github.Int(10), Title: github.String(title)}},
			})
		})

		a := &approvalEnvironment{client: newTestClient(t, mux), targetRepoOwner: "o", targetRepoName: "r"}
		issues, err := a.searchIssuesByTitle(context.Background(), title)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(issues) != 1 || issues[0].GetNumber() != 10 {
			t.Fatalf("expected to find issue #10, got %+v", issues)
		}
		if calls != 2 {
			t.Fatalf("expected exactly one retry (2 calls), got %d", calls)
		}
	})

	t.Run("gives_up_after_exhausting_retries", func(t *testing.T) {
		calls := 0
		mux := http.NewServeMux()
		mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
			calls++
			abuseRateLimitHandler(w, r)
		})

		a := &approvalEnvironment{client: newTestClient(t, mux), targetRepoOwner: "o", targetRepoName: "r"}
		if _, err := a.searchIssuesByTitle(context.Background(), title); err == nil {
			t.Fatal("expected an error once retries are exhausted")
		}
		if want := maxSearchRetries + 1; calls != want {
			t.Fatalf("expected %d calls (initial + %d retries), got %d", want, maxSearchRetries, calls)
		}
	})

	t.Run("does_not_retry_a_non_rate_limit_error", func(t *testing.T) {
		calls := 0
		mux := http.NewServeMux()
		mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"message":"Bad credentials"}`)
		})

		a := &approvalEnvironment{client: newTestClient(t, mux), targetRepoOwner: "o", targetRepoName: "r"}
		if _, err := a.searchIssuesByTitle(context.Background(), title); err == nil {
			t.Fatal("expected an error")
		}
		if calls != 1 {
			t.Fatalf("expected no retries for a non-rate-limit error, got %d calls", calls)
		}
	})
}
