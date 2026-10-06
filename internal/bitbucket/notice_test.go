package bitbucket

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/notices"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

type noticeAPI struct {
	API
	id       int
	body     string
	statuses []CommitStatus
}

func (a *noticeAPI) ListPRStatuses(context.Context, RepoRef, int) ([]CommitStatus, error) {
	return a.statuses, nil
}

func (a *noticeAPI) AddPRComment(_ context.Context, _ RepoRef, id int, body string) error {
	a.id, a.body = id, body
	return nil
}

func TestGetChecksRefusesReusableStatusIdentity(t *testing.T) {
	api := &noticeAPI{statuses: []CommitStatus{{Name: "build", State: "SUCCESSFUL", UUID: "reused-uuid", URL: "https://ci.example/status/reused", UpdatedOn: "2025-01-01T00:00:00Z"}}}
	host := NewHost(api, RepoRef{Workspace: "test", RepoSlug: "repo"})
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatal(err)
	}
	if checks[0].AttemptID != "" {
		t.Fatalf("reusable Bitbucket status became attempt identity: %q", checks[0].AttemptID)
	}
	if _, err := host.GetCIAttemptIdentity(context.Background(), &scm.PR{Number: "123"}, "abc123", checks); err == nil || !strings.Contains(err.Error(), "build-status path does not expose") {
		t.Fatalf("GetCIAttemptIdentity() error = %v", err)
	}
}

func TestPublishPRNoticeAppendsComment(t *testing.T) {
	api := &noticeAPI{}
	host := NewHost(api, RepoRef{Workspace: "test", RepoSlug: "repo"})
	if err := host.PublishPRNotice(context.Background(), &scm.PR{Number: "123"}, "head-bound notice"); err != nil {
		t.Fatal(err)
	}
	if api.id != 123 || api.body != "head-bound notice" {
		t.Fatalf("comment = (%d, %q)", api.id, api.body)
	}
}

func TestListPRCommentsStopsBeforePaginationCapacity(t *testing.T) {
	repo := RepoRef{Workspace: "w", RepoSlug: "r"}

	t.Run("page cap continuation", func(t *testing.T) {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			page := requests
			fmt.Fprintf(w, `{"values":[{"content":{"raw":"page-%d"}}],"next":%q}`, page, fmt.Sprintf("http://%s?page=%d", r.Host, page+1))
		}))
		defer server.Close()
		client := &Client{baseURL: server.URL, email: "test", token: "token", httpClient: server.Client()}
		_, err := client.ListPRComments(context.Background(), repo, 1)
		if !errors.Is(err, notices.ErrCapacity) {
			t.Fatalf("error = %v, want capacity refusal", err)
		}
		if requests != notices.MaxPages {
			t.Fatalf("requests = %d, want %d (no cap+1 request)", requests, notices.MaxPages)
		}
	})

	t.Run("exact page cap EOF", func(t *testing.T) {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			next := ""
			if requests < notices.MaxPages {
				next = fmt.Sprintf("http://%s?page=%d", r.Host, requests+1)
			}
			fmt.Fprintf(w, `{"values":[{"content":{"raw":"x"}}],"next":%q}`, next)
		}))
		defer server.Close()
		client := &Client{baseURL: server.URL, email: "test", token: "token", httpClient: server.Client()}
		bodies, err := client.ListPRComments(context.Background(), repo, 1)
		if err != nil {
			t.Fatal(err)
		}
		if requests != notices.MaxPages || len(bodies) != notices.MaxPages {
			t.Fatalf("requests/bodies = %d/%d, want %d/%d", requests, len(bodies), notices.MaxPages, notices.MaxPages)
		}
	})

	t.Run("comment cap continuation", func(t *testing.T) {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			fmt.Fprint(w, `{"values":[`)
			for i := 0; i < notices.MaxComments; i++ {
				if i > 0 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprint(w, `{"content":{"raw":"x"}}`)
			}
			fmt.Fprintf(w, `],"next":%q}`, fmt.Sprintf("http://%s?page=2", r.Host))
		}))
		defer server.Close()
		client := &Client{baseURL: server.URL, email: "test", token: "token", httpClient: server.Client()}
		_, err := client.ListPRComments(context.Background(), repo, 1)
		if !errors.Is(err, notices.ErrCapacity) {
			t.Fatalf("error = %v, want capacity refusal", err)
		}
		if requests != 1 {
			t.Fatalf("requests = %d, want 1 (no request after comment cap)", requests)
		}
	})

	t.Run("exact comment cap EOF", func(t *testing.T) {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			fmt.Fprint(w, `{"values":[`)
			for i := 0; i < notices.MaxComments; i++ {
				if i > 0 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprint(w, `{"content":{"raw":"x"}}`)
			}
			fmt.Fprint(w, `]}`)
		}))
		defer server.Close()
		client := &Client{baseURL: server.URL, email: "test", token: "token", httpClient: server.Client()}
		bodies, err := client.ListPRComments(context.Background(), repo, 1)
		if err != nil {
			t.Fatal(err)
		}
		if requests != 1 || len(bodies) != notices.MaxComments {
			t.Fatalf("requests/bodies = %d/%d, want 1/%d", requests, len(bodies), notices.MaxComments)
		}
	})
}

func TestListPRCommentsBoundsExternalConversation(t *testing.T) {
	t.Run("large single body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"values":[{"content":{"raw":%q}}]}`, strings.Repeat("x", notices.MaxResponseBytes))
		}))
		defer server.Close()
		client := &Client{baseURL: server.URL, email: "test", token: "token", httpClient: server.Client()}
		_, err := client.ListPRComments(context.Background(), RepoRef{Workspace: "w", RepoSlug: "r"}, 1)
		if !errors.Is(err, notices.ErrCapacity) {
			t.Fatalf("error = %v, want capacity refusal", err)
		}
	})

	t.Run("many comments", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"values":[`)
			for i := 0; i <= notices.MaxComments; i++ {
				if i > 0 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprint(w, `{"content":{"raw":"x"}}`)
			}
			fmt.Fprint(w, `]}`)
		}))
		defer server.Close()
		client := &Client{baseURL: server.URL, email: "test", token: "token", httpClient: server.Client()}
		_, err := client.ListPRComments(context.Background(), RepoRef{Workspace: "w", RepoSlug: "r"}, 1)
		if !errors.Is(err, notices.ErrCapacity) {
			t.Fatalf("error = %v, want comment-count refusal", err)
		}
	})
}
