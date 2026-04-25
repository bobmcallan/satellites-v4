package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTokenExchange_SignedInUserMintsBearer(t *testing.T) {
	t.Parallel()
	users := NewMemoryUserStore()
	sessions := NewMemorySessionStore()
	user := User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, DefaultSessionTTL)

	v := NewBearerValidator(BearerValidatorConfig{CacheTTL: time.Minute})
	te := &TokenExchange{Sessions: sessions, Users: users, Validator: v}
	mux := http.NewServeMux()
	te.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/auth/token/exchange", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
		TokenType string `json:"token_type"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(body.Token, "sat_") {
		t.Errorf("token = %q, want sat_ prefix", body.Token)
	}
	if body.TokenType != "Bearer" {
		t.Errorf("token_type = %q", body.TokenType)
	}
	if body.ExpiresIn <= 0 {
		t.Errorf("expires_in = %d", body.ExpiresIn)
	}

	// The minted token should validate.
	info, err := v.Validate(req.Context(), body.Token)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if info.UserID != "u_alice" {
		t.Errorf("UserID = %q", info.UserID)
	}
}

func TestTokenExchange_NoSession_401(t *testing.T) {
	t.Parallel()
	te := &TokenExchange{
		Sessions:  NewMemorySessionStore(),
		Users:     NewMemoryUserStore(),
		Validator: NewBearerValidator(BearerValidatorConfig{}),
	}
	mux := http.NewServeMux()
	te.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/auth/token/exchange", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
