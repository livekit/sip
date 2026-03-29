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
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// SecretsConfig holds secret management configuration.
type SecretsConfig struct {
	// Provider selects the secret provider: "env" (default) or "file".
	Provider string `yaml:"provider" json:"provider"`
	// FilePath is the directory containing secret files (for "file" provider).
	// Each secret is a file named after the key (e.g., api_key, api_secret).
	FilePath string `yaml:"file_path" json:"file_path,omitempty"`
	// RefreshInterval is how often to re-read file secrets (default: 30s).
	RefreshInterval time.Duration `yaml:"refresh_interval" json:"refresh_interval,omitempty"`
}

// SecretProvider reads secrets by key name.
type SecretProvider interface {
	// Get returns the secret value for the given key, or error if not found.
	Get(key string) (string, error)
	// Close stops any background goroutines.
	Close()
}

// --- Env Provider ---

// EnvSecretProvider reads secrets from environment variables.
// Key mapping: "api_key" → LIVEKIT_API_KEY, "api_secret" → LIVEKIT_API_SECRET, etc.
type EnvSecretProvider struct{}

// NewEnvSecretProvider creates an environment-based secret provider.
func NewEnvSecretProvider() *EnvSecretProvider {
	return &EnvSecretProvider{}
}

func (e *EnvSecretProvider) Get(key string) (string, error) {
	envKey := envKeyName(key)
	val := os.Getenv(envKey)
	if val == "" {
		return "", fmt.Errorf("secret %q not found (env: %s)", key, envKey)
	}
	return val, nil
}

func (e *EnvSecretProvider) Close() {}

func envKeyName(key string) string {
	return "LIVEKIT_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// --- File Provider ---

// FileSecretProvider reads secrets from files in a directory.
// Supports auto-refresh by watching file modification times.
// Suitable for Vault agent sidecar, Kubernetes secrets, or similar.
type FileSecretProvider struct {
	dir      string
	interval time.Duration

	mu    sync.RWMutex
	cache map[string]cachedSecret
	done  chan struct{}
}

type cachedSecret struct {
	value   string
	modTime time.Time
}

// NewFileSecretProvider creates a file-based secret provider that reads
// secrets from the given directory. Each secret is a file named after the key.
func NewFileSecretProvider(dir string, refreshInterval time.Duration) (*FileSecretProvider, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("secrets directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("secrets path is not a directory: %s", dir)
	}

	if refreshInterval <= 0 {
		refreshInterval = 30 * time.Second
	}

	fp := &FileSecretProvider{
		dir:      dir,
		interval: refreshInterval,
		cache:    make(map[string]cachedSecret),
		done:     make(chan struct{}),
	}

	// Start background refresh
	go fp.refreshLoop()

	return fp, nil
}

func (f *FileSecretProvider) Get(key string) (string, error) {
	// Check cache first
	f.mu.RLock()
	if cached, ok := f.cache[key]; ok {
		f.mu.RUnlock()
		return cached.value, nil
	}
	f.mu.RUnlock()

	// Read from file
	return f.readAndCache(key)
}

func (f *FileSecretProvider) Close() {
	select {
	case <-f.done:
	default:
		close(f.done)
	}
}

func (f *FileSecretProvider) readAndCache(key string) (string, error) {
	path := f.dir + "/" + key

	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("secret file %q: %w", key, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading secret %q: %w", key, err)
	}

	value := strings.TrimSpace(string(data))

	f.mu.Lock()
	f.cache[key] = cachedSecret{value: value, modTime: info.ModTime()}
	f.mu.Unlock()

	return value, nil
}

func (f *FileSecretProvider) refreshLoop() {
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()

	for {
		select {
		case <-f.done:
			return
		case <-ticker.C:
			f.refreshAll()
		}
	}
}

func (f *FileSecretProvider) refreshAll() {
	f.mu.RLock()
	keys := make([]string, 0, len(f.cache))
	for k := range f.cache {
		keys = append(keys, k)
	}
	f.mu.RUnlock()

	for _, key := range keys {
		path := f.dir + "/" + key
		info, err := os.Stat(path)
		if err != nil {
			continue
		}

		f.mu.RLock()
		cached := f.cache[key]
		f.mu.RUnlock()

		if info.ModTime().After(cached.modTime) {
			// File changed, re-read
			f.readAndCache(key)
		}
	}
}

// NewSecretProvider creates a secret provider based on configuration.
func NewSecretProvider(cfg SecretsConfig) (SecretProvider, error) {
	switch cfg.Provider {
	case "file":
		return NewFileSecretProvider(cfg.FilePath, cfg.RefreshInterval)
	case "env", "":
		return NewEnvSecretProvider(), nil
	default:
		return nil, fmt.Errorf("unknown secret provider: %s", cfg.Provider)
	}
}
