package ui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Gate decides who may see the live view.
//
// The view lists every workload name, every failure, and every change podsmedic
// made to the cluster. That was tolerable while the page was only reachable
// through `kubectl port-forward`, but the moment it binds to anything else it is
// readable by whoever can reach the port. So the rule is: a non-loopback bind
// requires a token, checked here.
//
// The token is compared in constant time and never leaves the process — a
// successful login mints a random session id, and only that id is put in a
// cookie. Sessions live in memory, so a restart logs everyone out. That is the
// right trade for a component that must not grow a session database.
type Gate struct {
	token string

	mu       sync.Mutex
	sessions map[string]time.Time // id -> expiry
	failures map[string][]time.Time
	ttl      time.Duration
}

// SessionCookie is the cookie a logged-in browser carries.
const SessionCookie = "podsmedic_session"

// DefaultSessionTTL is how long a login lasts. Long enough to leave a dashboard
// open for a working day, short enough that a forgotten browser tab does not
// stay authorised forever.
const DefaultSessionTTL = 12 * time.Hour

// Login throttling. A four-character typo should not lock an operator out, but a
// script guessing tokens must not get thousands of attempts either.
const (
	maxFailedLogins = 5
	lockoutWindow   = 5 * time.Minute
	maxTrackedPeers = 1024
	maxSessions     = 256
)

// NewGate builds a gate. An empty token means the gate is open — every request
// passes. Callers decide whether that is acceptable for the address they bind
// (see RequiresToken).
func NewGate(token string, ttl time.Duration) *Gate {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &Gate{
		token:    token,
		sessions: map[string]time.Time{},
		failures: map[string][]time.Time{},
		ttl:      ttl,
	}
}

// Enabled reports whether the gate actually checks anything.
func (g *Gate) Enabled() bool { return g != nil && g.token != "" }

// Authorised reports whether this request may see cluster state. A gate with no
// token authorises everything; a nil gate does too, so call sites stay
// unconditional.
func (g *Gate) Authorised(r *http.Request, now time.Time) bool {
	if !g.Enabled() {
		return true
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	expiry, ok := g.sessions[c.Value]
	if !ok {
		return false
	}
	if !now.Before(expiry) {
		delete(g.sessions, c.Value)
		return false
	}
	return true
}

// Login checks a submitted token and, on success, returns a new session id.
//
// peer identifies the client for throttling — the caller passes the remote
// address, since only it knows whether a proxy header can be trusted (here,
// nothing may be: the view sits behind no proxy by design).
//
// The returned locked flag distinguishes "wrong token" from "too many attempts",
// because telling an operator which one it is saves a support conversation and
// tells an attacker nothing they could not measure anyway.
func (g *Gate) Login(submitted, peer string, now time.Time) (session string, locked bool) {
	if !g.Enabled() {
		return "", false
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.lockedOut(peer, now) {
		return "", true
	}
	// Constant-time so the comparison does not leak the token's length or a
	// matching prefix through timing.
	if subtle.ConstantTimeCompare([]byte(submitted), []byte(g.token)) != 1 {
		g.recordFailure(peer, now)
		return "", false
	}

	delete(g.failures, peer)
	id, err := newSessionID()
	if err != nil {
		// Without a random id there is no safe session to hand out. Refusing is
		// the only correct answer; a predictable id would be worse than no login.
		return "", false
	}
	g.pruneSessions(now)
	g.sessions[id] = now.Add(g.ttl)
	return id, false
}

// Logout forgets a session.
func (g *Gate) Logout(session string) {
	if !g.Enabled() || session == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.sessions, session)
}

// Sessions counts live sessions, for tests and metrics.
func (g *Gate) Sessions(now time.Time) int {
	if !g.Enabled() {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneSessions(now)
	return len(g.sessions)
}

// lockedOut reports whether this peer has spent its attempts. Caller holds the
// lock.
func (g *Gate) lockedOut(peer string, now time.Time) bool {
	return len(g.recentFailures(peer, now)) >= maxFailedLogins
}

// recentFailures returns (and compacts) this peer's failures inside the window.
// Caller holds the lock.
func (g *Gate) recentFailures(peer string, now time.Time) []time.Time {
	cutoff := now.Add(-lockoutWindow)
	kept := g.failures[peer][:0]
	for _, t := range g.failures[peer] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(g.failures, peer)
		return nil
	}
	g.failures[peer] = kept
	return kept
}

// recordFailure charges a failed attempt against a peer. Caller holds the lock.
func (g *Gate) recordFailure(peer string, now time.Time) {
	kept := g.recentFailures(peer, now)
	if len(kept) == 0 && len(g.failures) >= maxTrackedPeers {
		// A flood of distinct source addresses must not grow the map without
		// bound. Dropping the record only costs this one peer its throttle; the
		// token check itself is unaffected.
		return
	}
	g.failures[peer] = append(kept, now)
}

// pruneSessions drops expired sessions, and the oldest ones if the map is
// somehow full. Caller holds the lock.
func (g *Gate) pruneSessions(now time.Time) {
	for id, expiry := range g.sessions {
		if !now.Before(expiry) {
			delete(g.sessions, id)
		}
	}
	for len(g.sessions) >= maxSessions {
		var oldestID string
		var oldest time.Time
		for id, expiry := range g.sessions {
			if oldestID == "" || expiry.Before(oldest) {
				oldestID, oldest = id, expiry
			}
		}
		delete(g.sessions, oldestID)
	}
}

func newSessionID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// peerOf identifies a client for throttling. Only the socket's own address is
// used: X-Forwarded-For is attacker-controlled, and the view is documented as
// sitting behind no proxy, so trusting it would hand out unlimited attempts.
func peerOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RequiresToken reports whether binding to this address exposes the view beyond
// the local machine, and therefore must not run unauthenticated.
//
// An empty host (":3456") binds every interface, which is the case that most
// often surprises people: it reads like "just a port" and means "the whole LAN".
func RequiresToken(addr string) bool {
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.TrimSpace(host)
	switch host {
	case "", "0.0.0.0", "::", "[::]", "*":
		return true
	case "localhost":
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		// A hostname we cannot classify. Assume it resolves off-box: guessing
		// "loopback" here would silently disable the gate.
		return true
	}
	return !ip.IsLoopback()
}
