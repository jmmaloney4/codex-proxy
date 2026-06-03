package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// jwtClaims holds the subset of OAuth JWT claims we rely on. ChatGPT access
// tokens are JWTs whose payload carries the canonical expiry (`exp`) and, under
// the OpenAI auth namespace, the account id that upstream expects in the
// chatgpt-account-id header. Decoding these from the token itself (rather than
// trusting separately-stored fields) is how the upstream codex-rs CLI behaves
// and keeps us from ever using a token whose stored metadata has drifted.
type jwtClaims struct {
	Exp    float64 `json:"exp"`
	OpenAI struct {
		ChatGPTAccountID string `json:"chatgpt_account_id"`
	} `json:"https://api.openai.com/auth"`
}

// parseJWTClaims decodes (without verifying the signature) the payload segment
// of a JWT. Returns ok=false when the token is not a well-formed JWT.
func parseJWTClaims(token string) (jwtClaims, bool) {
	var c jwtClaims
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return c, false
	}
	// JWT payloads are base64url-encoded, conventionally without padding.
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Tolerate tokens that carry padding anyway.
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return c, false
		}
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return c, false
	}
	return c, true
}

// AccessTokenExpiresAtMs returns the token's true expiry in epoch milliseconds,
// decoded from the JWT `exp` claim. When the token is not a decodable JWT (or
// carries no `exp`), it falls back to the supplied stored value so behavior is
// never worse than before.
func AccessTokenExpiresAtMs(accessToken string, fallbackMs int64) int64 {
	if c, ok := parseJWTClaims(accessToken); ok && c.Exp > 0 {
		return int64(c.Exp) * 1000
	}
	return fallbackMs
}

// AccountIDFromJWT returns the canonical chatgpt-account-id carried in the
// token's `https://api.openai.com/auth` claim, matching how the upstream
// codex-rs CLI derives it. Falls back to the supplied stored value when the
// claim is absent so a token without it still works.
func AccountIDFromJWT(accessToken, fallback string) string {
	if c, ok := parseJWTClaims(accessToken); ok && c.OpenAI.ChatGPTAccountID != "" {
		return c.OpenAI.ChatGPTAccountID
	}
	return fallback
}
