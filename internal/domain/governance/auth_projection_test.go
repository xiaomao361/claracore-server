package governance

import (
	"reflect"
	"testing"
	"time"
)

func TestAuthenticationProjectionsCannotCarryCredentialHashes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		typeValue any
		forbidden string
	}{
		{name: "token", typeValue: AuthToken{}, forbidden: "Hash"},
		{name: "user", typeValue: AuthUser{}, forbidden: "PasswordHash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := reflect.TypeOf(tc.typeValue).FieldByName(tc.forbidden); ok {
				t.Fatalf("authentication projection must not expose %s", tc.forbidden)
			}
		})
	}
}

func TestAuthTokenValidationMatchesBearerStateRules(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	expired := now
	revoked := now.Add(-time.Minute)
	if err := (AuthToken{}).Validate(now); err != nil {
		t.Fatalf("active token rejected: %v", err)
	}
	if err := (AuthToken{ExpiresAt: &expired}).Validate(now); err != ErrTokenExpired {
		t.Fatalf("expired token error=%v", err)
	}
	if err := (AuthToken{RevokedAt: &revoked}).Validate(now); err != ErrTokenRevoked {
		t.Fatalf("revoked token error=%v", err)
	}
}
