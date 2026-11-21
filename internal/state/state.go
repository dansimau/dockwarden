// Package state persists the base image digests that compose services were
// last built from, so dockwarden can tell when a rebuild is needed.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ServiceState records what a built service's image was built from.
type ServiceState struct {
	// BaseDigests maps base image reference -> digest used at last build.
	BaseDigests map[string]string `json:"base_digests"`
	// ImageID is the local image ID produced by the last build (informational).
	ImageID   string    `json:"image_id,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// File is a JSON-backed state store. A nil *File behaves like an empty,
// non-persistent store.
type File struct {
	path string
	mu   sync.Mutex
	data struct {
		Version  int                     `json:"version"`
		Services map[string]ServiceState `json:"services"`
	}
}

// Key builds the state key for a compose service.
func Key(project, service string) string { return project + "/" + service }

// Load reads the state file; a missing file yields an empty state.
func Load(path string) (*File, error) {
	f := &File{path: path}
	f.data.Version = 1
	f.data.Services = map[string]ServiceState{}
	if path == "" {
		return f, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return f, nil
	}
	if err := json.Unmarshal(b, &f.data); err != nil {
		return nil, fmt.Errorf("parse state file %s: %w", path, err)
	}
	if f.data.Services == nil {
		f.data.Services = map[string]ServiceState{}
	}
	return f, nil
}

// Get returns the recorded state for a key.
func (f *File) Get(key string) (ServiceState, bool) {
	if f == nil {
		return ServiceState{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.data.Services[key]
	return s, ok
}

// Set records state for a key and persists it.
func (f *File) Set(key string, s ServiceState) error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = time.Now().UTC()
	}
	f.data.Services[key] = s
	return f.saveLocked()
}

// Keys returns all keys, sorted.
func (f *File) Keys() []string {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.data.Services))
	for k := range f.data.Services {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (f *File) saveLocked() error {
	if f.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}
