package auth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// OAuthTokenURL is the endpoint for refreshing OAuth tokens
	OAuthTokenURL = "https://auth.openai.com/oauth/token"
	// ClientID is the OAuth client ID for ChatGPT/Codex
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// TokenExpiryBuffer is how early before a token's true expiry we refresh.
	// Kept small on purpose: every refresh rotates the refresh token upstream,
	// so refreshing far ahead of need (the old value was 60m) churns sessions
	// and increases the chance of an app_session_terminated. The on-demand path
	// in GetCredentials still refreshes synchronously if a request arrives
	// inside this window, so a tight buffer is safe.
	TokenExpiryBuffer = 10 * time.Minute
)

// refreshHTTPClient is used for token-refresh requests. It carries an explicit
// timeout because the refresh runs while OAuthFetcher holds its write lock; a
// hung upstream on the default (timeout-less) client would otherwise block every
// concurrent GetCredentials indefinitely.
var refreshHTTPClient = &http.Client{Timeout: 15 * time.Second}

// TokenExpired checks if the token is expired or will expire soon
func TokenExpired(expiresAtMs int64) bool {
	bufferMs := TokenExpiryBuffer.Milliseconds()
	currentTimeMs := time.Now().UnixMilli()
	return currentTimeMs >= (expiresAtMs - bufferMs)
}

// RefreshToken performs an OAuth token refresh and returns new credentials.
//
// The request is form-urlencoded with only grant_type/refresh_token/client_id
// and no scope, matching the upstream codex-rs CLI and the working hermes-agent
// implementation. The previous JSON body with an explicit "openid profile
// email" scope diverged from the real client; aligning it removes a variable in
// the recurring app_session_terminated failures.
func RefreshToken(refreshToken string) (*TokenRefreshResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", ClientID)

	resp, err := refreshHTTPClient.Post(OAuthTokenURL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to make refresh request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Bound the error-body read: this is an untrusted upstream response and
		// we only need enough of it to make the error message useful.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("token refresh failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp TokenRefreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("failed to decode refresh response: %w", err)
	}

	return &tokenResp, nil
}

// CalculateExpiresAt calculates the expiry timestamp from expires_in seconds
func CalculateExpiresAt(expiresIn int) int64 {
	return (time.Now().Unix() + int64(expiresIn)) * 1000 // Convert to milliseconds
}
