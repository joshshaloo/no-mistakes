package runcompletion

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSuccessfulCompletionPersistenceHasSingleCaller(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	var callers []string
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || path == filepath.Join(root, "internal", "db", "run.go") {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(contents), ".CompleteSuccessfulRun(") {
			callers = append(callers, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "internal", "runcompletion", "finalizer.go")
	if len(callers) != 1 || callers[0] != want {
		t.Fatalf("successful completion persistence callers = %v, want [%s]", callers, want)
	}
}
