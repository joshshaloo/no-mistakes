package bitbucket

import "testing"

func TestResolvedEnvironmentDoesNotResurrectAmbientRESTCredentials(t *testing.T) {
	t.Setenv(envEmail, "ambient@example.com")
	t.Setenv(envToken, "ambient-token")
	api, err := NewAPIFromEnvironment([]string{"PATH=/bin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := api.(*BKTClient); !ok {
		t.Fatalf("resolved empty credentials fell back to ambient REST: %T", api)
	}
}
