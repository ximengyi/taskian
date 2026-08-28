package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveCommandKeepsWorkingAbsoluteCommand(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	resolved, ok := ResolveCommand("generic", executable)
	if !ok {
		t.Fatal("expected current executable to resolve")
	}
	want, _ := filepath.Abs(executable)
	got, _ := filepath.Abs(resolved)
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("resolved=%q want=%q", got, want)
	}
}
