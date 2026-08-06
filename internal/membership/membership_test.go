package membership

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Client construction ---

func TestNewRequiresBaseURL(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("empty base URL was accepted")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	c, err := New(Options{BaseURL: "http://example.com"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if c.http.Timeout != 3*time.Second {
		t.Errorf("default timeout = %v, want 3s", c.http.Timeout)
	}
}

func TestTrimSlash(t *testing.T) {
	cases := map[string]string{
		"http://x.com":     "http://x.com",
		"http://x.com/":    "http://x.com",
		"http://x.com///":  "http://x.com",
		"http://x.com/api": "http://x.com/api",
		"":                 "",
		"///":              "",
	}
	for in, want := range cases {
		if got := trimSlash(in); got != want {
			t.Errorf("trimSlash(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBaseURLTrailingSlashDoesNotDoubleUp(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"member":true}`))
	}))
	defer srv.Close()

	c, _ := New(Options{BaseURL: srv.URL + "/"})
	c.IsMember(context.Background(), "", "alice", "g1")

	if gotPath != "/is-member" {
		t.Errorf("path = %q, want /is-member (trailing slash was not trimmed)", gotPath)
	}
}

// --- IsMember ---

func TestIsMemberSendsCorrectQuery(t *testing.T) {
	var got struct{ tenant, user, group, path, auth string }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		got.tenant, got.user, got.group = q.Get("tenant"), q.Get("user"), q.Get("group")
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		w.Write([]byte(`{"member":true}`))
	}))
	defer srv.Close()

	c, _ := New(Options{BaseURL: srv.URL, AuthHeader: "Bearer svc-token"})
	ok, err := c.IsMember(context.Background(), "tenant-a", "alice", "eng")
	if err != nil {
		t.Fatalf("IsMember: %v", err)
	}
	if !ok {
		t.Error("expected member=true")
	}
	if got.path != "/is-member" {
		t.Errorf("path = %q", got.path)
	}
	if got.tenant != "tenant-a" || got.user != "alice" || got.group != "eng" {
		t.Errorf("query params wrong: %+v", got)
	}
	if got.auth != "Bearer svc-token" {
		t.Errorf("auth header = %q", got.auth)
	}
}

func TestIsMemberFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"member":false}`))
	}))
	defer srv.Close()

	c, _ := New(Options{BaseURL: srv.URL})
	ok, err := c.IsMember(context.Background(), "", "mallory", "eng")
	if err != nil || ok {
		t.Errorf("got ok=%v err=%v, want false/nil", ok, err)
	}
}

func TestIsMemberHandlesMissingField(t *testing.T) {
	// A response with no "member" key must decode to false, not error —
	// but it must certainly not decode to true.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c, _ := New(Options{BaseURL: srv.URL})
	ok, err := c.IsMember(context.Background(), "", "alice", "eng")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("an empty response granted membership")
	}
}

// --- Members ---

func TestMembersReturnsList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/members" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte(`{"members":["alice","bob","carol"]}`))
	}))
	defer srv.Close()

	c, _ := New(Options{BaseURL: srv.URL})
	got, err := c.Members(context.Background(), "t1", "eng")
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(got) != 3 || got[0] != "alice" || got[2] != "carol" {
		t.Errorf("members = %v", got)
	}
}

func TestMembersEmptyGroup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"members":[]}`))
	}))
	defer srv.Close()

	c, _ := New(Options{BaseURL: srv.URL})
	got, err := c.Members(context.Background(), "", "ghost")
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

// --- Error handling ---

func TestNonOKStatusIsAnError(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 500, 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			w.Write([]byte(`{"error":"nope"}`))
		}))

		c, _ := New(Options{BaseURL: srv.URL})
		_, err := c.IsMember(context.Background(), "", "alice", "eng")
		if err == nil {
			t.Errorf("status %d was not treated as an error", code)
		}
		if err != nil && !strings.Contains(err.Error(), fmt.Sprint(code)) {
			t.Errorf("status %d: error does not mention the code: %v", code, err)
		}
		srv.Close()
	}
}

func TestMalformedJSONIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`this is not json`))
	}))
	defer srv.Close()

	c, _ := New(Options{BaseURL: srv.URL})
	if _, err := c.IsMember(context.Background(), "", "alice", "eng"); err == nil {
		t.Error("malformed JSON was accepted")
	}
}

func TestUnreachableEndpointIsAnError(t *testing.T) {
	c, _ := New(Options{BaseURL: "http://127.0.0.1:1", Timeout: 200 * time.Millisecond})
	if _, err := c.IsMember(context.Background(), "", "alice", "eng"); err == nil {
		t.Error("an unreachable endpoint returned no error")
	}
}

func TestTimeoutIsEnforced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Write([]byte(`{"member":true}`))
	}))
	defer srv.Close()

	c, _ := New(Options{BaseURL: srv.URL, Timeout: 50 * time.Millisecond})
	start := time.Now()
	_, err := c.IsMember(context.Background(), "", "alice", "eng")
	elapsed := time.Since(start)

	if err == nil {
		t.Error("slow endpoint did not time out")
	}
	// A membership lookup sits inline on the send path; it must fail fast
	// rather than hanging the user's connection.
	if elapsed > 300*time.Millisecond {
		t.Errorf("timeout took %v to fire, configured for 50ms", elapsed)
	}
}

func TestContextCancellationAborts(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Write([]byte(`{"member":true}`))
	}))
	defer srv.Close()
	defer close(release)

	c, _ := New(Options{BaseURL: srv.URL, Timeout: 10 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	if _, err := c.IsMember(ctx, "", "alice", "eng"); err == nil {
		t.Error("cancelled context did not abort the request")
	}
}

func TestOversizedResponseIsCapped(t *testing.T) {
	// A compromised or buggy membership endpoint must not be able to exhaust
	// broker memory with an unbounded body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"members":["`))
		chunk := strings.Repeat("a", 1<<20)
		for i := 0; i < 20; i++ { // 20MB, past the 8MB cap
			w.Write([]byte(chunk))
		}
		w.Write([]byte(`"]}`))
	}))
	defer srv.Close()

	c, _ := New(Options{BaseURL: srv.URL, Timeout: 5 * time.Second})
	// Truncated at the cap, so it fails to parse rather than being consumed.
	if _, err := c.Members(context.Background(), "", "big"); err == nil {
		t.Error("a 20MB response was accepted whole")
	}
}

func TestTruncateHelper(t *testing.T) {
	if got := truncate([]byte("short"), 100); got != "short" {
		t.Errorf("truncate short = %q", got)
	}
	got := truncate([]byte(strings.Repeat("x", 500)), 10)
	if len(got) != 13 || !strings.HasSuffix(got, "...") {
		t.Errorf("truncate long = %q (len %d)", got, len(got))
	}
}

func TestConnectionReuse(t *testing.T) {
	var conns int64
	// ConnState must be installed before the server starts serving, or the
	// assignment races the accept loop.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"member":true}`))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt64(&conns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	c, _ := New(Options{BaseURL: srv.URL})
	for i := 0; i < 20; i++ {
		if _, err := c.IsMember(context.Background(), "", "alice", "eng"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if n := atomic.LoadInt64(&conns); n > 3 {
		t.Errorf("20 sequential calls opened %d connections — keep-alive is not working", n)
	}
}

func TestConcurrentRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(fmt.Sprintf(`{"member":%v}`, r.URL.Query().Get("user") == "alice")))
	}))
	defer srv.Close()

	c, _ := New(Options{BaseURL: srv.URL})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := "alice"
			if i%2 == 0 {
				user = "mallory"
			}
			ok, err := c.IsMember(context.Background(), "", user, "eng")
			if err != nil {
				t.Errorf("concurrent call: %v", err)
				return
			}
			if ok != (user == "alice") {
				t.Errorf("user %s got member=%v — responses crossed", user, ok)
			}
		}(i)
	}
	wg.Wait()
}

// --- Static ---

func TestStaticMembers(t *testing.T) {
	s := NewStatic(map[string][]string{
		"eng":    {"alice", "bob"},
		"design": {"carol"},
		"empty":  {},
	})
	ctx := context.Background()

	got, _ := s.Members(ctx, "", "eng")
	if len(got) != 2 {
		t.Errorf("eng members = %v", got)
	}
	if got, _ := s.Members(ctx, "", "nonexistent"); len(got) != 0 {
		t.Errorf("unknown group returned %v", got)
	}

	for _, tc := range []struct {
		user, group string
		want        bool
	}{
		{"alice", "eng", true},
		{"bob", "eng", true},
		{"carol", "eng", false},
		{"alice", "design", false},
		{"alice", "nonexistent", false},
	} {
		if ok, _ := s.IsMember(ctx, "", tc.user, tc.group); ok != tc.want {
			t.Errorf("IsMember(%s, %s) = %v, want %v", tc.user, tc.group, ok, tc.want)
		}
	}
}

func TestStaticCopiesInput(t *testing.T) {
	src := map[string][]string{"eng": {"alice"}}
	s := NewStatic(src)

	// Mutating the caller's map must not change the lister.
	src["eng"] = append(src["eng"], "mallory")
	src["new"] = []string{"eve"}

	if ok, _ := s.IsMember(context.Background(), "", "mallory", "eng"); ok {
		t.Error("mutating the source map added a member")
	}
	if got, _ := s.Members(context.Background(), "", "new"); len(got) != 0 {
		t.Error("mutating the source map added a group")
	}
}

// --- DirectMembers ---

func TestDirectMembersDerivesFromID(t *testing.T) {
	d := &DirectMembers{}
	ctx := context.Background()

	got, err := d.Members(ctx, "", "alice:bob")
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Errorf("members = %v", got)
	}

	for _, tc := range []struct {
		user string
		want bool
	}{
		{"alice", true},
		{"bob", true},
		{"mallory", false},
	} {
		if ok, _ := d.IsMember(ctx, "", tc.user, "alice:bob"); ok != tc.want {
			t.Errorf("IsMember(%s) = %v, want %v", tc.user, ok, tc.want)
		}
	}
}

func TestDirectMembersCustomSeparator(t *testing.T) {
	d := &DirectMembers{Separator: "|"}
	got, _ := d.Members(context.Background(), "", "alice|bob")
	if len(got) != 2 || got[0] != "alice" {
		t.Errorf("members = %v", got)
	}
	// The default separator must no longer split.
	if got, _ := d.Members(context.Background(), "", "alice:bob"); len(got) != 0 {
		t.Errorf("wrong separator still split: %v", got)
	}
}

func TestDirectMembersRejectsMalformedIDs(t *testing.T) {
	d := &DirectMembers{}
	ctx := context.Background()

	// Anything that is not exactly two non-empty parts must not be treated as
	// a direct conversation — otherwise ":" would grant access to a
	// conversation between two empty-named users.
	for _, id := range []string{"alice", "alice:bob:carol", ":", ":bob", "alice:", ""} {
		got, _ := d.Members(ctx, "", id)
		if len(got) != 0 {
			t.Errorf("malformed id %q produced members %v", id, got)
		}
	}
}

func TestDirectMembersFallback(t *testing.T) {
	fallback := NewStatic(map[string][]string{"eng-team": {"alice", "bob", "carol"}})
	d := &DirectMembers{Fallback: fallback}
	ctx := context.Background()

	// A direct ID is handled without touching the fallback.
	got, _ := d.Members(ctx, "", "alice:bob")
	if len(got) != 2 {
		t.Errorf("direct id went to the fallback: %v", got)
	}

	// A group ID falls through.
	got, _ = d.Members(ctx, "", "eng-team")
	if len(got) != 3 {
		t.Errorf("group id did not reach the fallback: %v", got)
	}
	if ok, _ := d.IsMember(ctx, "", "carol", "eng-team"); !ok {
		t.Error("IsMember did not consult the fallback")
	}
	if ok, _ := d.IsMember(ctx, "", "mallory", "eng-team"); ok {
		t.Error("fallback granted membership to a non-member")
	}
}

func TestDirectMembersFallbackErrorPropagates(t *testing.T) {
	boom := fmt.Errorf("backend down")
	d := &DirectMembers{Fallback: memberFunc(func(context.Context, string, string) ([]string, error) {
		return nil, boom
	})}

	if _, err := d.Members(context.Background(), "", "group-1"); err == nil {
		t.Error("fallback error was swallowed by Members")
	}
	// IsMember must fail closed on a backend error, never fail open.
	ok, err := d.IsMember(context.Background(), "", "alice", "group-1")
	if err == nil {
		t.Error("fallback error was swallowed by IsMember")
	}
	if ok {
		t.Error("IsMember returned true despite a backend error")
	}
}

type memberFunc func(ctx context.Context, tenant, group string) ([]string, error)

func (f memberFunc) Members(ctx context.Context, tenant, group string) ([]string, error) {
	return f(ctx, tenant, group)
}

func TestDirectMembersNoFallbackReturnsNothing(t *testing.T) {
	d := &DirectMembers{}
	got, err := d.Members(context.Background(), "", "some-group-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("group id with no fallback returned %v", got)
	}
	if ok, _ := d.IsMember(context.Background(), "", "alice", "some-group-id"); ok {
		t.Error("group with no fallback granted membership")
	}
}
