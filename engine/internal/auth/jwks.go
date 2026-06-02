// Package auth implements JWKS caching and JWT validation for BasuyuDB.
package auth

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jws"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

// SessionClaims holds validated JWT claims extracted for a connection.
type SessionClaims struct {
	Sub string
	Iss string
	Jti string
	Role string // "user" | "admin" | "service"
	NamespaceAccess []string // array of namespace UUIDs (or ["*"])
	NamespaceID string // derived: namespace_access[0] when len==1, else ""
	Email string // may be empty
	Azp string // may be empty
}

// JWKSCache caches the JWKS key set with automatic background refresh.
type JWKSCache struct {
	url string
	ttl time.Duration
	logger *slog.Logger
	httpClient *http.Client

	mu sync.RWMutex
	keySet jwk.Set
	fetchedAt time.Time
	stale bool

	sfGroup singleflightGroup

	stopCh chan struct{}
	ageUnix atomic.Int64 // unix timestamp of last successful fetch
}

// NewJWKSCache creates and initialises a JWKSCache, performing an initial JWKS
// fetch. Returns an error if the JWKS URL is missing or unreachable.
func NewJWKSCache(logger *slog.Logger) (*JWKSCache, error) {
	jwksURL := firstEnv("BASUYUDB_JWKS_URL", "PASSPORTAUTH_JWKS_URL")
	if jwksURL == "" {
		return nil, fmt.Errorf("BASUYUDB_JWKS_URL is required but not set")
	}

	ttlSec := envIntOr("BASUYUDB_JWKS_CACHE_TTL_SECONDS", 300)
	ttl := time.Duration(ttlSec) * time.Second

	c := &JWKSCache{
		url: jwksURL,
		ttl: ttl,
		logger: logger,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion: tls.VersionTLS12,
				},
			},
		},
		stopCh: make(chan struct{}),
	}

	if err := c.fetchAndCache(); err != nil {
		return nil, fmt.Errorf("initial JWKS fetch from %s: %w", jwksURL, err)
	}

	go c.backgroundRefresh()
	return c, nil
}

// Stop signals the background refresh goroutine to exit.
func (c *JWKSCache) Stop() {
	close(c.stopCh)
}

// CacheAgeSeconds returns the age of the JWKS cache in seconds (for Prometheus metrics).
func (c *JWKSCache) CacheAgeSeconds() float64 {
	last := c.ageUnix.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.Unix(last, 0)).Seconds()
}

func (c *JWKSCache) backgroundRefresh() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.mu.RLock()
			age := time.Since(c.fetchedAt)
			c.mu.RUnlock()

			if age >= c.ttl-60*time.Second {
				if err := c.fetchAndCache(); err != nil {
					c.logger.Warn("JWKS background refresh failed",
						"err", err,
						"cache_age_seconds", int(age.Seconds()),
					)
					if age > 30*time.Minute {
						c.logger.Error("JWKS cache stale > 30 min — all new auth will fail",
							"cache_age_seconds", int(age.Seconds()),
						)
						c.mu.Lock()
						c.stale = true
						c.mu.Unlock()
					}
				}
			}
		}
	}
}

func (c *JWKSCache) fetchAndCache() error {
	_, err := c.sfGroup.Do("jwks", func() (interface{}, error) {
		return nil, c.doFetch()
	})
	return err
}

func (c *JWKSCache) doFetch() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP GET %s: %w", c.url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS endpoint returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return fmt.Errorf("read JWKS response: %w", err)
	}

	keySet, err := jwk.ParseString(string(body))
	if err != nil {
		return fmt.Errorf("parse JWKS JSON: %w", err)
	}

	c.mu.Lock()
	c.keySet = keySet
	c.fetchedAt = time.Now()
	c.stale = false
	c.mu.Unlock()
	c.ageUnix.Store(time.Now().Unix())

	c.logger.Info("JWKS cache refreshed", "key_count", keySet.Len())
	return nil
}

// ValidateToken validates a JWT token string and returns SessionClaims.
// Security: algorithm allowlist is checked before signature verification.
func (c *JWKSCache) ValidateToken(tokenStr string) (*SessionClaims, error) {
	// Enforce algorithm allowlist FIRST — before any crypto operations.
	if err := enforceAlgorithmAllowlist(tokenStr); err != nil {
		return nil, err
	}

	c.mu.RLock()
	ks := c.keySet
	stale := c.stale
	c.mu.RUnlock()

	if ks == nil {
		return nil, fmt.Errorf("JWKS cache not initialised")
	}
	if stale {
		return nil, fmt.Errorf("JWKS cache is stale — cannot validate tokens safely")
	}

	token, err := jwt.Parse(
		[]byte(tokenStr),
		jwt.WithKeySet(ks, jws.WithRequireKid(false)),
		jwt.WithValidate(true),
		jwt.WithAcceptableSkew(30*time.Second),
		jwt.WithAudience("basuyudb"),
	)
	if err != nil {
		// On key-not-found, do one immediate refresh and retry.
		if isKeyNotFoundError(err) {
			c.logger.Info("JWKS key not found — triggering immediate refresh")
			if refreshErr := c.fetchAndCache(); refreshErr != nil {
				return nil, fmt.Errorf("JWT validation failed and JWKS refresh failed: %w", err)
			}
			c.mu.RLock()
			ks = c.keySet
			c.mu.RUnlock()
			token, err = jwt.Parse(
				[]byte(tokenStr),
				jwt.WithKeySet(ks, jws.WithRequireKid(false)),
				jwt.WithValidate(true),
				jwt.WithAcceptableSkew(30*time.Second),
				jwt.WithAudience("basuyudb"),
			)
			if err != nil {
				return nil, fmt.Errorf("JWT validation failed after JWKS refresh: %w", err)
			}
		} else {
			return nil, fmt.Errorf("JWT validation: %w", err)
		}
	}

	return extractClaims(token)
}

// enforceAlgorithmAllowlist parses the JWT header and rejects any algorithm
// not in [RS256, ES256]. This is checked before signature verification.
func enforceAlgorithmAllowlist(tokenStr string) error {
	parts := strings.SplitN(tokenStr, ".", 3)
	if len(parts) < 2 {
		return fmt.Errorf("malformed JWT: expected at least 2 dot-separated parts")
	}

	// Base64URL decode the header (add padding if needed).
	headerB64 := parts[0]
	switch len(headerB64) % 4 {
	case 2:
		headerB64 += "=="
	case 3:
		headerB64 += "="
	}
	headerB64 = strings.NewReplacer("-", "+", "_", "/").Replace(headerB64)

	headerBytes, err := base64.StdEncoding.DecodeString(headerB64)
	if err != nil {
		return fmt.Errorf("decode JWT header: %w", err)
	}

	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return fmt.Errorf("parse JWT header JSON: %w", err)
	}

	switch jwa.SignatureAlgorithm(header.Alg) {
	case jwa.RS256, jwa.ES256:
		// Allowed.
		return nil
	default:
		return fmt.Errorf("JWT algorithm %q is not allowed; BasuyuDB accepts only RS256 and ES256", header.Alg)
	}
}

func extractClaims(token jwt.Token) (*SessionClaims, error) {
	sub := token.Subject()
	if sub == "" {
		return nil, fmt.Errorf("JWT missing required 'sub' claim")
	}

	jtiVal, _ := token.Get("jti")
	jti, _ := jtiVal.(string)
	if jti == "" {
		return nil, fmt.Errorf("JWT missing required 'jti' claim")
	}

	roleVal, _ := token.Get("role")
	role, _ := roleVal.(string)
	switch role {
	case "user", "admin", "service":
		// valid
	default:
		return nil, fmt.Errorf("JWT 'role' claim must be 'user', 'admin', or 'service'; got %q", role)
	}

	nsAccessVal, ok := token.Get("namespace_access")
	if !ok {
		return nil, fmt.Errorf("JWT missing required 'namespace_access' claim")
	}
	nsAccess, err := toStringSlice(nsAccessVal)
	if err != nil {
		return nil, fmt.Errorf("JWT 'namespace_access' must be a string array: %w", err)
	}

	if len(nsAccess) == 1 && nsAccess[0] == "*" && role != "service" {
		return nil, fmt.Errorf("namespace_access=[\"*\"] is only valid for role='service'")
	}

	emailVal, _ := token.Get("email")
	email, _ := emailVal.(string)

	azpVal, _ := token.Get("azp")
	azp, _ := azpVal.(string)

	// Derive namespace_id per JWT_SESSION_VARIABLES.md spec.
	var nsID string
	if len(nsAccess) == 1 {
		nsID = nsAccess[0]
	}

	return &SessionClaims{
		Sub: sub,
		Iss: token.Issuer(),
		Jti: jti,
		Role: role,
		NamespaceAccess: nsAccess,
		NamespaceID: nsID,
		Email: email,
		Azp: azp,
	}, nil
}

func toStringSlice(v interface{}) ([]string, error) {
	switch tv := v.(type) {
	case []interface{}:
		result := make([]string, 0, len(tv))
		for _, item := range tv {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("non-string element: %T", item)
			}
			result = append(result, s)
		}
		return result, nil
	case []string:
		return tv, nil
	default:
		return nil, fmt.Errorf("expected []string, got %T", v)
	}
}

func isKeyNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "failed to find key") ||
		strings.Contains(msg, "key not found") ||
		strings.Contains(msg, "no matching key") ||
		strings.Contains(msg, "could not find")
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func envIntOr(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// ─── Minimal singleflight ─────────────────────────────────────────────────────

type singleflightGroup struct {
	mu sync.Mutex
	m map[string]*sfCall
}

type sfCall struct {
	wg sync.WaitGroup
	val interface{}
	err error
}

func (g *singleflightGroup) Do(key string, fn func() (interface{}, error)) (interface{}, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*sfCall)
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &sfCall{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()

	return c.val, c.err
}
