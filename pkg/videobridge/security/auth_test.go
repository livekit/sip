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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	testKey    = "APItest123"
	testSecret = "secret456xyz"
)

func TestGenerateToken_Valid(t *testing.T) {
	token, err := GenerateToken(testKey, testSecret, RoleAdmin, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Error("expected non-empty token")
	}
}

func TestGenerateToken_InvalidRole(t *testing.T) {
	_, err := GenerateToken(testKey, testSecret, "superadmin", time.Hour)
	if err == nil {
		t.Error("expected error for invalid role")
	}
}

func TestGenerateToken_EmptyCredentials(t *testing.T) {
	_, err := GenerateToken("", testSecret, RoleAdmin, time.Hour)
	if err == nil {
		t.Error("expected error for empty api_key")
	}
	_, err = GenerateToken(testKey, "", RoleAdmin, time.Hour)
	if err == nil {
		t.Error("expected error for empty api_secret")
	}
}

func TestVerifyToken_Valid(t *testing.T) {
	token, _ := GenerateToken(testKey, testSecret, RoleOperator, time.Hour)
	payload, err := VerifyToken(token, testKey, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Role != RoleOperator {
		t.Errorf("expected role operator, got %s", payload.Role)
	}
	if payload.Sub != testKey {
		t.Errorf("expected sub %s, got %s", testKey, payload.Sub)
	}
}

func TestVerifyToken_WrongSecret(t *testing.T) {
	token, _ := GenerateToken(testKey, testSecret, RoleAdmin, time.Hour)
	_, err := VerifyToken(token, testKey, "wrong-secret")
	if err == nil {
		t.Error("expected error for wrong secret")
	}
}

func TestVerifyToken_WrongKey(t *testing.T) {
	token, _ := GenerateToken(testKey, testSecret, RoleAdmin, time.Hour)
	_, err := VerifyToken(token, "wrong-key", testSecret)
	if err == nil {
		t.Error("expected error for wrong key")
	}
}

func TestVerifyToken_Expired(t *testing.T) {
	token, _ := GenerateToken(testKey, testSecret, RoleAdmin, -time.Hour)
	_, err := VerifyToken(token, testKey, testSecret)
	if err == nil {
		t.Error("expected error for expired token")
	}
}

func TestVerifyToken_Malformed(t *testing.T) {
	_, err := VerifyToken("not-a-token", testKey, testSecret)
	if err == nil {
		t.Error("expected error for malformed token")
	}
}

func TestVerifyToken_TamperedPayload(t *testing.T) {
	token, _ := GenerateToken(testKey, testSecret, RoleAdmin, time.Hour)
	// Tamper with the middle part
	parts := splitToken(token)
	parts[1] = parts[1] + "x"
	tampered := parts[0] + "." + parts[1] + "." + parts[2]
	_, err := VerifyToken(tampered, testKey, testSecret)
	if err == nil {
		t.Error("expected error for tampered payload")
	}
}

func splitToken(token string) [3]string {
	var result [3]string
	idx := 0
	for i, ch := range token {
		if ch == '.' {
			idx++
			if idx >= 3 {
				break
			}
			continue
		}
		result[idx] += string(token[i])
	}
	return result
}

// --- Middleware tests ---

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
}

func TestAuthMiddleware_Disabled(t *testing.T) {
	cfg := AuthConfig{Enabled: false}
	mw := AuthMiddleware(cfg, nil)
	handler := mw(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 when auth disabled, got %d", rr.Code)
	}
}

func TestAuthMiddleware_MissingToken(t *testing.T) {
	cfg := AuthConfig{Enabled: true, ApiKey: testKey, ApiSecret: testSecret}
	mw := AuthMiddleware(cfg, nil)
	handler := mw(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing token, got %d", rr.Code)
	}
}

func TestAuthMiddleware_ValidOperatorRead(t *testing.T) {
	cfg := AuthConfig{Enabled: true, ApiKey: testKey, ApiSecret: testSecret}
	writePaths := map[string]bool{"/kill": true, "/config": true}
	mw := AuthMiddleware(cfg, writePaths)
	handler := mw(okHandler())

	token, _ := GenerateToken(testKey, testSecret, RoleOperator, time.Hour)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for valid operator on GET, got %d", rr.Code)
	}
}

func TestAuthMiddleware_OperatorDeniedWrite(t *testing.T) {
	cfg := AuthConfig{Enabled: true, ApiKey: testKey, ApiSecret: testSecret}
	writePaths := map[string]bool{"/kill": true, "/config": true}
	mw := AuthMiddleware(cfg, writePaths)
	handler := mw(okHandler())

	token, _ := GenerateToken(testKey, testSecret, RoleOperator, time.Hour)
	req := httptest.NewRequest(http.MethodPost, "/kill", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403 for operator on write path, got %d", rr.Code)
	}
}

func TestAuthMiddleware_AdminAllowedWrite(t *testing.T) {
	cfg := AuthConfig{Enabled: true, ApiKey: testKey, ApiSecret: testSecret}
	writePaths := map[string]bool{"/kill": true, "/config": true}
	mw := AuthMiddleware(cfg, writePaths)
	handler := mw(okHandler())

	token, _ := GenerateToken(testKey, testSecret, RoleAdmin, time.Hour)
	req := httptest.NewRequest(http.MethodPost, "/kill", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for admin on write path, got %d", rr.Code)
	}
}

func TestAuthMiddleware_QueryParamToken(t *testing.T) {
	cfg := AuthConfig{Enabled: true, ApiKey: testKey, ApiSecret: testSecret}
	mw := AuthMiddleware(cfg, nil)
	handler := mw(okHandler())

	token, _ := GenerateToken(testKey, testSecret, RoleOperator, time.Hour)
	req := httptest.NewRequest(http.MethodGet, "/health?token="+token, nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 with query param token, got %d", rr.Code)
	}
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	cfg := AuthConfig{Enabled: true, ApiKey: testKey, ApiSecret: testSecret}
	mw := AuthMiddleware(cfg, nil)
	handler := mw(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer invalid.token.here")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid token, got %d", rr.Code)
	}
}
