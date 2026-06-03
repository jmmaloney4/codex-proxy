package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// makeJWT builds an unsigned JWT (header.payload.signature) whose payload is the
// supplied claims map. The signature is irrelevant: we only decode the payload.
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	return header + "." + payload + ".sig"
}

func TestAccessTokenExpiresAtMs_FromJWT(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).Unix()
	token := makeJWT(t, map[string]any{"exp": exp})

	got := AccessTokenExpiresAtMs(token, 0)
	want := exp * 1000
	if got != want {
		t.Fatalf("expiresAt: got %d, want %d", got, want)
	}
}

func TestAccessTokenExpiresAtMs_FallbackForNonJWT(t *testing.T) {
	const fallback int64 = 1781330112000
	if got := AccessTokenExpiresAtMs("not-a-jwt", fallback); got != fallback {
		t.Fatalf("non-jwt: got %d, want fallback %d", got, fallback)
	}
	// JWT without an exp claim should also fall back.
	noExp := makeJWT(t, map[string]any{"foo": "bar"})
	if got := AccessTokenExpiresAtMs(noExp, fallback); got != fallback {
		t.Fatalf("jwt-without-exp: got %d, want fallback %d", got, fallback)
	}
}

func TestAccountIDFromJWT_FromClaim(t *testing.T) {
	const acct = "7eee5010-7dc5-4a13-8618-013749512845"
	token := makeJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": acct,
		},
	})
	if got := AccountIDFromJWT(token, "stored-id"); got != acct {
		t.Fatalf("accountID: got %q, want %q", got, acct)
	}
}

func TestAccountIDFromJWT_FallbackWhenMissing(t *testing.T) {
	const stored = "stored-id"
	// Non-JWT token.
	if got := AccountIDFromJWT("opaque-token", stored); got != stored {
		t.Fatalf("non-jwt: got %q, want %q", got, stored)
	}
	// JWT without the account-id claim.
	token := makeJWT(t, map[string]any{"exp": time.Now().Unix()})
	if got := AccountIDFromJWT(token, stored); got != stored {
		t.Fatalf("jwt-without-claim: got %q, want %q", got, stored)
	}
}
