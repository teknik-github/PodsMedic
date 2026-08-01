package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

func withSession(id string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: id})
	return r
}

func TestNoTokenLeavesTheGateOpen(t *testing.T) {
	// The port-forward case, which must keep working without ceremony.
	g := NewGate("", 0)
	if g.Enabled() {
		t.Fatal("a gate with no token is not enabled")
	}
	if !g.Authorised(httptest.NewRequest(http.MethodGet, "/", nil), now) {
		t.Fatal("an open gate must authorise everything")
	}
}

func TestNilGateAuthorises(t *testing.T) {
	// Call sites stay unconditional; a nil gate is the no-op.
	var g *Gate
	if !g.Authorised(httptest.NewRequest(http.MethodGet, "/", nil), now) {
		t.Fatal("a nil gate must be a no-op, not a refusal")
	}
}

func TestLoginMintsASessionThatAuthorises(t *testing.T) {
	g := NewGate("s3cret", time.Hour)
	id, locked := g.Login("s3cret", "10.0.0.1", now)
	if locked || id == "" {
		t.Fatalf("expected a session, got id=%q locked=%v", id, locked)
	}
	if !g.Authorised(withSession(id), now) {
		t.Fatal("the minted session must authorise")
	}
	if g.Authorised(withSession("some-other-id"), now) {
		t.Fatal("an unknown session must not authorise")
	}
	if g.Authorised(httptest.NewRequest(http.MethodGet, "/", nil), now) {
		t.Fatal("no cookie must not authorise")
	}
}

func TestWrongTokenMintsNothing(t *testing.T) {
	g := NewGate("s3cret", time.Hour)
	if id, _ := g.Login("s3cre", "10.0.0.1", now); id != "" {
		t.Fatal("a near-miss token must not be accepted")
	}
	if id, _ := g.Login("s3cretx", "10.0.0.1", now); id != "" {
		t.Fatal("a token with a matching prefix must not be accepted")
	}
	if id, _ := g.Login("", "10.0.0.1", now); id != "" {
		t.Fatal("an empty submission must not be accepted")
	}
}

func TestSessionExpires(t *testing.T) {
	g := NewGate("s3cret", time.Hour)
	id, _ := g.Login("s3cret", "10.0.0.1", now)
	if !g.Authorised(withSession(id), now.Add(59*time.Minute)) {
		t.Fatal("session expired early")
	}
	if g.Authorised(withSession(id), now.Add(61*time.Minute)) {
		t.Fatal("session outlived its TTL")
	}
}

func TestLogoutEndsTheSessionImmediately(t *testing.T) {
	g := NewGate("s3cret", time.Hour)
	id, _ := g.Login("s3cret", "10.0.0.1", now)
	g.Logout(id)
	if g.Authorised(withSession(id), now) {
		t.Fatal("a signed-out session must stop working at once")
	}
}

func TestRepeatedGuessesAreThrottled(t *testing.T) {
	// Without this the token is brute-forceable at HTTP speed.
	g := NewGate("s3cret", time.Hour)
	for i := 0; i < maxFailedLogins; i++ {
		if _, locked := g.Login("wrong", "10.0.0.1", now); locked {
			t.Fatalf("locked out after only %d attempts", i)
		}
	}
	if _, locked := g.Login("wrong", "10.0.0.1", now); !locked {
		t.Fatal("expected a lockout once the attempts were spent")
	}
	// The lockout must hold even for the right token, or it is no lockout.
	if id, locked := g.Login("s3cret", "10.0.0.1", now); !locked || id != "" {
		t.Fatal("the correct token must not bypass an active lockout")
	}
	// Another client is unaffected.
	if id, locked := g.Login("s3cret", "10.0.0.2", now); locked || id == "" {
		t.Fatal("one peer's lockout must not affect another")
	}
	// And it lifts.
	if id, locked := g.Login("s3cret", "10.0.0.1", now.Add(lockoutWindow+time.Minute)); locked || id == "" {
		t.Fatal("the lockout must expire with its window")
	}
}

func TestSuccessClearsTheFailureCount(t *testing.T) {
	g := NewGate("s3cret", time.Hour)
	for i := 0; i < maxFailedLogins-1; i++ {
		g.Login("wrong", "10.0.0.1", now)
	}
	if _, locked := g.Login("s3cret", "10.0.0.1", now); locked {
		t.Fatal("should still have had an attempt left")
	}
	// A mistyped token before a correct one must not leave the operator one
	// slip away from a lockout for the next five minutes.
	for i := 0; i < maxFailedLogins-1; i++ {
		if _, locked := g.Login("wrong", "10.0.0.1", now); locked {
			t.Fatalf("failures were not reset by the successful login (attempt %d)", i)
		}
	}
}

func TestSessionsAreBounded(t *testing.T) {
	// A long-running agent must not accumulate sessions without limit.
	g := NewGate("s3cret", time.Hour)
	for i := 0; i < maxSessions+50; i++ {
		g.Login("s3cret", "10.0.0.1", now)
	}
	if n := g.Sessions(now); n > maxSessions {
		t.Fatalf("session map grew to %d, past the %d cap", n, maxSessions)
	}
}

func TestRequiresToken(t *testing.T) {
	// This is the function that decides whether an unauthenticated view is
	// allowed to start at all, so every case is spelled out.
	cases := []struct {
		addr string
		want bool
	}{
		{"", false},               // disabled entirely
		{"127.0.0.1:3456", false}, // the port-forward case
		{"localhost:3456", false}, //
		{"[::1]:3456", false},     //
		{":3456", true},           // reads like "a port", means every interface
		{"0.0.0.0:3456", true},    //
		{"[::]:3456", true},       //
		{"10.0.0.5:3456", true},   // a LAN address
		{"some-host:3456", true},  // unclassifiable: assume it is reachable
	}
	for _, c := range cases {
		if got := RequiresToken(c.addr); got != c.want {
			t.Errorf("RequiresToken(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestServeRefusesAnExposedViewWithoutAToken(t *testing.T) {
	// The whole point of the feature: a dashboard listing every workload must
	// not reach the network unauthenticated because a warning went unread.
	s := New(nil, nil, NewGate("", 0), nil)
	err := s.Serve(t.Context(), "0.0.0.0:0")
	if err == nil {
		t.Fatal("expected a refusal, not a warning")
	}
	if !strings.Contains(err.Error(), "PODSMEDIC_UI_TOKEN") {
		t.Fatalf("the error must name the fix, got %q", err)
	}
}

func TestDataEndpointsRefuseWithoutASession(t *testing.T) {
	s := New(nil, nil, NewGate("s3cret", time.Hour), nil)
	for _, path := range []string{"/api/snapshot", "/api/events"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s answered %d without a session; cluster state must not leak", path, rec.Code)
		}
	}
}

func TestPageOffersTheLoginFormInsteadOfCrypticFailure(t *testing.T) {
	s := New(nil, nil, NewGate("s3cret", time.Hour), nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected the sign-in page, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="token"`) {
		t.Fatal("expected somewhere to type the token")
	}
	if strings.Contains(body, "<canvas") {
		t.Fatal("the globe leaked to an unauthenticated visitor")
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "form-action 'self'") {
		t.Fatal("form-action does not inherit from default-src; it must be spelled out")
	}
}

func TestLoginRoundTripReachesTheView(t *testing.T) {
	s, _ := newTestServer(t)
	s.gate = NewGate("s3cret", time.Hour)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("token=s3cret"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected a redirect after signing in, got %d", rec.Code)
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			session = c
		}
	}
	if session == nil || session.Value == "" {
		t.Fatal("no session cookie was set")
	}
	if !session.HttpOnly {
		t.Fatal("the session cookie must be HttpOnly: inline script has no business reading it")
	}

	page := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(session)
	s.Handler().ServeHTTP(page, req)
	if !strings.Contains(page.Body.String(), "<canvas") {
		t.Fatal("a signed-in visitor should get the globe")
	}
	if !strings.Contains(page.Body.String(), `action="/logout"`) {
		t.Fatal("a signed-in visitor needs a way out")
	}
}

func TestSignOutControlIsAbsentWhenThereIsNoGate(t *testing.T) {
	// Offering "sign out" on an open view would imply a protection that is not
	// there.
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rec.Body.String(), `action="/logout"`) {
		t.Fatal("an unauthenticated view must not pretend to have sessions")
	}
}
