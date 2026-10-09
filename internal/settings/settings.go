// Package settings persists what is entered in the web UI as a JSON file.
// Environment variables take precedence (see cmd/alpha2web).
package settings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Values are the settings that can be changed at runtime.
type Values struct {
	CameraHost        string `json:"camera_host,omitempty"`
	CameraMAC         string `json:"camera_mac,omitempty"`
	CameraUser        string `json:"camera_user,omitempty"`
	CameraPassword    string `json:"camera_password,omitempty"`
	CameraFingerprint string `json:"camera_fingerprint,omitempty"`
	// Plugins holds each plugin's own key/value settings, by plugin name.
	Plugins map[string]map[string]string `json:"plugins,omitempty"`
}

// Store reads and writes the settings file.
type Store struct {
	path string
	mu   sync.Mutex
	v    Values
}

// Open loads path; a missing file means empty settings.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	return s, json.Unmarshal(b, &s.v)
}

func (s *Store) Get() Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v
}

// Set replaces and persists the values atomically with mode 0600 (it holds the camera password).
func (s *Store) Set(v Values) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.v = v
	return nil
}
