package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

var testKey = SigningKey{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}

func mustVerifier(t *testing.T, keys ...SigningKey) *Verifier {
	t.Helper()
	if len(keys) == 0 {
		keys = []SigningKey{testKey}
	}
	v, err := NewVerifier(VerifierConfig{Keys: keys})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	return v
}

func mustSign(t *testing.T, claims Claims) string {
	t.Helper()
	tok, err := Sign(testKey, claims)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}

func TestSignVerifyRoundTrip(t *testing.T) {
	v := mustVerifier(t)
	tok := mustSign(t, Claims{
		Subject:   "user-42",
		DeviceID:  "phone-1",
		Tenant:    "tenant-a",
		Scopes:    []string{ScopePublish, ScopeSubscribe},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})

	p, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if p.UserID != "user-42" || p.DeviceID != "phone-1" || p.Tenant != "tenant-a" {
		t.Errorf("principal mismatch: %+v", p)
	}
	if !p.HasScope(ScopePublish) || !p.HasScope(ScopeSubscribe) {
		t.Error("scopes not carried through")
	}
	if p.HasScope(ScopeAdmin) {
		t.Error("admin scope granted without being in the token")
	}
}

func TestVerifyRejectsTamperedClaims(t *testing.T) {
	v := mustVerifier(t)
	tok := mustSign(t, Claims{Subject: "user-1", ExpiresAt: time.Now().Add(time.Hour).Unix()})

	parts := strings.Split(tok, ".")
	var claims Claims
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	json.Unmarshal(raw, &claims)

	// Escalate to another user and re-encode, keeping the original signature.
	claims.Subject = "victim"
	claims.Scopes = []string{ScopeAdmin}
	newClaims, _ := json.Marshal(claims)
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString(newClaims) + "." + parts[2]

	if _, err := v.Verify(forged); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered token verified or wrong error: %v", err)
	}
}

func TestVerifyRejectsAlgNone(t *testing.T) {
	v := mustVerifier(t)

	// The classic JWT bypass: claim there is no signature algorithm.
	hdr, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	claims, _ := json.Marshal(Claims{Subject: "attacker", Scopes: []string{ScopeAdmin}})
	tok := base64.RawURLEncoding.EncodeToString(hdr) + "." +
		base64.RawURLEncoding.EncodeToString(claims) + "."

	if _, err := v.Verify(tok); err == nil {
		t.Fatal("alg=none token was accepted")
	}
}

func TestVerifyRejectsUnexpectedAlg(t *testing.T) {
	v := mustVerifier(t)
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1"})
	claims, _ := json.Marshal(Claims{Subject: "attacker"})
	tok := base64.RawURLEncoding.EncodeToString(hdr) + "." +
		base64.RawURLEncoding.EncodeToString(claims) + ".AAAA"

	if _, err := v.Verify(tok); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("RS256 header accepted or wrong error: %v", err)
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	other := SigningKey{ID: "k1", Secret: []byte("ffffffffffffffffffffffffffffffff")}
	tok, _ := Sign(other, Claims{Subject: "user-1"})

	v := mustVerifier(t)
	if _, err := v.Verify(tok); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("token signed with a foreign key verified: %v", err)
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	v := mustVerifier(t)
	for _, tok := range []string{"", "abc", "a.b", "a.b.c.d", "!!!.???.###"} {
		if _, err := v.Verify(tok); err == nil {
			t.Errorf("malformed token %q was accepted", tok)
		}
	}
}

func TestVerifyExpiry(t *testing.T) {
	v := mustVerifier(t)
	now := time.Now()

	expired := mustSign(t, Claims{Subject: "u1", ExpiresAt: now.Add(-2 * time.Hour).Unix()})
	if _, err := v.VerifyAt(expired, now); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("expired token: got %v", err)
	}

	future := mustSign(t, Claims{Subject: "u1", NotBefore: now.Add(2 * time.Hour).Unix()})
	if _, err := v.VerifyAt(future, now); !errors.Is(err, ErrTokenNotYetValid) {
		t.Errorf("not-yet-valid token: got %v", err)
	}

	// Just-expired tokens are tolerated within the leeway window.
	fresh := mustSign(t, Claims{Subject: "u1", ExpiresAt: now.Add(-10 * time.Second).Unix()})
	if _, err := v.VerifyAt(fresh, now); err != nil {
		t.Errorf("token inside the clock-skew leeway rejected: %v", err)
	}
}

func TestVerifyRequiresSubject(t *testing.T) {
	v := mustVerifier(t)
	// Sign() refuses an empty subject, so build the token by hand.
	hdr, _ := json.Marshal(jwtHeader{Alg: "HS256", Kid: "k1"})
	claims, _ := json.Marshal(Claims{Scopes: []string{ScopePublish}})
	input := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(claims)

	tok, err := signRaw(testKey, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(tok); !errors.Is(err, ErrNoSubject) {
		t.Errorf("subject-less token: got %v", err)
	}
}

// signRaw signs an already-encoded header.claims string.
func signRaw(key SigningKey, input string) (string, error) {
	tmp, err := Sign(key, Claims{Subject: "x"})
	if err != nil {
		return "", err
	}
	_ = tmp
	// Recompute the MAC over the supplied input using the same primitive.
	h := hmacSHA256(key.Secret, []byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(h), nil
}

func TestIssuerEnforcement(t *testing.T) {
	v, err := NewVerifier(VerifierConfig{Keys: []SigningKey{testKey}, Issuer: "auth.example.com"})
	if err != nil {
		t.Fatal(err)
	}

	wrong := mustSign(t, Claims{Subject: "u1", Issuer: "evil.example.com"})
	if _, err := v.Verify(wrong); err == nil {
		t.Error("token from an unexpected issuer was accepted")
	}

	right := mustSign(t, Claims{Subject: "u1", Issuer: "auth.example.com"})
	if _, err := v.Verify(right); err != nil {
		t.Errorf("token from the configured issuer rejected: %v", err)
	}
}

func TestKeyRotation(t *testing.T) {
	oldKey := SigningKey{ID: "old", Secret: []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}
	newKey := SigningKey{ID: "new", Secret: []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")}

	v := mustVerifier(t, oldKey)
	if err := v.AddKey(newKey); err != nil {
		t.Fatalf("add key: %v", err)
	}

	// Both keys verify during the overlap window.
	for _, k := range []SigningKey{oldKey, newKey} {
		tok, _ := Sign(k, Claims{Subject: "u1"})
		if _, err := v.Verify(tok); err != nil {
			t.Errorf("key %s did not verify during rotation: %v", k.ID, err)
		}
	}

	v.RemoveKey("old")
	tok, _ := Sign(oldKey, Claims{Subject: "u1"})
	if _, err := v.Verify(tok); !errors.Is(err, ErrUnknownKeyID) {
		t.Errorf("retired key still verifying: %v", err)
	}
}

func TestShortKeyRejected(t *testing.T) {
	if _, err := NewVerifier(VerifierConfig{Keys: []SigningKey{{ID: "weak", Secret: []byte("short")}}}); err == nil {
		t.Error("a 5-byte HMAC secret was accepted")
	}
}

func TestRevocation(t *testing.T) {
	v := mustVerifier(t)
	tok := mustSign(t, Claims{
		Subject: "u1", TokenID: "jti-1",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})

	if _, err := v.Verify(tok); err != nil {
		t.Fatalf("pre-revocation verify: %v", err)
	}
	v.Revoke("jti-1", time.Now().Add(time.Hour))
	if _, err := v.Verify(tok); err == nil {
		t.Error("revoked token still verifies")
	}
}

func TestDefaultScopesExcludeAdmin(t *testing.T) {
	v := mustVerifier(t)
	tok := mustSign(t, Claims{Subject: "u1"}) // no scp claim

	p, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !p.HasScope(ScopePublish) || !p.HasScope(ScopeSubscribe) {
		t.Error("default scopes should cover publish and subscribe")
	}
	if p.HasScope(ScopeAdmin) {
		t.Error("admin must never be granted implicitly")
	}
}

func TestPrincipalExpiredIsRecheckedOnEachCall(t *testing.T) {
	p := &Principal{UserID: "u1", ExpiresAt: time.Now().Add(time.Second)}
	if p.Expired(time.Now()) {
		t.Error("principal reported expired too early")
	}
	if !p.Expired(time.Now().Add(2 * time.Second)) {
		t.Error("principal did not expire")
	}

	anon := &Principal{Anonymous: true}
	if anon.Expired(time.Now().Add(100 * time.Hour)) {
		t.Error("shared-key principal should not expire")
	}
}

// --- Pattern matching ---

func TestTopicMatch(t *testing.T) {
	cases := []struct {
		topic, pattern string
		want           bool
	}{
		{"chat.inbox.u1", "chat.inbox.u1", true},
		{"chat.inbox.u1", "chat.inbox.*", true},
		{"chat.inbox.u1", "chat.inbox.u2", false},
		{"chat.inbox.u1.msg", "chat.inbox.*", false},
		{"chat.inbox.u1.msg", "chat.inbox.#", true},
		{"chat.inbox.u1", "chat.inbox.#", true},
		{"chat.inbox", "chat.inbox.#", true},
		{"anything.at.all", "#", true},
		{"chat.group.g1.meta", "chat.group.*.#", true},
		{"chat.group.g1", "chat.group.*.#", true},
		{"chat.direct.g1", "chat.group.*.#", false},
		{"a.b.c.d.e", "a.#.e", true},
		{"a.b.c.d.f", "a.#.e", false},
	}
	for _, c := range cases {
		if got := topicMatch(c.topic, c.pattern); got != c.want {
			t.Errorf("topicMatch(%q, %q) = %v, want %v", c.topic, c.pattern, got, c.want)
		}
	}
}

func TestExpandPlaceholders(t *testing.T) {
	p := &Principal{UserID: "u1", DeviceID: "d9", Tenant: "t3"}
	got := expand("chat.inbox.${user}.${device}.${tenant}", p)
	if got != "chat.inbox.u1.d9.t3" {
		t.Errorf("expand gave %q", got)
	}
	if got := expand("chat.static", p); got != "chat.static" {
		t.Errorf("expand mangled a literal pattern: %q", got)
	}
}

// --- Policy ---

func chatPolicy(t *testing.T) (*Policy, *StaticMembership) {
	t.Helper()
	m := NewStaticMembership()
	return NewPolicy(PolicyConfig{Rules: ChatPolicyRules(), Membership: m}), m
}

func userPrincipal(id string, scopes ...string) *Principal {
	s := map[string]bool{ScopePublish: true, ScopeSubscribe: true, ScopePresence: true}
	for _, x := range scopes {
		s[x] = true
	}
	return &Principal{UserID: id, Scopes: s}
}

func TestPolicyDefaultDeny(t *testing.T) {
	p := NewPolicy(PolicyConfig{})
	if err := p.Authorize(context.Background(), userPrincipal("u1"), "anything", ActionRead); !errors.Is(err, ErrDenied) {
		t.Errorf("empty policy did not deny: %v", err)
	}
}

func TestPolicyRejectsNilPrincipal(t *testing.T) {
	p, _ := chatPolicy(t)
	if err := p.Authorize(context.Background(), nil, "chat.inbox.u1", ActionRead); !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("nil principal: got %v", err)
	}
}

func TestPolicyOwnInboxOnly(t *testing.T) {
	p, _ := chatPolicy(t)
	ctx := context.Background()
	alice := userPrincipal("alice")

	if err := p.Authorize(ctx, alice, "chat.inbox.alice", ActionRead); err != nil {
		t.Errorf("alice denied her own inbox: %v", err)
	}
	if err := p.Authorize(ctx, alice, "chat.inbox.alice.archive", ActionWrite); err != nil {
		t.Errorf("alice denied a sub-topic of her own inbox: %v", err)
	}

	// The critical property: no reading anyone else's inbox.
	if err := p.Authorize(ctx, alice, "chat.inbox.bob", ActionRead); !errors.Is(err, ErrDenied) {
		t.Error("alice could read bob's inbox")
	}
	if err := p.Authorize(ctx, alice, "chat.inbox.bob.archive", ActionWrite); !errors.Is(err, ErrDenied) {
		t.Error("alice could write into bob's inbox")
	}
}

func TestPolicyMembershipGating(t *testing.T) {
	p, m := chatPolicy(t)
	ctx := context.Background()
	alice := userPrincipal("alice")

	topic := "chat.group.eng-team"
	if err := p.Authorize(ctx, alice, topic, ActionRead); !errors.Is(err, ErrDenied) {
		t.Error("non-member could read a group conversation")
	}

	m.Add("", "eng-team", "alice")
	if err := p.Authorize(ctx, alice, topic, ActionRead); err != nil {
		t.Errorf("member denied: %v", err)
	}
	if err := p.Authorize(ctx, alice, topic+".attachments", ActionWrite); err != nil {
		t.Errorf("member denied a sub-topic: %v", err)
	}

	// Removal takes effect on the next check.
	m.Remove("", "eng-team", "alice")
	if err := p.Authorize(ctx, alice, topic, ActionRead); !errors.Is(err, ErrDenied) {
		t.Error("removed member retained access")
	}
}

func TestPolicyMembershipFailureDeniesAccess(t *testing.T) {
	failing := MembershipFunc(func(context.Context, string, string, string) (bool, error) {
		return false, errors.New("database unreachable")
	})
	p := NewPolicy(PolicyConfig{Rules: ChatPolicyRules(), Membership: failing})

	// A membership backend outage must fail closed, not open.
	err := p.Authorize(context.Background(), userPrincipal("alice"), "chat.group.g1", ActionRead)
	if !errors.Is(err, ErrDenied) {
		t.Errorf("membership backend failure did not deny: %v", err)
	}
}

func TestPolicyMissingMembershipCheckerFailsClosed(t *testing.T) {
	p := NewPolicy(PolicyConfig{Rules: ChatPolicyRules()}) // no Membership configured
	if err := p.Authorize(context.Background(), userPrincipal("alice"), "chat.group.g1", ActionRead); !errors.Is(err, ErrDenied) {
		t.Error("policy without a membership checker allowed group access")
	}
}

func TestPolicyDenyBeatsAllow(t *testing.T) {
	p := NewPolicy(PolicyConfig{Rules: []Rule{
		{Pattern: "secret.#", Effect: Deny},
		{Pattern: "#", Effect: Allow}, // broad allow placed after the deny
	}})
	ctx := context.Background()
	pr := userPrincipal("u1")

	if err := p.Authorize(ctx, pr, "public.thing", ActionRead); err != nil {
		t.Errorf("broad allow did not apply: %v", err)
	}
	if err := p.Authorize(ctx, pr, "secret.thing", ActionRead); !errors.Is(err, ErrDenied) {
		t.Error("a later broad allow overrode an explicit deny")
	}
}

func TestPolicyActionScoping(t *testing.T) {
	p, _ := chatPolicy(t)
	ctx := context.Background()
	alice := userPrincipal("alice")

	if err := p.Authorize(ctx, alice, "system.announcements", ActionRead); err != nil {
		t.Errorf("user denied reading system announcements: %v", err)
	}
	if err := p.Authorize(ctx, alice, "system.announcements", ActionWrite); !errors.Is(err, ErrDenied) {
		t.Error("user could write to a system topic")
	}
}

func TestPolicyPresenceAsymmetry(t *testing.T) {
	p, _ := chatPolicy(t)
	ctx := context.Background()
	alice := userPrincipal("alice")

	if err := p.Authorize(ctx, alice, "presence.alice", ActionWrite); err != nil {
		t.Errorf("alice cannot publish her own presence: %v", err)
	}
	if err := p.Authorize(ctx, alice, "presence.bob", ActionWrite); !errors.Is(err, ErrDenied) {
		t.Error("alice could forge bob's presence")
	}
	if err := p.Authorize(ctx, alice, "presence.bob", ActionRead); err != nil {
		t.Errorf("alice cannot observe bob's presence: %v", err)
	}
}

func TestPolicyManageRequiresAdminScope(t *testing.T) {
	p, _ := chatPolicy(t)
	ctx := context.Background()

	if err := p.Authorize(ctx, userPrincipal("alice"), "chat.group.g1", ActionManage); !errors.Is(err, ErrDenied) {
		t.Error("a plain user could manage topics")
	}
	admin := userPrincipal("ops", ScopeAdmin)
	if err := p.Authorize(ctx, admin, "chat.group.g1", ActionManage); err != nil {
		t.Errorf("admin denied manage: %v", err)
	}
}

func TestPolicyExpiredPrincipalDenied(t *testing.T) {
	p, _ := chatPolicy(t)
	alice := userPrincipal("alice")
	alice.ExpiresAt = time.Now().Add(-time.Minute)

	if err := p.Authorize(context.Background(), alice, "chat.inbox.alice", ActionRead); !errors.Is(err, ErrDenied) {
		t.Error("expired principal was authorised")
	}
}

func TestPolicyAnonymousBypass(t *testing.T) {
	ctx := context.Background()
	anon := &Principal{Anonymous: true}

	strict := NewPolicy(PolicyConfig{Rules: ChatPolicyRules()})
	if err := strict.Authorize(ctx, anon, "chat.inbox.bob", ActionWrite); !errors.Is(err, ErrDenied) {
		t.Error("shared-key connection allowed while AllowAnonymous is off")
	}

	lenient := NewPolicy(PolicyConfig{Rules: ChatPolicyRules(), AllowAnonymous: true})
	if err := lenient.Authorize(ctx, anon, "chat.inbox.bob", ActionWrite); err != nil {
		t.Errorf("trusted backend denied while AllowAnonymous is on: %v", err)
	}
}

func TestPolicyConcurrentAuthorize(t *testing.T) {
	p, m := chatPolicy(t)
	m.Add("", "g1", "alice")
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pr := userPrincipal(fmt.Sprintf("u%d", i))
			p.Can(ctx, pr, "chat.group.g1", ActionRead)
			p.Can(ctx, userPrincipal("alice"), "chat.group.g1", ActionRead)
			if i%10 == 0 {
				p.SetRules(ChatPolicyRules())
			}
		}(i)
	}
	wg.Wait()
}

// --- Membership helpers ---

func TestStaticMembership(t *testing.T) {
	m := NewStaticMembership()
	ctx := context.Background()

	m.Add("t1", "g1", "alice", "bob")
	if ok, _ := m.IsMember(ctx, "t1", "alice", "g1"); !ok {
		t.Error("alice not a member after Add")
	}
	if ok, _ := m.IsMember(ctx, "t1", "carol", "g1"); ok {
		t.Error("carol reported as a member")
	}
	// Tenants are isolated.
	if ok, _ := m.IsMember(ctx, "t2", "alice", "g1"); ok {
		t.Error("membership leaked across tenants")
	}
	if m.MemberCount("t1", "g1") != 2 {
		t.Errorf("MemberCount = %d, want 2", m.MemberCount("t1", "g1"))
	}

	m.Remove("t1", "g1", "alice")
	if ok, _ := m.IsMember(ctx, "t1", "alice", "g1"); ok {
		t.Error("alice still a member after Remove")
	}
}

func TestCachedMembership(t *testing.T) {
	var calls int
	var mu sync.Mutex
	inner := MembershipFunc(func(_ context.Context, _, user, group string) (bool, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return user == "alice", nil
	})

	c := NewCachedMembership(inner, time.Minute, 100)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		if ok, _ := c.IsMember(ctx, "", "alice", "g1"); !ok {
			t.Fatal("alice should be a member")
		}
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("backend called %d times for 10 identical questions", got)
	}

	// Negative answers are cached too — otherwise an attacker probing groups
	// they do not belong to would hammer the backend on every attempt.
	for i := 0; i < 5; i++ {
		c.IsMember(ctx, "", "mallory", "g1")
	}
	mu.Lock()
	got = calls
	mu.Unlock()
	if got != 2 {
		t.Errorf("negative answers not cached: %d backend calls", got)
	}

	c.Invalidate("", "alice", "g1")
	c.IsMember(ctx, "", "alice", "g1")
	mu.Lock()
	got = calls
	mu.Unlock()
	if got != 3 {
		t.Errorf("Invalidate did not force a refresh: %d calls", got)
	}
}

func TestCachedMembershipExpiry(t *testing.T) {
	var calls int
	inner := MembershipFunc(func(context.Context, string, string, string) (bool, error) {
		calls++
		return true, nil
	})
	c := NewCachedMembership(inner, 50*time.Millisecond, 100)
	ctx := context.Background()

	c.IsMember(ctx, "", "alice", "g1")
	c.IsMember(ctx, "", "alice", "g1")
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}

	time.Sleep(80 * time.Millisecond)
	c.IsMember(ctx, "", "alice", "g1")
	if calls != 2 {
		t.Errorf("cache entry did not expire: %d calls", calls)
	}
}

func TestCachedMembershipInvalidateGroup(t *testing.T) {
	var calls int
	inner := MembershipFunc(func(context.Context, string, string, string) (bool, error) {
		calls++
		return true, nil
	})
	c := NewCachedMembership(inner, time.Minute, 100)
	ctx := context.Background()

	c.IsMember(ctx, "t1", "alice", "g1")
	c.IsMember(ctx, "t1", "bob", "g1")
	c.IsMember(ctx, "t1", "alice", "g2")
	before := calls

	c.InvalidateGroup("t1", "g1")

	c.IsMember(ctx, "t1", "alice", "g2") // still cached
	if calls != before {
		t.Error("InvalidateGroup evicted an unrelated group")
	}
	c.IsMember(ctx, "t1", "alice", "g1")
	c.IsMember(ctx, "t1", "bob", "g1")
	if calls != before+2 {
		t.Errorf("InvalidateGroup did not evict both members: %d calls", calls-before)
	}
}

func TestCachedMembershipPropagatesError(t *testing.T) {
	boom := errors.New("boom")
	c := NewCachedMembership(MembershipFunc(func(context.Context, string, string, string) (bool, error) {
		return false, boom
	}), time.Minute, 10)

	if _, err := c.IsMember(context.Background(), "", "u", "g"); !errors.Is(err, boom) {
		t.Errorf("error not propagated: %v", err)
	}
}

func BenchmarkAuthorize(b *testing.B) {
	m := NewStaticMembership()
	m.Add("", "g1", "alice")
	p := NewPolicy(PolicyConfig{Rules: ChatPolicyRules(), Membership: m})
	pr := userPrincipal("alice")
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Authorize(ctx, pr, "chat.group.g1", ActionWrite)
	}
}

func BenchmarkVerify(b *testing.B) {
	v, _ := NewVerifier(VerifierConfig{Keys: []SigningKey{testKey}})
	tok, _ := Sign(testKey, Claims{Subject: "u1", ExpiresAt: time.Now().Add(time.Hour).Unix()})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v.Verify(tok)
	}
}
