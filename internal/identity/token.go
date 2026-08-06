// Package identity provides per-user authentication and topic authorisation.
//
// BoltQ's original security model was a single shared API key: any client that
// knew it could publish to and consume from every queue. That is workable for
// a trusted backend fleet and unworkable the moment untrusted end-user devices
// connect directly, which is what a chat or dating app requires. This package
// replaces "one key, full access" with signed per-user tokens and an explicit
// capability grid.
package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	// ErrMalformedToken means the token is not a well-formed JWS.
	ErrMalformedToken = errors.New("identity: malformed token")
	// ErrBadSignature means the signature did not verify against any known key.
	ErrBadSignature = errors.New("identity: signature verification failed")
	// ErrTokenExpired means the token's exp claim is in the past.
	ErrTokenExpired = errors.New("identity: token expired")
	// ErrTokenNotYetValid means the token's nbf claim is in the future.
	ErrTokenNotYetValid = errors.New("identity: token not yet valid")
	// ErrUnknownKeyID means the token's kid does not match a configured key.
	ErrUnknownKeyID = errors.New("identity: unknown key id")
	// ErrNoSubject means the token carries no user identity.
	ErrNoSubject = errors.New("identity: token has no subject")
)

// Claims is the token payload.
//
// The field names follow RFC 7519 where a registered claim exists so that
// tokens minted by an existing auth service (Auth0, Firebase, a homegrown
// service) need no translation layer.
type Claims struct {
	Subject   string   `json:"sub"`           // user ID — the identity that owns messages
	DeviceID  string   `json:"did,omitempty"` // device instance; distinct cursors per device
	Tenant    string   `json:"tid,omitempty"` // tenant for multi-tenant deployments
	Scopes    []string `json:"scp,omitempty"` // coarse capability grants
	IssuedAt  int64    `json:"iat,omitempty"` // Unix seconds
	NotBefore int64    `json:"nbf,omitempty"` // Unix seconds
	ExpiresAt int64    `json:"exp,omitempty"` // Unix seconds
	Issuer    string   `json:"iss,omitempty"`
	TokenID   string   `json:"jti,omitempty"` // for revocation
}

// Scope constants. Scopes are coarse; fine-grained access is decided by the
// ACL, which can reference the authenticated user's own ID.
const (
	ScopePublish   = "publish"   // append to streams and publish to queues
	ScopeSubscribe = "subscribe" // read streams, subscribe to topics
	ScopeAdmin     = "admin"     // cluster and topic administration
	ScopePresence  = "presence"  // publish ephemeral presence/typing signals
)

// Principal is a verified identity attached to a connection.
type Principal struct {
	UserID    string
	DeviceID  string
	Tenant    string
	Scopes    map[string]bool
	ExpiresAt time.Time
	TokenID   string

	// Anonymous marks a connection authenticated by the legacy shared API key
	// rather than a user token. Such a connection has full access and is meant
	// for trusted backend services, never for end-user devices.
	Anonymous bool
}

// HasScope reports whether the principal holds a scope. An anonymous
// (shared-key) principal holds every scope.
func (p *Principal) HasScope(scope string) bool {
	if p == nil {
		return false
	}
	if p.Anonymous {
		return true
	}
	return p.Scopes[scope]
}

// Expired reports whether the principal's token has passed its expiry.
//
// Connections are long-lived — a mobile client may hold one open for hours —
// so expiry is re-checked on each authorisation decision rather than only at
// connect time. Otherwise a token would effectively never expire.
func (p *Principal) Expired(now time.Time) bool {
	if p == nil {
		return true
	}
	if p.Anonymous || p.ExpiresAt.IsZero() {
		return false
	}
	return now.After(p.ExpiresAt)
}

// String renders the principal for logs. It deliberately omits the token ID.
func (p *Principal) String() string {
	if p == nil {
		return "<nil>"
	}
	if p.Anonymous {
		return "shared-key"
	}
	if p.DeviceID != "" {
		return p.UserID + "/" + p.DeviceID
	}
	return p.UserID
}

// SigningKey is one HMAC key usable for verification.
type SigningKey struct {
	ID     string // matches the token's "kid" header
	Secret []byte
}

// Verifier validates signed tokens.
//
// Only HMAC-SHA256 is supported. Asymmetric algorithms would let BoltQ verify
// without holding a signing secret, which is preferable, but HS256 keeps this
// dependency-free; RS256 support belongs behind the same interface if needed.
// Critically, the "alg" header is not trusted: the algorithm is fixed at
// verification time, closing the alg=none and RS256-as-HS256 confusion attacks.
type Verifier struct {
	mu       sync.RWMutex
	keys     map[string]SigningKey
	fallback *SigningKey // used when a token carries no kid
	issuer   string
	leeway   time.Duration
	revoked  map[string]time.Time // jti -> expiry
}

// VerifierConfig configures token verification.
type VerifierConfig struct {
	// Keys are the accepted signing keys. More than one allows rotation: mint
	// with the new key while the old one still verifies outstanding tokens.
	Keys []SigningKey
	// Issuer, when set, is required to match the token's iss claim.
	Issuer string
	// Leeway tolerates clock skew between BoltQ and the token issuer.
	Leeway time.Duration
}

// NewVerifier builds a Verifier.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	if len(cfg.Keys) == 0 {
		return nil, errors.New("identity: at least one signing key is required")
	}
	if cfg.Leeway <= 0 {
		cfg.Leeway = 60 * time.Second
	}

	v := &Verifier{
		keys:    make(map[string]SigningKey, len(cfg.Keys)),
		issuer:  cfg.Issuer,
		leeway:  cfg.Leeway,
		revoked: make(map[string]time.Time),
	}
	for _, k := range cfg.Keys {
		if len(k.Secret) < 32 {
			return nil, fmt.Errorf("identity: signing key %q is %d bytes; 32 or more required", k.ID, len(k.Secret))
		}
		v.keys[k.ID] = k
	}
	first := cfg.Keys[0]
	v.fallback = &first
	return v, nil
}

// AddKey registers an additional verification key at runtime, enabling
// zero-downtime rotation.
func (v *Verifier) AddKey(k SigningKey) error {
	if len(k.Secret) < 32 {
		return fmt.Errorf("identity: signing key %q is too short", k.ID)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.keys[k.ID] = k
	return nil
}

// RemoveKey retires a verification key. Tokens signed with it stop verifying
// immediately, so retire only after the longest token lifetime has elapsed.
func (v *Verifier) RemoveKey(id string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.keys, id)
}

// Revoke blacklists a token ID until the given time. Revocation is per-node
// and in-memory; a deployment that needs cluster-wide revocation should
// replicate the call, or rely on short token lifetimes instead.
func (v *Verifier) Revoke(tokenID string, until time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.revoked[tokenID] = until
	// Opportunistically drop entries that have outlived their token.
	now := time.Now()
	for id, exp := range v.revoked {
		if now.After(exp) {
			delete(v.revoked, id)
		}
	}
}

// hmacSHA256 computes the JWS signature over an encoded header.claims string.
func hmacSHA256(secret, data []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	return mac.Sum(nil)
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ,omitempty"`
	Kid string `json:"kid,omitempty"`
}

// Verify parses and validates a compact JWS and returns the principal.
func (v *Verifier) Verify(token string) (*Principal, error) {
	return v.VerifyAt(token, time.Now())
}

// VerifyAt is Verify with an explicit clock, for testing.
func (v *Verifier) VerifyAt(token string, now time.Time) (*Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrMalformedToken
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header is not base64url", ErrMalformedToken)
	}
	var hdr jwtHeader
	if err := json.Unmarshal(headerJSON, &hdr); err != nil {
		return nil, fmt.Errorf("%w: header is not JSON", ErrMalformedToken)
	}

	// The algorithm is dictated by this server, not by the token. A token
	// claiming alg=none or a different family is rejected outright.
	if hdr.Alg != "HS256" {
		return nil, fmt.Errorf("%w: unsupported alg %q", ErrBadSignature, hdr.Alg)
	}

	v.mu.RLock()
	var key SigningKey
	if hdr.Kid != "" {
		k, ok := v.keys[hdr.Kid]
		if !ok {
			v.mu.RUnlock()
			return nil, fmt.Errorf("%w: %s", ErrUnknownKeyID, hdr.Kid)
		}
		key = k
	} else {
		key = *v.fallback
	}
	issuer, leeway := v.issuer, v.leeway
	v.mu.RUnlock()

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature is not base64url", ErrMalformedToken)
	}

	expected := hmacSHA256(key.Secret, []byte(parts[0]+"."+parts[1]))

	// Constant-time comparison: a timing-variable compare would leak the
	// signature byte by byte to an attacker able to submit many tokens.
	if subtle.ConstantTimeCompare(signature, expected) != 1 {
		return nil, ErrBadSignature
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: claims are not base64url", ErrMalformedToken)
	}
	var claims Claims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return nil, fmt.Errorf("%w: claims are not JSON", ErrMalformedToken)
	}

	if claims.Subject == "" {
		return nil, ErrNoSubject
	}
	if issuer != "" && claims.Issuer != issuer {
		return nil, fmt.Errorf("%w: issuer %q not accepted", ErrBadSignature, claims.Issuer)
	}
	if claims.ExpiresAt != 0 && now.After(time.Unix(claims.ExpiresAt, 0).Add(leeway)) {
		return nil, ErrTokenExpired
	}
	if claims.NotBefore != 0 && now.Before(time.Unix(claims.NotBefore, 0).Add(-leeway)) {
		return nil, ErrTokenNotYetValid
	}

	if claims.TokenID != "" {
		v.mu.RLock()
		until, revoked := v.revoked[claims.TokenID]
		v.mu.RUnlock()
		if revoked && now.Before(until) {
			return nil, fmt.Errorf("%w: token revoked", ErrBadSignature)
		}
	}

	scopes := make(map[string]bool, len(claims.Scopes))
	for _, s := range claims.Scopes {
		scopes[s] = true
	}
	// A token with no explicit scopes gets the two an end-user client needs.
	// Admin is never granted implicitly.
	if len(scopes) == 0 {
		scopes[ScopePublish] = true
		scopes[ScopeSubscribe] = true
		scopes[ScopePresence] = true
	}

	p := &Principal{
		UserID:   claims.Subject,
		DeviceID: claims.DeviceID,
		Tenant:   claims.Tenant,
		Scopes:   scopes,
		TokenID:  claims.TokenID,
	}
	if claims.ExpiresAt != 0 {
		p.ExpiresAt = time.Unix(claims.ExpiresAt, 0)
	}
	return p, nil
}

// Sign mints a token. BoltQ does not need to sign tokens in production — an
// application's auth service does — but tests, the CLI and local development
// all need a way to produce one, and shelling out to a separate tool for that
// is friction with no security benefit.
func Sign(key SigningKey, claims Claims) (string, error) {
	if len(key.Secret) < 32 {
		return "", fmt.Errorf("identity: signing key %q is too short", key.ID)
	}
	if claims.Subject == "" {
		return "", ErrNoSubject
	}

	hdr := jwtHeader{Alg: "HS256", Typ: "JWT", Kid: key.ID}
	hdrJSON, err := json.Marshal(hdr)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	signingInput := base64.RawURLEncoding.EncodeToString(hdrJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)

	sig := hmacSHA256(key.Secret, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
