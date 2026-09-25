package github

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestGetPRContentTargetsExactPR(t *testing.T) {
	url := "https://github.com/test/repo/pull/123"
	for _, pr := range []*scm.PR{{Number: "123"}, {URL: url}} {
		selector := pr.Number
		if selector == "" {
			selector = url
		}
		host := New(githubTestCmdFactory(map[string]githubTestResponse{
			"gh pr view " + selector + " --repo test/repo --json title,body": {stdout: `{"title":"original title","body":"summary\n\n## Risk Assessment\n\nLow"}`},
		}), nil, "", "test/repo")
		got, err := host.GetPRContent(context.Background(), pr)
		if err != nil || got.Title != "original title" || got.Body != "summary\n\n## Risk Assessment\n\nLow" {
			t.Fatalf("content=%+v, err=%v", got, err)
		}
		if _, err := host.GetPRContent(context.Background(), &scm.PR{}); err == nil {
			t.Fatal("missing identity did not fail closed")
		}
	}
}
