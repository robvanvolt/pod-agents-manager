package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type authContext struct {
	Role string
}

type loginRateState struct {
	WindowStart time.Time
	Attempts    int
	Failures    int
	LastSeen    time.Time
}

type authState struct {
	BootstrapTokenSHA256           string              `json:"bootstrap_token_sha256"`
	CreatedAt                      string              `json:"created_at"`
	UpdatedAt                      string              `json:"updated_at"`
	Sessions                       []authSession       `json:"sessions"`
	PasskeyUserID                  string              `json:"passkey_user_id,omitempty"`
	Passkeys                       []passkeyCredential `json:"passkeys,omitempty"`
	PasskeyRegistrationChallenge   passkeyChallenge    `json:"passkey_registration_challenge,omitempty"`
	PasskeyAuthenticationChallenge passkeyChallenge    `json:"passkey_authentication_challenge,omitempty"`
}

type authSession struct {
	TokenSHA256 string `json:"token_sha256"`
	Role        string `json:"role"`
	CreatedAt   string `json:"created_at"`
	ExpiresAt   string `json:"expires_at"`
	RemoteAddr  string `json:"remote_addr,omitempty"`
	UserAgent   string `json:"user_agent,omitempty"`
}

func authFile(root string) string { return filepath.Join(root, "server", "auth.json") }

func auditFile(root string) string { return filepath.Join(root, "server", "audit.jsonl") }

func auditArchivePattern(root string) string {
	return filepath.Join(root, "server", "audit.*.jsonl")
}

func currentAuthContext(r *http.Request, root string) authContext {
	role := "viewer"
	session := sessionTokenFromRequest(r)
	if session == "" {
		return authContext{Role: role}
	}
	state, err := loadAuthState(root)
	if err != nil {
		return authContext{Role: role}
	}
	now := time.Now()
	hash := tokenHash(session)
	for _, s := range state.Sessions {
		expires, err := time.Parse(time.RFC3339, s.ExpiresAt)
		if err != nil || now.After(expires) {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(s.TokenSHA256), []byte(hash)) == 1 && s.Role == "operator" {
			role = "operator"
			break
		}
	}
	return authContext{Role: role}
}

func requireOperator(w http.ResponseWriter, r *http.Request, root, action, target string) (authContext, bool) {
	auth := currentAuthContext(r, root)
	if !requireSameOriginWrite(w, r, root, action, target, auth.Role) {
		return auth, false
	}
	if auth.Role == "operator" {
		return auth, true
	}
	appendAudit(root, auditEntryFromRequest(r, action, target, "denied", "operator role required", auth.Role))
	http.Error(w, "operator role required", http.StatusUnauthorized)
	return auth, false
}

func requireOperatorRead(w http.ResponseWriter, r *http.Request, root string) bool {
	if currentAuthContext(r, root).Role == "operator" {
		return true
	}
	http.Error(w, "operator role required", http.StatusUnauthorized)
	return false
}

func requireSameOriginWrite(w http.ResponseWriter, r *http.Request, root, action, target, role string) bool {
	if err := validateWriteOrigin(r); err != nil {
		appendAudit(root, auditEntryFromRequest(r, action, target, "csrf_blocked", err.Error(), role))
		http.Error(w, "cross-origin write blocked", http.StatusForbidden)
		return false
	}
	return true
}

func validateWriteOrigin(r *http.Request) error {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		fetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
		if fetchSite == "" || fetchSite == "same-origin" {
			return nil
		}
		return fmt.Errorf("missing Origin with Sec-Fetch-Site=%s", fetchSite)
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid Origin")
	}
	if originAllowedForRequest(u, r) {
		return nil
	}
	return fmt.Errorf("Origin %s does not match Host %s", u.Host, r.Host)
}

func originAllowedForRequest(origin *url.URL, r *http.Request) bool {
	if sameHostPort(origin.Host, r.Host) {
		return true
	}
	originHost, originPort := splitHostPortLoose(origin.Host)
	if originPort == "" {
		originPort = defaultPortForScheme(origin.Scheme)
	}
	_, requestPort := splitHostPortLoose(r.Host)
	if requestPort == "" {
		if r.TLS != nil {
			requestPort = "443"
		} else {
			requestPort = "80"
		}
	}
	if originPort != requestPort {
		return false
	}
	originHost = strings.Trim(strings.ToLower(originHost), "[]")
	for _, allowed := range append(localIPs(), "127.0.0.1", "localhost", "::1") {
		if originHost == strings.Trim(strings.ToLower(allowed), "[]") {
			return true
		}
	}
	if host, err := os.Hostname(); err == nil && host != "" && originHost == strings.ToLower(host) {
		return true
	}
	return false
}

func sameHostPort(a, b string) bool {
	aHost, aPort := splitHostPortLoose(a)
	bHost, bPort := splitHostPortLoose(b)
	return strings.EqualFold(strings.Trim(aHost, "[]"), strings.Trim(bHost, "[]")) && aPort == bPort
}

func splitHostPortLoose(hostport string) (string, string) {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return "", ""
	}
	if host, port, err := net.SplitHostPort(hostport); err == nil {
		return host, port
	}
	if strings.HasPrefix(hostport, "[") && strings.Contains(hostport, "]") {
		end := strings.Index(hostport, "]")
		host := hostport[1:end]
		rest := hostport[end+1:]
		if strings.HasPrefix(rest, ":") {
			return host, strings.TrimPrefix(rest, ":")
		}
		return host, ""
	}
	if strings.Count(hostport, ":") == 1 {
		parts := strings.SplitN(hostport, ":", 2)
		return parts[0], parts[1]
	}
	return hostport, ""
}

func defaultPortForScheme(scheme string) string {
	switch strings.ToLower(scheme) {
	case "https":
		return "443"
	default:
		return "80"
	}
}

func reserveLoginAttempt(r *http.Request) bool {
	ip := rateLimitIP(r)
	now := time.Now()

	loginRateMutex.Lock()
	defer loginRateMutex.Unlock()

	state := loginRateAttempts[ip]
	if state == nil || now.Sub(state.WindowStart) >= loginRateWindow {
		state = &loginRateState{WindowStart: now}
		loginRateAttempts[ip] = state
	}
	state.LastSeen = now
	if state.Attempts >= loginRateMaxAttempts {
		return false
	}
	state.Attempts++
	return true
}

func recordLoginFailure(r *http.Request) time.Duration {
	ip := rateLimitIP(r)
	now := time.Now()

	loginRateMutex.Lock()
	defer loginRateMutex.Unlock()

	state := loginRateAttempts[ip]
	if state == nil {
		state = &loginRateState{WindowStart: now}
		loginRateAttempts[ip] = state
	}
	state.Failures++
	state.LastSeen = now
	if state.Failures < 5 {
		return 0
	}
	exp := state.Failures - 5
	if exp > 4 {
		exp = 4
	}
	return time.Duration(1<<exp) * time.Second
}

func recordLoginSuccess(r *http.Request) {
	ip := rateLimitIP(r)
	loginRateMutex.Lock()
	defer loginRateMutex.Unlock()
	delete(loginRateAttempts, ip)
}

func readLoginToken(r *http.Request) (string, error) {
	contentType := r.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") {
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
			return "", fmt.Errorf("invalid JSON body")
		}
		body.Token = strings.TrimSpace(body.Token)
		if body.Token == "" {
			return "", fmt.Errorf("token is required")
		}
		return body.Token, nil
	}
	if err := r.ParseForm(); err != nil {
		return "", fmt.Errorf("bad form data")
	}
	token := strings.TrimSpace(r.FormValue("token"))
	if token == "" {
		return "", fmt.Errorf("token is required")
	}
	return token, nil
}

func verifyDashboardTokenAndCreateSession(root, token string, r *http.Request) (string, error) {
	authMutex.Lock()
	defer authMutex.Unlock()

	state, err := loadOrInitAuthState(root)
	if err != nil {
		return "", err
	}
	if err := verifyDashboardToken(state, token); err != nil {
		return "", err
	}
	session, err := randomToken(32)
	if err != nil {
		return "", err
	}
	now := time.Now()
	state.Sessions = compactSessions(state.Sessions, now)
	state.Sessions = append(state.Sessions, authSession{
		TokenSHA256: tokenHash(session),
		Role:        "operator",
		CreatedAt:   now.Format(time.RFC3339),
		ExpiresAt:   now.Add(operatorSessionTTL).Format(time.RFC3339),
		RemoteAddr:  clientIP(r),
		UserAgent:   r.UserAgent(),
	})
	state.UpdatedAt = now.Format(time.RFC3339)
	if err := saveAuthState(root, state); err != nil {
		return "", err
	}
	return session, nil
}

func verifyDashboardToken(state authState, token string) error {
	hash := tokenHash(token)
	configured := false
	if apiKey := strings.TrimSpace(os.Getenv("POD_SERVER_API_KEY")); apiKey != "" {
		configured = true
		if subtle.ConstantTimeCompare([]byte(tokenHash(apiKey)), []byte(hash)) == 1 {
			return nil
		}
	}
	if state.BootstrapTokenSHA256 != "" {
		configured = true
		if subtle.ConstantTimeCompare([]byte(state.BootstrapTokenSHA256), []byte(hash)) == 1 {
			return nil
		}
	}
	if !configured {
		return fmt.Errorf("dashboard API key not configured; set POD_SERVER_API_KEY or run `pod server token rotate`")
	}
	return fmt.Errorf("invalid dashboard token")
}

// Legacy bootstrap-token callers share the dashboard unlock path while rotated
// bootstrap tokens remain accepted for existing installs.
func verifyBootstrapTokenAndCreateSession(root, token string, r *http.Request) (string, error) {
	return verifyDashboardTokenAndCreateSession(root, token, r)
}

func revokeSession(root, session string) {
	authMutex.Lock()
	defer authMutex.Unlock()

	state, err := loadAuthState(root)
	if err != nil {
		return
	}
	hash := tokenHash(session)
	kept := []authSession{}
	for _, s := range state.Sessions {
		if subtle.ConstantTimeCompare([]byte(s.TokenSHA256), []byte(hash)) == 1 {
			continue
		}
		kept = append(kept, s)
	}
	state.Sessions = compactSessions(kept, time.Now())
	state.UpdatedAt = time.Now().Format(time.RFC3339)
	_ = saveAuthState(root, state)
}

func compactSessions(sessions []authSession, now time.Time) []authSession {
	out := []authSession{}
	for _, s := range sessions {
		expires, err := time.Parse(time.RFC3339, s.ExpiresAt)
		if err == nil && now.Before(expires) {
			out = append(out, s)
		}
	}
	return out
}

func loadAuthState(root string) (authState, error) {
	var state authState
	data, err := os.ReadFile(authFile(root))
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	return state, nil
}

func loadOrInitAuthState(root string) (authState, error) {
	state, err := loadAuthState(root)
	if err == nil {
		return state, nil
	}
	if !os.IsNotExist(err) {
		return state, err
	}
	now := time.Now().Format(time.RFC3339)
	return authState{
		CreatedAt: now,
		UpdatedAt: now,
		Sessions:  []authSession{},
	}, nil
}

func saveAuthState(root string, state authState) error {
	path := authFile(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func expectedOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func randomToken(bytesLen int) (string, error) {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func sessionTokenFromRequest(r *http.Request) string {
	if cookie, err := r.Cookie("pod_session"); err == nil {
		return cookie.Value
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "pod_session",
		Value:    token,
		Path:     "/",
		MaxAge:   int(operatorSessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   secureCookie(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "pod_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secureCookie(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func secureCookie(r *http.Request) bool {
	return r.TLS != nil || os.Getenv("POD_SERVER_FORCE_SECURE_COOKIE") == "1"
}

func clientIP(r *http.Request) string {
	forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if forwarded != "" {
		return strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func rateLimitIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
