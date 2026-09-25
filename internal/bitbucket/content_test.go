package bitbucket

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

type contentAPI struct {
	API
	pr *PullRequest
}

func (c contentAPI) GetPR(context.Context, RepoRef, int) (*PullRequest, error) { return c.pr, nil }

func TestGetPRContentPreservesBothTransportPayloads(t *testing.T) {
	raw := []byte(`{"id":123,"title":"original title","description":"original\nbody"}`)
	var rest bitbucketPullRequest
	var bkt bktPullRequest
	if err := json.Unmarshal(raw, &rest); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &bkt); err != nil {
		t.Fatal(err)
	}
	repo := RepoRef{Workspace: "test", RepoSlug: "repo"}
	fromBKT, err := bktPRToPullRequest(repo, bkt)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range []*PullRequest{rest.toPullRequest(), fromBKT} {
		host := NewHost(contentAPI{pr: pr}, repo)
		got, err := host.GetPRContent(context.Background(), &scm.PR{Number: "123"})
		if err != nil || got.Title != "original title" || got.Body != "original\nbody" {
			t.Fatalf("content=%+v, err=%v", got, err)
		}
	}
}
