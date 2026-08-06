package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Principal ---

func TestPrincipalString(t *testing.T) {
	cases := []struct {
		p    *Principal
		want string
	}{
		{nil, "<nil>"},
		{&Principal{Anonymous: true}, "shared-key"},
		{&Principal{UserID: "alice"}, "alice"},
		{&Principal{UserID: "alice", DeviceID: "phone"}, "alice/phone"},
	}
	for _, c := range cases {
		if got := c.p.String(); got != c.want {
			t.Errorf("String() = %q, want %q", got, c.want)
		}
	}
}

func TestPrincipalStringOmitsTokenID(t *testing.T) {
	// A log line must never leak a credential.
	p := &Principal{UserID: "alice", DeviceID: "phone", TokenID: "secret-jti-value"}
	if strings.Contains(p.String(), "secret-jti-value") {
		t.Error("String() leaked the token ID")
	}
}

func TestNilPrincipalHasNoScopesAndIsExpired(t *testing.T) {
	var p *Principal
	if p.HasScope(ScopePublish) {
		t.Error("a nil principal has scopes")
	}
	if !p.Expired(time.Now()) {
		t.Error("a nil principal is not expired")
	}
}

func TestPrincipalWithoutExpiryNeverExpires(t *testing.T) {
	p := &Principal{UserID: "alice"}
	if p.Expired(time.Now().Add(1000 * time.Hour)) {
		t.Error("a principal with no expiry claim expired")
	}
}

// --- Policy accessors ---

func TestPolicyRulesReturnsCopy(t *testing.T) {
	p := NewPolicy(PolicyConfig{Rules: ChatPolicyRules()})

	rules := p.Rules()
	if len(rules) == 0 {
		t.Fatal("Rules() returned nothing")
	}

	// Mutating the returned slice must not change the live policy.
	rules[0].Effect = Deny
	rules[0].Pattern = "#"

	fresh := p.Rules()
	if fresh[0].Effect == Deny && fresh[0].Pattern == "#" {
		t.Error("Rules() handed out the live rule set")
	}
}

func TestNewPolicyCopiesInputRules(t *testing.T) {
	rules := ChatPolicyRules()
	p := NewPolicy(PolicyConfig{Rules: rules, Membership: NewStaticMembership()})

	// Mutating the caller's slice after construction must not affect policy.
	rules[0] = Rule{Pattern: "#", Effect: Allow}

	alice := userPrincipal("alice")
	if err := p.Authorize(context.Background(), alice, "chat.inbox.bob", ActionRead); err == nil {
		t.Error("mutating the caller's rule slice changed the policy")
	}
}

func TestSetMembership(t *testing.T) {
	p := NewPolicy(PolicyConfig{Rules: ChatPolicyRules()})
	ctx := context.Background()
	alice := userPrincipal("alice")

	// Default is deny-all membership.
	if err := p.Authorize(ctx, alice, "chat.group.g1", ActionRead); err == nil {
		t.Fatal("the default membership checker granted access")
	}

	m := NewStaticMembership()
	m.Add("", "g1", "alice")
	p.SetMembership(m)

	if err := p.Authorize(ctx, alice, "chat.group.g1", ActionRead); err != nil {
		t.Errorf("after SetMembership: %v", err)
	}

	// Setting nil must fall back to deny-all, not panic or allow.
	p.SetMembership(nil)
	if err := p.Authorize(ctx, alice, "chat.group.g1", ActionRead); err == nil {
		t.Error("SetMembership(nil) left the policy open")
	}
}

func TestSetRulesIsAtomic(t *testing.T) {
	p := NewPolicy(PolicyConfig{Rules: []Rule{{Pattern: "a.#", Effect: Allow}}})
	ctx := context.Background()
	pr := userPrincipal("alice")

	if !p.Can(ctx, pr, "a.x", ActionRead) {
		t.Fatal("initial rule not applied")
	}
	p.SetRules([]Rule{{Pattern: "b.#", Effect: Allow}})
	if p.Can(ctx, pr, "a.x", ActionRead) {
		t.Error("old rules survived SetRules")
	}
	if !p.Can(ctx, pr, "b.x", ActionRead) {
		t.Error("new rules not applied")
	}
}

// --- StaticMembership edge cases ---

func TestStaticMembershipMembersList(t *testing.T) {
	m := NewStaticMembership()
	m.Add("t1", "g1", "alice", "bob", "carol")

	got := m.Members("t1", "g1")
	if len(got) != 3 {
		t.Fatalf("Members = %v", got)
	}
	set := map[string]bool{}
	for _, u := range got {
		set[u] = true
	}
	for _, want := range []string{"alice", "bob", "carol"} {
		if !set[want] {
			t.Errorf("%s missing from Members", want)
		}
	}

	if got := m.Members("t1", "nonexistent"); len(got) != 0 {
		t.Errorf("unknown group returned %v", got)
	}
	if got := m.Members("other-tenant", "g1"); len(got) != 0 {
		t.Errorf("membership leaked across tenants: %v", got)
	}
}

func TestStaticMembershipAddIsIdempotent(t *testing.T) {
	m := NewStaticMembership()
	m.Add("", "g1", "alice")
	m.Add("", "g1", "alice")
	m.Add("", "g1", "alice", "alice")

	if n := m.MemberCount("", "g1"); n != 1 {
		t.Errorf("MemberCount = %d after repeated adds, want 1", n)
	}
}

func TestStaticMembershipRemoveLastPrunesGroup(t *testing.T) {
	m := NewStaticMembership()
	m.Add("", "g1", "alice")
	m.Remove("", "g1", "alice")

	if n := m.MemberCount("", "g1"); n != 0 {
		t.Errorf("MemberCount = %d", n)
	}
	// Removing from a group that no longer exists must not panic.
	m.Remove("", "g1", "alice")
	m.Remove("", "never-existed", "bob")
}

func TestStaticMembershipConcurrent(t *testing.T) {
	m := NewStaticMembership()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			group := "g" + string(rune('a'+i%5))
			user := "u" + string(rune('a'+i))
			for j := 0; j < 100; j++ {
				m.Add("", group, user)
				m.IsMember(ctx, "", user, group)
				m.Members("", group)
				m.MemberCount("", group)
				if j%10 == 0 {
					m.Remove("", group, user)
				}
			}
		}(i)
	}
	wg.Wait()
}

// --- Token edge cases ---

func TestVerifyTokenWithoutKidUsesFallback(t *testing.T) {
	// Sign without a key ID, so the token carries no kid header.
	unnamed := SigningKey{ID: "", Secret: testKey.Secret}
	tok, err := Sign(unnamed, Claims{Subject: "alice"})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	v := mustVerifier(t)
	if _, err := v.Verify(tok); err != nil {
		t.Errorf("a token with no kid did not use the fallback key: %v", err)
	}
}

func TestSignRejectsShortKeyAndEmptySubject(t *testing.T) {
	if _, err := Sign(SigningKey{ID: "k", Secret: []byte("short")}, Claims{Subject: "alice"}); err == nil {
		t.Error("Sign accepted a short key")
	}
	if _, err := Sign(testKey, Claims{}); err == nil {
		t.Error("Sign accepted an empty subject")
	}
}

func TestAddKeyRejectsShortSecret(t *testing.T) {
	v := mustVerifier(t)
	if err := v.AddKey(SigningKey{ID: "weak", Secret: []byte("nope")}); err == nil {
		t.Error("AddKey accepted a short secret")
	}
}

func TestRevocationExpires(t *testing.T) {
	v := mustVerifier(t)
	tok := mustSign(t, Claims{
		Subject: "alice", TokenID: "jti-1",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})

	// Revoke for a window that has already passed: the token is usable again.
	v.Revoke("jti-1", time.Now().Add(-time.Minute))
	if _, err := v.Verify(tok); err != nil {
		t.Errorf("an expired revocation still blocked the token: %v", err)
	}
}

func TestRevocationDoesNotAffectOtherTokens(t *testing.T) {
	v := mustVerifier(t)
	a := mustSign(t, Claims{Subject: "alice", TokenID: "jti-a", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	b := mustSign(t, Claims{Subject: "bob", TokenID: "jti-b", ExpiresAt: time.Now().Add(time.Hour).Unix()})

	v.Revoke("jti-a", time.Now().Add(time.Hour))
	if _, err := v.Verify(a); err == nil {
		t.Error("the revoked token still verifies")
	}
	if _, err := v.Verify(b); err != nil {
		t.Errorf("an unrelated token was revoked: %v", err)
	}
}

func TestTokenWithNoTokenIDSkipsRevocationCheck(t *testing.T) {
	v := mustVerifier(t)
	v.Revoke("", time.Now().Add(time.Hour)) // revoking the empty jti

	tok := mustSign(t, Claims{Subject: "alice", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	if _, err := v.Verify(tok); err != nil {
		t.Errorf("a token with no jti was caught by an empty-string revocation: %v", err)
	}
}

func TestVerifyRejectsNonJSONHeader(t *testing.T) {
	v := mustVerifier(t)
	tok := base64.RawURLEncoding.EncodeToString([]byte("not json")) + ".e30.sig"
	if _, err := v.Verify(tok); err == nil {
		t.Error("a token with a non-JSON header was accepted")
	}
}

func TestVerifyRejectsNonJSONClaims(t *testing.T) {
	hdr, _ := json.Marshal(jwtHeader{Alg: "HS256", Kid: "k1"})
	input := base64.RawURLEncoding.EncodeToString(hdr) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("not json"))
	sig := hmacSHA256(testKey.Secret, []byte(input))
	tok := input + "." + base64.RawURLEncoding.EncodeToString(sig)

	v := mustVerifier(t)
	if _, err := v.Verify(tok); err == nil {
		t.Error("a correctly-signed token with non-JSON claims was accepted")
	}
}

func TestVerifyRejectsNonBase64Segments(t *testing.T) {
	v := mustVerifier(t)
	valid := mustSign(t, Claims{Subject: "alice"})
	parts := strings.Split(valid, ".")

	for i := 0; i < 3; i++ {
		bad := append([]string(nil), parts...)
		bad[i] = "!!!not-base64!!!"
		if _, err := v.Verify(strings.Join(bad, ".")); err == nil {
			t.Errorf("segment %d with invalid base64 was accepted", i)
		}
	}
}

func TestConcurrentVerifyAndRotate(t *testing.T) {
	v := mustVerifier(t)
	tok := mustSign(t, Claims{Subject: "alice", ExpiresAt: time.Now().Add(time.Hour).Unix()})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				v.Verify(tok)
				if j%25 == 0 {
					v.AddKey(SigningKey{
						ID:     "rotating" + string(rune('a'+i)),
						Secret: []byte("0123456789abcdef0123456789abcdef"),
					})
					v.Revoke("jti-"+string(rune('a'+i)), time.Now().Add(time.Minute))
				}
			}
		}(i)
	}
	wg.Wait()

	// The original key must still work after all that churn.
	if _, err := v.Verify(tok); err != nil {
		t.Errorf("the original key stopped verifying after concurrent rotation: %v", err)
	}
}

// --- ACL: exception patterns ---

func TestExceptPatternCarvesAHole(t *testing.T) {
	p := NewPolicy(PolicyConfig{Rules: []Rule{
		{Pattern: "secret.#", Effect: Allow},
		{Pattern: "secret.#", ExceptPattern: "secret.${user}.#", Effect: Deny},
	}})
	ctx := context.Background()
	alice := userPrincipal("alice")

	if err := p.Authorize(ctx, alice, "secret.alice.notes", ActionRead); err != nil {
		t.Errorf("the exception did not spare alice's own path: %v", err)
	}
	if err := p.Authorize(ctx, alice, "secret.bob.notes", ActionRead); err == nil {
		t.Error("the deny rule did not apply outside its exception")
	}
}

func TestExceptPatternOnAllowRule(t *testing.T) {
	p := NewPolicy(PolicyConfig{Rules: []Rule{
		{Pattern: "data.#", ExceptPattern: "data.private.#", Effect: Allow},
	}})
	ctx := context.Background()
	pr := userPrincipal("alice")

	if !p.Can(ctx, pr, "data.public.x", ActionRead) {
		t.Error("allow rule did not apply")
	}
	if p.Can(ctx, pr, "data.private.x", ActionRead) {
		t.Error("the exception did not remove the allow")
	}
}

// --- ACL: membership segment bounds ---

func TestMembershipSegmentOutOfRangeIsSkipped(t *testing.T) {
	m := NewStaticMembership()
	m.Add("", "anything", "alice")

	p := NewPolicy(PolicyConfig{
		Rules: []Rule{{
			Pattern: "chat.#", Effect: Allow,
			RequireMembership: true, MembershipSegment: 9,
		}},
		Membership: m,
	})

	// The pattern matches but the topic has no segment 9, so the rule cannot
	// apply and default-deny takes over. It must not panic or index wildly.
	if err := p.Authorize(context.Background(), userPrincipal("alice"), "chat.x", ActionRead); err == nil {
		t.Error("a rule with an out-of-range membership segment granted access")
	}
}

func TestNegativeMembershipSegmentIsSkipped(t *testing.T) {
	p := NewPolicy(PolicyConfig{
		Rules: []Rule{{
			Pattern: "chat.#", Effect: Allow,
			RequireMembership: true, MembershipSegment: -1,
		}},
		Membership: NewStaticMembership(),
	})
	if err := p.Authorize(context.Background(), userPrincipal("alice"), "chat.x", ActionRead); err == nil {
		t.Error("a negative membership segment granted access")
	}
}

// --- ACL: scope requirements ---

func TestRequireScopeWithMultipleOptions(t *testing.T) {
	p := NewPolicy(PolicyConfig{Rules: []Rule{
		{Pattern: "x.#", Effect: Allow, RequireScope: []string{ScopeAdmin, ScopePresence}},
	}})
	ctx := context.Background()

	// Holding either listed scope is enough.
	if !p.Can(ctx, userPrincipal("a"), "x.y", ActionRead) {
		t.Error("a principal with the presence scope was denied")
	}
	if !p.Can(ctx, userPrincipal("b", ScopeAdmin), "x.y", ActionRead) {
		t.Error("a principal with the admin scope was denied")
	}

	// Holding neither is not.
	bare := &Principal{UserID: "c", Scopes: map[string]bool{ScopeSubscribe: true}}
	if p.Can(ctx, bare, "x.y", ActionRead) {
		t.Error("a principal with neither required scope was allowed")
	}
}

func TestAnonymousPrincipalHoldsEveryScope(t *testing.T) {
	anon := &Principal{Anonymous: true}
	for _, s := range []string{ScopePublish, ScopeSubscribe, ScopeAdmin, ScopePresence, "invented"} {
		if !anon.HasScope(s) {
			t.Errorf("anonymous principal lacks scope %q", s)
		}
	}
}

// --- ACL: action coverage ---

func TestRuleWithNoActionsCoversAll(t *testing.T) {
	p := NewPolicy(PolicyConfig{Rules: []Rule{{Pattern: "x.#", Effect: Allow}}})
	ctx := context.Background()
	pr := userPrincipal("alice", ScopeAdmin)

	for _, a := range []Action{ActionRead, ActionWrite, ActionManage} {
		if !p.Can(ctx, pr, "x.y", a) {
			t.Errorf("a rule with no Actions did not cover %s", a)
		}
	}
}

func TestUnknownActionIsDenied(t *testing.T) {
	p := NewPolicy(PolicyConfig{Rules: []Rule{
		{Pattern: "#", Effect: Allow, Actions: []Action{ActionRead}},
	}})
	if p.Can(context.Background(), userPrincipal("alice"), "x", Action("delete")) {
		t.Error("an action outside the rule's list was allowed")
	}
}

// --- Pattern matching corner cases ---

func TestTopicMatchEdgeCases(t *testing.T) {
	cases := []struct {
		topic, pattern string
		want           bool
	}{
		{"", "", true},
		{"", "#", true},
		{"", "*", true}, // one empty segment
		{"a", "", false},
		{"a.b", "a.b.#", true},
		{"a.b.c", "#.c", true},
		{"a.b.c", "#.#", true},
		{"a", "#.#.#", true},
		{"a.b", "*.*", true},
		{"a.b.c", "*.*", false},
		{"a..b", "a.*.b", true}, // an empty middle segment still matches '*'
	}
	for _, c := range cases {
		if got := topicMatch(c.topic, c.pattern); got != c.want {
			t.Errorf("topicMatch(%q, %q) = %v, want %v", c.topic, c.pattern, got, c.want)
		}
	}
}

func TestExpandWithEmptyPrincipalFields(t *testing.T) {
	// A principal with no device must not turn "${device}" into a pattern that
	// accidentally matches something.
	p := &Principal{UserID: "alice"}
	got := expand("chat.${user}.${device}", p)
	if got != "chat.alice." {
		t.Errorf("expand = %q", got)
	}
	if topicMatch("chat.alice.phone", got) {
		t.Error("an empty device placeholder matched a real device segment")
	}
}

// --- Cached membership ---

func TestCachedMembershipEvictionUnderPressure(t *testing.T) {
	var calls int
	var mu sync.Mutex
	inner := MembershipFunc(func(_ context.Context, _, _, _ string) (bool, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return true, nil
	})

	c := NewCachedMembership(inner, time.Hour, 10)
	ctx := context.Background()

	// Far more distinct keys than the cache holds: it must bound itself rather
	// than growing without limit.
	for i := 0; i < 200; i++ {
		c.IsMember(ctx, "", "user"+string(rune(i)), "g1")
	}

	c.mu.RLock()
	size := len(c.entries)
	c.mu.RUnlock()
	if size > 20 {
		t.Errorf("cache holds %d entries against a max of 10", size)
	}
}

func TestCachedMembershipDefaults(t *testing.T) {
	c := NewCachedMembership(NewStaticMembership(), 0, 0)
	if c.ttl <= 0 || c.maxSize <= 0 {
		t.Errorf("defaults not applied: ttl=%v maxSize=%d", c.ttl, c.maxSize)
	}
}

func TestCachedMembershipConcurrent(t *testing.T) {
	inner := NewStaticMembership()
	inner.Add("", "g1", "alice")
	c := NewCachedMembership(inner, 50*time.Millisecond, 1000)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				ok, err := c.IsMember(ctx, "", "alice", "g1")
				if err != nil {
					t.Errorf("IsMember: %v", err)
					return
				}
				if !ok {
					t.Error("alice reported as a non-member")
					return
				}
				if j%50 == 0 {
					c.Invalidate("", "alice", "g1")
					c.InvalidateGroup("", "g1")
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestDenyAllMembership(t *testing.T) {
	ok, err := DenyAllMembership{}.IsMember(context.Background(), "t", "u", "g")
	if err != nil || ok {
		t.Errorf("DenyAllMembership returned %v, %v", ok, err)
	}
}

// --- ChatPolicyRules as a whole ---

func TestChatPolicyRulesFullMatrix(t *testing.T) {
	m := NewStaticMembership()
	m.Add("", "g1", "alice")
	m.Add("", "d1", "alice")
	p := NewPolicy(PolicyConfig{Rules: ChatPolicyRules(), Membership: m})
	ctx := context.Background()

	alice := userPrincipal("alice")
	admin := userPrincipal("ops", ScopeAdmin)

	cases := []struct {
		who   *Principal
		topic string
		act   Action
		allow bool
	}{
		{alice, "chat.inbox.alice", ActionRead, true},
		{alice, "chat.inbox.alice", ActionWrite, true},
		{alice, "chat.inbox.alice.archive", ActionRead, true},
		{alice, "chat.inbox.bob", ActionRead, false},
		{alice, "chat.inbox.bob.archive", ActionWrite, false},
		{alice, "chat.group.g1", ActionRead, true},
		{alice, "chat.group.g1", ActionWrite, true},
		{alice, "chat.group.g2", ActionRead, false},
		{alice, "chat.direct.d1", ActionWrite, true},
		{alice, "chat.direct.d2", ActionWrite, false},
		{alice, "presence.alice", ActionWrite, true},
		{alice, "presence.bob", ActionWrite, false},
		{alice, "presence.bob", ActionRead, true},
		{alice, "typing.g1", ActionWrite, true},
		{alice, "typing.g2", ActionWrite, false},
		{alice, "system.news", ActionRead, true},
		{alice, "system.news", ActionWrite, false},
		{alice, "chat.group.g1", ActionManage, false},
		{admin, "chat.group.g1", ActionManage, true},
		{alice, "totally.unknown.topic", ActionRead, false},
	}

	for _, c := range cases {
		err := p.Authorize(ctx, c.who, c.topic, c.act)
		got := err == nil
		if got != c.allow {
			t.Errorf("%s %s %s = %v, want %v (err=%v)",
				c.who.UserID, c.act, c.topic, got, c.allow, err)
		}
	}
}

func TestChatPolicyPresenceScopeRequired(t *testing.T) {
	p := NewPolicy(PolicyConfig{Rules: ChatPolicyRules(), Membership: NewStaticMembership()})
	ctx := context.Background()

	// A principal without the presence scope cannot publish presence.
	noPresence := &Principal{UserID: "alice", Scopes: map[string]bool{
		ScopePublish: true, ScopeSubscribe: true,
	}}
	if p.Can(ctx, noPresence, "presence.alice", ActionWrite) {
		t.Error("presence was writable without the presence scope")
	}
	// But reading is unaffected.
	if !p.Can(ctx, noPresence, "presence.bob", ActionRead) {
		t.Error("presence read requires the presence scope")
	}
}
