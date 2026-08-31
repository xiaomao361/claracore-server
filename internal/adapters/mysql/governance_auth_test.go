package mysql

import (
	"strings"
	"testing"
)

func TestAuthenticationQueryUsesCredentialOnlyAsLookupPredicate(t *testing.T) {
	parts := strings.SplitN(findAuthContextSQL, "FROM tokens t", 2)
	if len(parts) != 2 {
		t.Fatal("authentication query no longer has the expected projection boundary")
	}
	projection := strings.ToLower(parts[0])
	for _, forbidden := range []string{"token_hash", "password_hash"} {
		if strings.Contains(projection, forbidden) {
			t.Fatalf("authentication result must not select %s", forbidden)
		}
	}
	if !strings.Contains(strings.ToLower(parts[1]), "where t.token_hash = ?") {
		t.Fatal("authentication must still use the presented token hash as an indexed lookup predicate")
	}
}
