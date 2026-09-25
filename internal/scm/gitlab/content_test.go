package gitlab

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestGetPRContent(t *testing.T) {
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr view 123 --output json": {stdout: `{"iid":123,"title":"original title","description":"original\nbody"}`},
	}), nil, "", "")
	got, err := host.GetPRContent(context.Background(), &scm.PR{URL: "https://gitlab.com/test/repo/-/merge_requests/123"})
	if err != nil || got.Title != "original title" || got.Body != "original\nbody" {
		t.Fatalf("content=%+v, err=%v", got, err)
	}
}
