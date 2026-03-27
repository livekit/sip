// Copyright 2024 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package security

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- Env Provider ---

func TestEnvSecretProvider_Get(t *testing.T) {
	t.Setenv("LIVEKIT_API_KEY", "test-key-123")
	p := NewEnvSecretProvider()
	val, err := p.Get("api_key")
	if err != nil {
		t.Fatal(err)
	}
	if val != "test-key-123" {
		t.Errorf("expected test-key-123, got %s", val)
	}
}

func TestEnvSecretProvider_Missing(t *testing.T) {
	p := NewEnvSecretProvider()
	_, err := p.Get("nonexistent_secret_key_xyz")
	if err == nil {
		t.Error("expected error for missing env var")
	}
}

func TestEnvKeyName(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"api_key", "LIVEKIT_API_KEY"},
		{"api_secret", "LIVEKIT_API_SECRET"},
		{"redis.password", "LIVEKIT_REDIS_PASSWORD"},
	}
	for _, tc := range cases {
		got := envKeyName(tc.input)
		if got != tc.expected {
			t.Errorf("envKeyName(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

// --- File Provider ---

func TestFileSecretProvider_Get(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "api_key"), []byte("file-key-456\n"), 0600)

	p, err := NewFileSecretProvider(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	val, err := p.Get("api_key")
	if err != nil {
		t.Fatal(err)
	}
	if val != "file-key-456" {
		t.Errorf("expected file-key-456, got %q", val)
	}
}

func TestFileSecretProvider_Missing(t *testing.T) {
	dir := t.TempDir()
	p, err := NewFileSecretProvider(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	_, err = p.Get("nonexistent")
	if err == nil {
		t.Error("expected error for missing secret file")
	}
}

func TestFileSecretProvider_Cached(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "api_key"), []byte("cached-val"), 0600)

	p, err := NewFileSecretProvider(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// First read
	val1, _ := p.Get("api_key")
	// Second read (should hit cache)
	val2, _ := p.Get("api_key")
	if val1 != val2 {
		t.Errorf("cached values should match: %q vs %q", val1, val2)
	}
}

func TestFileSecretProvider_Refresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api_key")
	os.WriteFile(path, []byte("original"), 0600)

	p, err := NewFileSecretProvider(dir, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// Initial read to populate cache
	val, _ := p.Get("api_key")
	if val != "original" {
		t.Fatalf("expected 'original', got %q", val)
	}

	// Update file with a future mtime
	time.Sleep(100 * time.Millisecond)
	os.WriteFile(path, []byte("updated"), 0600)

	// Wait for refresh cycle
	time.Sleep(200 * time.Millisecond)

	val, _ = p.Get("api_key")
	if val != "updated" {
		t.Errorf("expected 'updated' after refresh, got %q", val)
	}
}

func TestFileSecretProvider_BadDir(t *testing.T) {
	_, err := NewFileSecretProvider("/nonexistent/path", time.Second)
	if err == nil {
		t.Error("expected error for nonexistent directory")
	}
}

func TestFileSecretProvider_NotADir(t *testing.T) {
	f, _ := os.CreateTemp("", "secret-test")
	f.Close()
	defer os.Remove(f.Name())

	_, err := NewFileSecretProvider(f.Name(), time.Second)
	if err == nil {
		t.Error("expected error when path is not a directory")
	}
}

// --- NewSecretProvider ---

func TestNewSecretProvider_Env(t *testing.T) {
	p, err := NewSecretProvider(SecretsConfig{Provider: "env"})
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
}

func TestNewSecretProvider_Default(t *testing.T) {
	p, err := NewSecretProvider(SecretsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
}

func TestNewSecretProvider_File(t *testing.T) {
	dir := t.TempDir()
	p, err := NewSecretProvider(SecretsConfig{Provider: "file", FilePath: dir})
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
}

func TestNewSecretProvider_Unknown(t *testing.T) {
	_, err := NewSecretProvider(SecretsConfig{Provider: "vault"})
	if err == nil {
		t.Error("expected error for unknown provider")
	}
}
