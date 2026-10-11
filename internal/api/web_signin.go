package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/config"
)

const (
	webAuthPath         = "/api/daemon/web-auth"
	webLoginPath        = webAuthPath + "/login"
	webAuthSessionsPath = webAuthPath + "/sessions"
	// maxWebLoginBody fits a 4096-byte key with every byte JSON-escaped as
	// \uXXXX (6 bytes each) plus the object wrapper.
	maxWebLoginBody = 32 << 10
)

type webLoginPolicy struct {
	origin     string
	host       string
	hosts      map[string]bool // nil unless public_origin enables the Host gate
	cookie     string
	cookieHash [sha256.Size]byte
	keyHash    [sha256.Size]byte
	lifetime   time.Duration
}

type webSignInRequest struct {
	APIKey string `json:"api_key"`
}
type webTabSession struct {
	Token        string `json:"token"`
	UploadSecret string `json:"upload_secret"`
}
type webSignInStatus struct {
	Enabled bool           `json:"enabled"`
	Session *webTabSession `json:"session,omitempty"`
}

// WebSignInRecord identifies a scoped login session without bearer credentials.
type WebSignInRecord struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}
type webAuthSessionList struct {
	Items []WebSignInRecord `json:"items"`
}

func newWebLoginPolicy(d Deps) *webLoginPolicy {
	if !d.WebLoginEnabled || !d.Cfg.Web.Enabled || d.WebURL == "" {
		return nil
	}
	origin := d.Cfg.Web.PublicOrigin
	if origin == "" {
		origin = strings.TrimSuffix(d.WebURL, "/")
	}
	origin, err := config.CanonicalWebOrigin(origin)
	if err != nil {
		panic("api: invalid web login origin")
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic("api: could not generate browser cookie")
	}
	cookie := base64.RawURLEncoding.EncodeToString(random[:])
	parsed, _ := url.Parse(origin)
	policy := &webLoginPolicy{
		origin:     origin,
		host:       parsed.Host,
		cookie:     cookie,
		cookieHash: sha256.Sum256([]byte(cookie)),
		keyHash:    sha256.Sum256([]byte(d.Cfg.Server.APIKey)),
		lifetime:   d.Cfg.Web.WebSessionLifetime(),
	}
	if d.Cfg.Web.PublicOrigin == "" {
		return policy
	}
	policy.hosts = map[string]bool{strings.ToLower(parsed.Host): true}
	if browser, err := url.Parse(d.WebURL); err == nil {
		policy.hosts[strings.ToLower(browser.Host)] = true
	}
	if d.APIAddress != "" {
		policy.hosts[strings.ToLower(d.APIAddress)] = true
	}
	for _, host := range d.Cfg.Web.AllowedHosts {
		authority, err := config.WebHostAuthority(host)
		if err != nil {
			panic("api: invalid allowed web host")
		}
		policy.hosts[authority] = true
	}
	return policy
}

// signInHosts lists the authorities that public_origin adds to the server
// Host allowlist. Without public_origin, the server allowlist alone applies.
func (p *webLoginPolicy) signInHosts() []string {
	if p == nil {
		return nil
	}
	hosts := make([]string, 0, len(p.hosts))
	for host := range p.hosts {
		hosts = append(hosts, host)
	}
	return hosts
}

func (r *webSessionRegistry) issueLogin() (*webTabSession, error) {
	token, secret, err := r.issue()
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(token))
	r.mu.Lock()
	state, ok := r.tokens[digest]
	if !ok || r.closing {
		r.mu.Unlock()
		return nil, errors.New("browser sessions are shutting down")
	}
	count := 0
	for _, existing := range r.tokens {
		if existing.login {
			count++
		}
	}
	if count >= 512 {
		r.mu.Unlock()
		r.revoke(token)
		return nil, errors.New("browser key-login capacity reached")
	}
	state.login = true
	state.createdAt = time.Now().UTC()
	state.expiresAt = state.createdAt.Add(r.login.lifetime)
	state.expires = time.AfterFunc(r.login.lifetime, func() { r.revokeDigest(digest) })
	r.tokens[digest] = state
	r.mu.Unlock()
	return &webTabSession{Token: token, UploadSecret: secret}, nil
}

func webCookieName(p *webLoginPolicy) string {
	// Browser cookies do not isolate by port, so keep same-host daemon origins
	// from replacing one another's shared instance cookie.
	digest := sha256.Sum256([]byte(p.origin))
	name := "docbank_session_" + hex.EncodeToString(digest[:])
	if strings.HasPrefix(p.origin, "https://") {
		return "__Host-" + name
	}
	return name
}

func webSameOrigin(req *http.Request, p *webLoginPolicy, mutation bool) bool {
	if req.Host != p.host || req.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := req.Header.Get("Origin")
	return origin == p.origin || !mutation && origin == ""
}

func (r *webSessionRegistry) authorizeBrowser(req *http.Request, token string) bool {
	r.mu.Lock()
	state, ok := r.tokens[sha256.Sum256([]byte(token))]
	r.mu.Unlock()
	if !ok {
		return false
	}
	if !state.login {
		return true
	}
	mutation := req.Method != http.MethodGet && req.Method != http.MethodHead
	if r.login == nil || !time.Now().Before(state.expiresAt) || !webSameOrigin(req, r.login, mutation) {
		return false
	}
	cookies := req.CookiesNamed(webCookieName(r.login))
	if len(cookies) != 1 {
		return false
	}
	cookieHash := sha256.Sum256([]byte(cookies[0].Value))
	return subtle.ConstantTimeCompare(cookieHash[:], r.login.cookieHash[:]) == 1
}

func (r *webSessionRegistry) loginRecords() []WebSignInRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	records := []WebSignInRecord{}
	for digest, state := range r.tokens {
		if state.login && time.Now().Before(state.expiresAt) {
			records = append(records, WebSignInRecord{
				ID: hex.EncodeToString(digest[:]), CreatedAt: state.createdAt, ExpiresAt: state.expiresAt,
			})
		}
	}
	slices.SortFunc(records, func(a, b WebSignInRecord) int { return strings.Compare(a.ID, b.ID) })
	return records
}

func registerWebSignIn(mux *http.ServeMux, sessions *webSessionRegistry) {
	mux.HandleFunc("GET "+webAuthPath, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		policy := sessions.login
		// Other allowed hosts serve the app for fragment sessions; key login is
		// offered only at the sign-in origin.
		enabled := policy != nil && webSameOrigin(req, policy, false)
		writeJSON(w, http.StatusOK, webSignInStatus{Enabled: enabled})
	})
	mux.HandleFunc("POST "+webLoginPath, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		policy := sessions.login
		if policy == nil {
			writeError(w, NewError(http.StatusServiceUnavailable, "web_unavailable", "browser key login is unavailable"))
			return
		}
		if !webSameOrigin(req, policy, true) {
			writeError(w, NewError(http.StatusForbidden, "web_origin", "browser Host or Origin rejected"))
			return
		}
		media, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
		if err != nil || media != jsonMediaType {
			writeError(w, NewError(http.StatusUnsupportedMediaType, "content_type", "login requires application/json"))
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxWebLoginBody))
		var input webSignInRequest
		if err != nil || json.Unmarshal(raw, &input, json.RejectUnknownMembers(true)) != nil {
			writeError(w, NewError(http.StatusBadRequest, "bad_request", "invalid login request"))
			return
		}
		keyHash := sha256.Sum256([]byte(input.APIKey))
		if input.APIKey == "" || subtle.ConstantTimeCompare(keyHash[:], policy.keyHash[:]) != 1 {
			writeError(w, NewError(http.StatusUnauthorized, "unauthorized", "invalid API key"))
			return
		}
		tab, err := sessions.issueLogin()
		if err != nil {
			writeError(w, NewError(http.StatusServiceUnavailable, "web_unavailable", "could not create a browser session"))
			return
		}
		// The instance cookie is shared by tabs. Login/logout never rotates or
		// deletes it; each tab credential carries independent authority.
		// HTTP uses loopback or the operator's explicit private-network trust;
		// HTTPS always uses Secure.
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure follows the origin scheme, as above.
			Name: webCookieName(policy), Value: policy.cookie, Path: "/", HttpOnly: true,
			Secure: strings.HasPrefix(policy.origin, "https://"), SameSite: http.SameSiteStrictMode,
		})
		writeJSON(w, http.StatusOK, webSignInStatus{Enabled: true, Session: tab})
	})
	mux.HandleFunc("GET "+webAuthSessionsPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, webAuthSessionList{Items: sessions.loginRecords()})
	})
	mux.HandleFunc("DELETE "+webAuthSessionsPath+"/{id}", func(w http.ResponseWriter, req *http.Request) {
		id := req.PathValue("id")
		if len(id) != 2*sha256.Size {
			writeError(w, NewError(http.StatusBadRequest, "bad_request", "invalid browser session ID"))
			return
		}
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != sha256.Size || id != strings.ToLower(id) {
			writeError(w, NewError(http.StatusBadRequest, "bad_request", "invalid browser session ID"))
			return
		}
		var digest [sha256.Size]byte
		copy(digest[:], decoded)
		sessions.revokeDigest(digest)
		w.WriteHeader(http.StatusNoContent)
	})
}

func webHostMiddleware(next http.Handler, policy *webLoginPolicy) http.Handler {
	if policy == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if policy.hosts != nil && !policy.hosts[strings.ToLower(req.Host)] {
			writeError(w, NewError(http.StatusForbidden, "web_host", "request Host rejected"))
			return
		}
		if !authExempt(req.URL.Path) {
			w = &webPrivateResponse{ResponseWriter: w}
		}
		next.ServeHTTP(w, req)
	})
}

type webPrivateResponse struct {
	http.ResponseWriter

	written bool
}

func (w *webPrivateResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *webPrivateResponse) WriteHeader(code int) {
	if !w.written {
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Add("Vary", "Cookie, "+WebSessionHeader)
		w.written = true
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *webPrivateResponse) Write(raw []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(raw)
}

// Preserve streaming responses when the Host policy wraps the shared listener.
func (w *webPrivateResponse) Flush() {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
