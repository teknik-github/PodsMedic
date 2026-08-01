// Package ui serves the live cluster view: a globe with the workloads around it
// and a wire drawn to each one as something happens to it.
//
// It is off by default and, when on, listens on its own address rather than
// joining the metrics server. That separation is deliberate. /metrics is safe to
// expose — a scraper reads counters. This page shows every workload name, every
// failure, and every change podsmedic made, so it gets its own switch and its
// own port: exposing it must be an explicit choice rather than something an
// operator inherits by enabling metrics.
//
// Binding it anywhere but loopback additionally requires PODSMEDIC_UI_TOKEN, and
// Serve refuses to start without one — see auth.go. There is still no TLS here,
// so on an untrusted network the intended route remains `kubectl port-forward`.
//
// The page is one embedded file with no external fetches — no CDN, no fonts, no
// libraries. A globe of dots is projection arithmetic, which does not justify a
// dependency in a project that has four.
package ui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/teknik-github/PodsMedic/internal/live"
	"github.com/teknik-github/PodsMedic/internal/metrics"
)

//go:embed globe.html login.html
var assets embed.FS

// Server exposes the live view.
type Server struct {
	stream *live.Stream
	source live.Source
	gate   *Gate
	log    *slog.Logger
}

// New builds the server. source supplies the initial picture; stream supplies
// everything that happens afterwards. A nil gate leaves the view open, which
// Serve permits only on a loopback address.
func New(stream *live.Stream, source live.Source, gate *Gate, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{stream: stream, source: source, gate: gate, log: log}
}

// Handler routes the page, its two data endpoints, and the session endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.page)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /api/snapshot", s.guard(s.snapshot))
	mux.HandleFunc("GET /api/events", s.guard(s.events))
	return mux
}

// guard refuses an unauthenticated request to a data endpoint. The page itself
// is not guarded this way — it answers with the login form instead, so a browser
// gets somewhere to type the token rather than a bare 401.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.gate.Authorised(r, time.Now()) {
			http.Error(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// Serve runs the view until the context is cancelled. An empty address disables
// it entirely.
//
// A bind that is reachable from off the machine without a token is refused
// rather than warned about. A warning in a log nobody reads is how a dashboard
// listing every workload ends up on a LAN, and the fix — set a token, or bind
// loopback and port-forward — takes one line either way.
func (s *Server) Serve(ctx context.Context, addr string) error {
	if addr == "" {
		return nil
	}
	if RequiresToken(addr) && !s.gate.Enabled() {
		return fmt.Errorf("live view on %s would be reachable from the network with no authentication: "+
			"set PODSMEDIC_UI_TOKEN, or bind 127.0.0.1 and use kubectl port-forward", addr)
	}
	srv := &http.Server{
		Addr:    addr,
		Handler: s.Handler(),
		// No WriteTimeout: the event stream is a long-lived response and any
		// deadline would sever it mid-flight.
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	if s.gate.Enabled() {
		s.log.Info("live view listening", "addr", addr, "auth", "token")
	} else {
		s.log.Info("live view listening", "addr", addr,
			"auth", "none", "note", "loopback only — reach it with kubectl port-forward")
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !s.gate.Authorised(r, time.Now()) {
		s.writeHTML(w, http.StatusOK, loginPage(""))
		return
	}
	body, err := assets.ReadFile("globe.html")
	if err != nil {
		http.Error(w, "view unavailable", http.StatusInternalServerError)
		return
	}
	s.writeHTML(w, http.StatusOK, withSignOut(body, s.gate.Enabled()))
}

// withSignOut adds the sign-out control, but only when there is a session to
// sign out of. An always-present button on an unauthenticated view would
// suggest a protection that is not there.
func withSignOut(page []byte, enabled bool) []byte {
	if !enabled {
		return page
	}
	return []byte(strings.Replace(string(page), "<!--SIGNOUT-->",
		`<form id="signout" method="post" action="/logout"><button type="submit">sign out</button></form>`, 1))
}

// login exchanges the configured token for a session cookie.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.gate.Enabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.writeHTML(w, http.StatusBadRequest, loginPage("Could not read that form."))
		return
	}
	session, locked := s.gate.Login(r.PostFormValue("token"), peerOf(r), time.Now())
	switch {
	case locked:
		metrics.UILoginsTotal.Inc("throttled")
		s.writeHTML(w, http.StatusTooManyRequests,
			loginPage("Too many failed attempts. Wait a few minutes and try again."))
	case session == "":
		metrics.UILoginsTotal.Inc("rejected")
		s.log.Warn("live view login rejected", "peer", peerOf(r))
		s.writeHTML(w, http.StatusUnauthorized, loginPage("That token is not right."))
	default:
		metrics.UILoginsTotal.Inc("ok")
		http.SetCookie(w, s.sessionCookie(session, DefaultSessionTTL))
		// 303 so a browser reload of the dashboard does not re-post the token.
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// logout drops the session both server-side and in the browser.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		s.gate.Logout(c.Value)
	}
	http.SetCookie(w, s.sessionCookie("", -time.Hour))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// sessionCookie builds the session cookie. Not Secure: the view speaks plain
// HTTP by design (no certificate to manage for something reached over
// port-forward), and marking it Secure would stop the cookie being sent at all.
func (s *Server) sessionCookie(value string, ttl time.Duration) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	}
}

// loginPage renders the sign-in form, with an optional message. The message is
// one of our own fixed strings, never anything the client sent, so there is
// nothing to escape.
func loginPage(msg string) []byte {
	body, err := assets.ReadFile("login.html")
	if err != nil {
		return []byte("<!doctype html><title>podsmedic</title><p>sign-in page unavailable")
	}
	if msg == "" {
		return body
	}
	return []byte(strings.Replace(string(body), "<!--ERROR-->",
		`<p class="err">`+msg+`</p>`, 1))
}

func (s *Server) writeHTML(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page loads nothing from anywhere. Saying so in a header means a
	// tampered copy cannot quietly start doing otherwise. form-action is spelled
	// out because it does not inherit from default-src.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; form-action 'self'")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	snap := live.Snapshot{At: time.Now()}
	if s.source != nil {
		snap = s.source.LiveSnapshot()
	}
	if s.stream != nil {
		snap.Events = s.stream.Recent()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		s.log.Debug("live snapshot write failed", "err", err)
	}
}

// events is a Server-Sent Events feed. SSE rather than a WebSocket because the
// traffic is one-way and SSE is plain HTTP the standard library already speaks —
// no upgrade handshake, no framing, and a browser reconnects on its own.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok || s.stream == nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := s.stream.Subscribe()
	defer cancel()

	// A comment line every so often keeps intermediaries from closing an idle
	// connection, which on a healthy cluster is most of the time.
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case e, open := <-ch:
			if !open {
				return
			}
			payload, err := json.Marshal(e)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
