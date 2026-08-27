package state

import (
	"path/filepath"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !store.Fresh {
		t.Fatal("new store should be fresh")
	}
	store.Mark("abc", "completed")
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Fresh || !reloaded.Has("abc") {
		t.Fatalf("state did not persist: %#v", reloaded)
	}
}
