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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Role defines the access level for an authenticated user.
type Role string

const (
	RoleOperator Role = "operator" // read-only: /health, /sessions, /audit, /config (GET)
	RoleAdmin    Role = "admin"    // read-write: /kill, /revive, /flags, /config (POST/PATCH)
)

// AuthConfig holds authentication configuration.
type AuthConfig struct {
	// Enabled turns on bearer-token auth for admin HTTP endpoints.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// ApiKey is the key identifier (same as LiveKit api_key).
	ApiKey string `yaml:"-" json:"-"`
	// ApiSecret is the HMAC signing secret (same as LiveKit api_secret).
	ApiSecret string `yaml:"-" json:"-"`
}

// tokenHeader is the JWT-like header (simplified, not full JWT).
type tokenHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// tokenPayload holds the token claims.
type tokenPayload struct {
	Sub  string `json:"sub"`  // api key
	Role Role   `json:"role"` // operator or admin
	Iat  int64  `json:"iat"`  // issued at (unix)
	Exp  int64  `json:"exp"`  // expires at (unix)
}

// GenerateToken creates a signed bearer token for the given role and TTL.
// The token format is: base64(header).base64(payload).base64(hmac-sha256).
func GenerateToken(apiKey, apiSecret string, role Role, ttl time.Duration) (string, error) {
	if apiKey == "" || apiSecret == "" {
		return "", fmt.Errorf("api_key and api_secret are required")
	}
	if role != RoleOperator && role != RoleAdmin {
		return "", fmt.Errorf("invalid role: %s", role)
	}

	now := time.Now()
	header := tokenHeader{Alg: "HS256", Typ: "VB"}
	payload := tokenPayload{
		Sub:  apiKey,
		Role: role,
		Iat:  now.Unix(),
		Exp:  now.Add(ttl).Unix(),
	}

	headerJSON, _ := json.Marshal(header)
	payloadJSON, _ := json.Marshal(payload)

	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)

	signingInput := headerB64 + "." + payloadB64
	sig := signHMAC(signingInput, apiSecret)

	return signingInput + "." + sig, nil
}

// VerifyToken validates a token and returns the payload if valid.
func VerifyToken(token, apiKey, apiSecret string) (*tokenPayload, error) {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed token")
	}

	signingInput := parts[0] + "." + parts[1]
	expectedSig := signHMAC(signingInput, apiSecret)
	if !hmac.Equal([]byte(parts[2]), []byte(expectedSig)) {
		return nil, fmt.Errorf("invalid token signature")
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid token payload encoding: %w", err)
	}

	var payload tokenPayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return nil, fmt.Errorf("invalid token payload: %w", err)
	}

	if payload.Sub != apiKey {
		return nil, fmt.Errorf("token api key mismatch")
	}
	if time.Now().Unix() > payload.Exp {
		return nil, fmt.Errorf("token expired")
	}

	return &payload, nil
}

// AuthMiddleware returns an HTTP middleware that enforces bearer-token authentication.
// writePaths are URL paths that require RoleAdmin (POST/PATCH/DELETE operations).
// All other paths only require RoleOperator.
func AuthMiddleware(cfg AuthConfig, writePaths map[string]bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cfg.Enabled {
				next.ServeHTTP(w, r)
				return
			}

			token := extractBearerToken(r)
			if token == "" {
				http.Error(w, `{"error":"missing authorization header"}`, http.StatusUnauthorized)
				return
			}

			payload, err := VerifyToken(token, cfg.ApiKey, cfg.ApiSecret)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusUnauthorized)
				return
			}

			// Check if this is a write operation requiring admin role
			isWrite := r.Method != http.MethodGet && r.Method != http.MethodHead
			requiresAdmin := isWrite && writePaths[r.URL.Path]

			if requiresAdmin && payload.Role != RoleAdmin {
				http.Error(w, `{"error":"admin role required"}`, http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	// Also check query param for dashboard/browser access
	return r.URL.Query().Get("token")
}

func signHMAC(data, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
