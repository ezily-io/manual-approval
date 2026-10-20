package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-github/v43/github"
)

func newTestClient(t *testing.T, mux *http.ServeMux) *github.Client {
	t.Helper()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client := github.NewClient(nil)
	baseURL, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("failed to parse test server URL: %v", err)
	}
	client.BaseURL = baseURL
	client.UploadURL = baseURL
	return client
}

func writeJSON(t *testing.T, w http.ResponseWriter, v interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("failed to encode test response: %v", err)
	}
}

func TestResolvedIssueTitle(t *testing.T) {
	testCases := []struct {
		name       string
		issueTitle string
		runID      int
		expected   string
	}{
		{
			name:       "custom_title",
			issueTitle: "Production deployment approval - abc123",
			runID:      42,
			expected:   "Production deployment approval - abc123",
		},
		{
			name:       "default_title_from_run_id",
			issueTitle: "",
			runID:      42,
			expected:   "Manual approval required for workflow run 42",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			a := approvalEnvironment{issueTitle: testCase.issueTitle, runID: testCase.runID}
			if actual := a.resolvedIssueTitle(); actual != testCase.expected {
				t.Fatalf("expected %q but got %q", testCase.expected, actual)
			}
		})
	}
}

func TestExactTitleMatches(t *testing.T) {
	title := "Production deployment approval - abc123"
	other := "Production deployment approval - def456"

	issues := []*github.Issue{
		{Number: github.Int(1), Title: github.String(title)},
		{Number: github.Int(2), Title: github.String(other)},
		{Number: github.Int(3), Title: github.String(title)},
	}

	matches := exactTitleMatches(issues, title)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches but got %d", len(matches))
	}
	for _, issue := range matches {
		if issue.GetTitle() != title {
			t.Fatalf("unexpected issue in matches: #%d %q", issue.GetNumber(), issue.GetTitle())
		}
	}
}

func TestLowestNumberedIssue(t *testing.T) {
	issues := []*github.Issue{
		{Number: github.Int(20)},
		{Number: github.Int(5)},
		{Number: github.Int(15)},
	}

	canonical := lowestNumberedIssue(issues)
	if canonical.GetNumber() != 5 {
		t.Fatalf("expected issue #5 but got #%d", canonical.GetNumber())
	}
}

func TestFindExistingIssue(t *testing.T) {
	title := "Production deployment approval - abc123"

	t.Run("found_reuses_lowest_numbered_match", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, &github.IssuesSearchResult{
				Total: github.Int(2),
				Issues: []*github.Issue{
					{Number: github.Int(20), Title: github.String(title), HTMLURL: github.String("https://github.com/o/r/issues/20")},
					{Number: github.Int(10), Title: github.String(title), HTMLURL: github.String("https://github.com/o/r/issues/10")},
				},
			})
		})

		a := &approvalEnvironment{
			client:          newTestClient(t, mux),
			issueTitle:      title,
			targetRepoOwner: "o",
			targetRepoName:  "r",
		}

		found, err := a.findExistingIssue(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !found {
			t.Fatal("expected an existing issue to be found")
		}
		if a.approvalIssueNumber != 10 {
			t.Fatalf("expected issue #10 (lowest numbered) but got #%d", a.approvalIssueNumber)
		}
	})

	t.Run("not_found_when_search_empty", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, &github.IssuesSearchResult{Total: github.Int(0), Issues: []*github.Issue{}})
		})

		a := &approvalEnvironment{
			client:          newTestClient(t, mux),
			issueTitle:      title,
			targetRepoOwner: "o",
			targetRepoName:  "r",
		}

		found, err := a.findExistingIssue(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if found {
			t.Fatalf("expected no issue to be found but got #%d", a.approvalIssueNumber)
		}
	})

	t.Run("ignores_non_exact_title_matches", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, &github.IssuesSearchResult{
				Total: github.Int(1),
				Issues: []*github.Issue{
					{Number: github.Int(99), Title: github.String("Production deployment approval - different-sha")},
				},
			})
		})

		a := &approvalEnvironment{
			client:          newTestClient(t, mux),
			issueTitle:      title,
			targetRepoOwner: "o",
			targetRepoName:  "r",
		}

		found, err := a.findExistingIssue(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if found {
			t.Fatal("expected a title that only partially matches not to count as found")
		}
	})
}

func TestFindOrCreateApprovalIssue(t *testing.T) {
	// Speed up the test; the delay only matters for real search index lag.
	originalDelay := duplicateReconcileDelay
	duplicateReconcileDelay = 0
	t.Cleanup(func() { duplicateReconcileDelay = originalDelay })

	title := "Production deployment approval - abc123"

	t.Run("reuses_existing_issue_without_creating", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, &github.IssuesSearchResult{
				Total: github.Int(1),
				Issues: []*github.Issue{
					{Number: github.Int(42), Title: github.String(title), HTMLURL: github.String("https://github.com/o/r/issues/42")},
				},
			})
		})
		mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("expected no issue creation when an existing issue was found, got %s %s", r.Method, r.URL.Path)
		})

		a := &approvalEnvironment{
			client:          newTestClient(t, mux),
			issueTitle:      title,
			targetRepoOwner: "o",
			targetRepoName:  "r",
		}

		if err := a.findOrCreateApprovalIssue(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a.approvalIssueNumber != 42 {
			t.Fatalf("expected to reuse issue #42 but got #%d", a.approvalIssueNumber)
		}
	})

	t.Run("creates_when_none_found_and_no_duplicate_appears", func(t *testing.T) {
		searchCalls := 0
		mux := http.NewServeMux()
		mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
			searchCalls++
			if searchCalls == 1 {
				// Nothing exists yet.
				writeJSON(t, w, &github.IssuesSearchResult{Total: github.Int(0), Issues: []*github.Issue{}})
				return
			}
			// Reconcile pass: only the issue we just created shows up.
			writeJSON(t, w, &github.IssuesSearchResult{
				Total: github.Int(1),
				Issues: []*github.Issue{
					{Number: github.Int(7), Title: github.String(title), HTMLURL: github.String("https://github.com/o/r/issues/7")},
				},
			})
		})
		mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Fatalf("unexpected method %s on issue creation", r.Method)
			}
			writeJSON(t, w, &github.Issue{
				Number:  github.Int(7),
				Title:   github.String(title),
				HTMLURL: github.String("https://github.com/o/r/issues/7"),
			})
		})

		a := &approvalEnvironment{
			client:          newTestClient(t, mux),
			issueTitle:      title,
			issueBody:       "**Changed files:**\n- terraform/cm/prod.tfvars",
			issueApprovers:  []string{"alice"},
			targetRepoOwner: "o",
			targetRepoName:  "r",
			runID:           123,
		}

		if err := a.findOrCreateApprovalIssue(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a.approvalIssueNumber != 7 {
			t.Fatalf("expected newly created issue #7 but got #%d", a.approvalIssueNumber)
		}
		if searchCalls != 2 {
			t.Fatalf("expected 2 search calls (find + reconcile) but got %d", searchCalls)
		}
	})
}

func TestReconcileDuplicate(t *testing.T) {
	originalDelay := duplicateReconcileDelay
	duplicateReconcileDelay = 0
	t.Cleanup(func() { duplicateReconcileDelay = originalDelay })

	title := "Production deployment approval - abc123"

	t.Run("defers_to_lowest_numbered_and_closes_ours", func(t *testing.T) {
		var commentedIssue, editedIssue int
		var closedState string

		mux := http.NewServeMux()
		mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, &github.IssuesSearchResult{
				Total: github.Int(2),
				Issues: []*github.Issue{
					{Number: github.Int(20), Title: github.String(title)}, // ours, created "second"
					{Number: github.Int(15), Title: github.String(title)}, // canonical, created first
				},
			})
		})
		mux.HandleFunc("/repos/o/r/issues/20/comments", func(w http.ResponseWriter, r *http.Request) {
			commentedIssue = 20
			writeJSON(t, w, &github.IssueComment{})
		})
		mux.HandleFunc("/repos/o/r/issues/20", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPatch {
				t.Fatalf("unexpected method %s on issue edit", r.Method)
			}
			var body struct {
				State string `json:"state"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode request body: %v", err)
			}
			editedIssue = 20
			closedState = body.State
			writeJSON(t, w, &github.Issue{Number: github.Int(20)})
		})

		a := &approvalEnvironment{
			client:              newTestClient(t, mux),
			issueTitle:          title,
			targetRepoOwner:     "o",
			targetRepoName:      "r",
			approvalIssueNumber: 20,
			approvalIssue:       &github.Issue{Number: github.Int(20)},
		}

		if err := a.reconcileDuplicate(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a.approvalIssueNumber != 15 {
			t.Fatalf("expected to defer to issue #15 but got #%d", a.approvalIssueNumber)
		}
		if commentedIssue != 20 || editedIssue != 20 || closedState != "closed" {
			t.Fatalf("expected issue #20 to be commented on and closed, got comment=%d edit=%d state=%q", commentedIssue, editedIssue, closedState)
		}
	})

	t.Run("no_op_when_only_one_match", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, &github.IssuesSearchResult{
				Total:  github.Int(1),
				Issues: []*github.Issue{{Number: github.Int(20), Title: github.String(title)}},
			})
		})
		mux.HandleFunc("/repos/o/r/issues/20/comments", func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("expected no comment to be posted when there is no duplicate")
		})
		mux.HandleFunc("/repos/o/r/issues/20", func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("expected no edit when there is no duplicate")
		})

		a := &approvalEnvironment{
			client:              newTestClient(t, mux),
			issueTitle:          title,
			targetRepoOwner:     "o",
			targetRepoName:      "r",
			approvalIssueNumber: 20,
			approvalIssue:       &github.Issue{Number: github.Int(20)},
		}

		if err := a.reconcileDuplicate(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a.approvalIssueNumber != 20 {
			t.Fatalf("expected to keep issue #20 but got #%d", a.approvalIssueNumber)
		}
	})

	t.Run("no_op_when_ours_is_already_canonical", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, &github.IssuesSearchResult{
				Total: github.Int(2),
				Issues: []*github.Issue{
					{Number: github.Int(10), Title: github.String(title)}, // ours, already the lowest
					{Number: github.Int(20), Title: github.String(title)},
				},
			})
		})
		mux.HandleFunc("/repos/o/r/issues/10/comments", func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("expected no comment on our own canonical issue")
		})
		mux.HandleFunc("/repos/o/r/issues/10", func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("expected no edit on our own canonical issue")
		})

		a := &approvalEnvironment{
			client:              newTestClient(t, mux),
			issueTitle:          title,
			targetRepoOwner:     "o",
			targetRepoName:      "r",
			approvalIssueNumber: 10,
			approvalIssue:       &github.Issue{Number: github.Int(10)},
		}

		if err := a.reconcileDuplicate(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a.approvalIssueNumber != 10 {
			t.Fatalf("expected to keep issue #10 but got #%d", a.approvalIssueNumber)
		}
	})
}

func TestCreateApprovalIssueEmbedsCustomBody(t *testing.T) {
	var createdBody string
	var commentPosted bool

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body struct {
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode request body: %v", err)
			}
			createdBody = body.Body
			writeJSON(t, w, &github.Issue{
				Number:  github.Int(1),
				Title:   github.String(body.Title),
				HTMLURL: github.String("https://github.com/o/r/issues/1"),
			})
			return
		}
		t.Fatalf("unexpected method %s", r.Method)
	})
	mux.HandleFunc("/repos/o/r/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		commentPosted = true
		writeJSON(t, w, &github.IssueComment{})
	})

	a := &approvalEnvironment{
		client:          newTestClient(t, mux),
		issueTitle:      "Production deployment approval - abc123",
		issueBody:       "**Changed files:**\n- terraform/cm/prod.tfvars",
		issueApprovers:  []string{"alice"},
		targetRepoOwner: "o",
		targetRepoName:  "r",
		runID:           123,
	}

	if err := a.createApprovalIssue(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if commentPosted {
		t.Fatal("expected the custom body to be embedded in the issue, not posted as a comment")
	}
	if !contains(createdBody, a.issueBody) {
		t.Fatalf("expected issue body to contain the custom issue-body, got:\n%s", createdBody)
	}
}

func TestCreateApprovalIssueDefersLongCustomBodyToComment(t *testing.T) {
	longBody := ""
	for i := 0; i < maxEmbeddedCustomBodyLength+1000; i++ {
		longBody += "a"
	}

	var commentPosted bool
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, &github.Issue{
			Number:  github.Int(1),
			Title:   github.String("t"),
			HTMLURL: github.String("https://github.com/o/r/issues/1"),
		})
	})
	mux.HandleFunc("/repos/o/r/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		commentPosted = true
		writeJSON(t, w, &github.IssueComment{})
	})

	a := &approvalEnvironment{
		client:          newTestClient(t, mux),
		issueTitle:      "t",
		issueBody:       longBody,
		targetRepoOwner: "o",
		targetRepoName:  "r",
	}

	if err := a.createApprovalIssue(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !commentPosted {
		t.Fatal("expected an overly long custom body to be posted as a follow-up comment")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})())
}
