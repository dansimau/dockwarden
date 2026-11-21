package state

import (
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "state.json")
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Get(Key("p", "s")); ok {
		t.Fatal("unexpected state")
	}
	if err := f.Set(Key("p", "s"), ServiceState{BaseDigests: map[string]string{"nginx:1": "sha256:a"}}); err != nil {
		t.Fatal(err)
	}
	f2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := f2.Get("p/s")
	if !ok || s.BaseDigests["nginx:1"] != "sha256:a" || s.UpdatedAt.IsZero() {
		t.Fatalf("got %+v %v", s, ok)
	}
	if k := f2.Keys(); len(k) != 1 || k[0] != "p/s" {
		t.Fatalf("keys %v", k)
	}
}

func TestNilAndEmptyPath(t *testing.T) {
	var f *File
	if err := f.Set("x", ServiceState{}); err != nil {
		t.Fatal(err)
	}
	f, _ = Load("")
	if err := f.Set("x", ServiceState{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Get("x"); !ok {
		t.Fatal("in-memory state should work")
	}
}
