package ui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teknik-github/PodsMedic/internal/live"
)

type stubSource struct{ snap live.Snapshot }

func (s stubSource) LiveSnapshot() live.Snapshot { return s.snap }

func newTestServer(t *testing.T) (*Server, *live.Stream) {
	t.Helper()
	stream := live.NewStream(10)
	src := stubSource{snap: live.Snapshot{
		Pods: 32, Problems: 3, Incidents: 2, Nodes: 1,
		Workloads: []live.Workload{{Key: "api/Deployment/web", Namespace: "api", Name: "Deployment/web", Pods: 2, Ready: 1}},
	}}
	return New(stream, src, nil, nil), stream
}

func TestPageIsSelfContained(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()

	// The page must fetch nothing from anywhere: it runs behind port-forward on
	// a cluster that may have no egress, and an external script would be a
	// supply-chain hole in a component that can patch workloads.
	for _, external := range []string{"src=\"http", "href=\"http", "@import", "//cdn", "googleapis"} {
		if strings.Contains(body, external) {
			t.Fatalf("page references something external: %q", external)
		}
	}
	if !strings.Contains(body, "<canvas") {
		t.Fatal("expected the canvas the globe draws on")
	}
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Fatalf("expected a restrictive CSP, got %q", got)
	}
}

func TestPageLaysWorkloadsOutEvenly(t *testing.T) {
	// A regression guard for a real defect: the first version placed workloads at
	// an angle hashed from the name. On a live 24-workload cluster two landed
	// 0.0 degrees apart and several within 1.5, so nodes and labels drew on top
	// of each other. Even spacing over a sorted key list cannot do that.
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	if strings.Contains(body, "16777619") {
		t.Fatal("the hashed-angle placement is back; it collides on real clusters")
	}
	if !strings.Contains(body, "function relayout()") {
		t.Fatal("expected the even-spacing layout")
	}
	// Labels cannot all fit around the ring, so the page must filter them
	// rather than draw every one.
	if !strings.Contains(body, "hover") {
		t.Fatal("expected hover labelling for the workloads the filter hides")
	}
}

func TestCanvasHasExplicitSize(t *testing.T) {
	// Regression guard for a bug that made the whole view useless: a <canvas> is
	// a replaced element with an intrinsic 300x150, and `position:fixed; inset:0`
	// does not stretch it. Measured in a real browser, the canvas was 300x150 at
	// (0,0) in a 1440x900 viewport, so the globe drew in the top-left corner.
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	var rule string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "canvas {") {
			rule = line
			break
		}
	}
	if rule == "" {
		t.Fatal("no canvas style rule found")
	}
	if !strings.Contains(rule, "width:") || !strings.Contains(rule, "height:") {
		t.Fatalf("the canvas must be sized explicitly or it stays 300x150: %q", rule)
	}
}

func TestWorkloadsOrbitAndStopOnlyWhenTheyFail(t *testing.T) {
	// The page's primary signal is motion: every workload orbits, and one that
	// has a problem stops dead while its shell-mates carry on past it. A single
	// motionless dot among two dozen moving ones is caught by peripheral vision,
	// which colour alone never is.
	//
	// Verified in a real browser on a live cluster — within the oom-test shell,
	// healthy-demo and probe-demo kept moving while img-demo, oom-demo and
	// wire-demo held still. This guards the pieces that make that work.
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	for _, needed := range []string{
		"function advance(",                    // the per-frame step
		"function stalled(",                    // the one predicate that decides who stops
		"if (!w.shell || stalled(w)) continue", // stopping is per workload, not per shell
	} {
		if !strings.Contains(body, needed) {
			t.Fatalf("the orbit mechanic is missing %q", needed)
		}
	}

	// Angle must survive a snapshot poll. Without this every orbit snaps back to
	// its starting slot once a minute, which reads as the whole cluster
	// twitching and buries the one signal the view exists for.
	if !strings.Contains(body, "angle: prev ? prev.angle : undefined") {
		t.Fatal("orbit angle is not carried across snapshots; every poll would reset the motion")
	}

	// Motion is the signal, so the OS setting that disables motion has to be
	// honoured rather than ignored.
	if !strings.Contains(body, "prefers-reduced-motion") {
		t.Fatal("expected reduced-motion to be respected")
	}

	// A stopped workload must not also pulse: something throbbing reads as
	// alive, which is the opposite of what a halt means.
	if !strings.Contains(body, "if (!stopped && !STILL)") {
		t.Fatal("expected the wake to be dropped for a stopped workload")
	}
}

func TestUnknownPathIs404(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/../etc/passwd", nil))
	if rec.Code == http.StatusOK {
		t.Fatal("only the three known routes should serve")
	}
}

func TestSnapshotCarriesStateAndRecentEvents(t *testing.T) {
	s, stream := newTestServer(t)
	stream.Publish(live.Event{Class: live.ClassProblem, Namespace: "api", Pod: "web-1", Reason: "OOMKilled"})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/snapshot", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var snap live.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Pods != 32 || snap.Problems != 3 || len(snap.Workloads) != 1 {
		t.Fatalf("snapshot lost state: %+v", snap)
	}
	// A viewer opening the page mid-incident must see what already happened,
	// not a blank screen until the next event.
	if len(snap.Events) != 1 || snap.Events[0].Reason != "OOMKilled" {
		t.Fatalf("expected the recent event replayed, got %+v", snap.Events)
	}
}

func TestSnapshotWithoutASourceStillAnswers(t *testing.T) {
	// Before the first sweep there is nothing to describe; the page must still
	// load rather than error.
	s := New(live.NewStream(4), nil, nil, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/snapshot", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestEventStreamDeliversAsServerSentEvents(t *testing.T) {
	s, stream := newTestServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("expected an SSE content type, got %q", ct)
	}

	// Give the handler a moment to register before publishing, or the event
	// races ahead of the subscription.
	time.Sleep(150 * time.Millisecond)
	stream.Publish(live.Event{Class: live.ClassHeal, Namespace: "api", Pod: "web-1", Detail: "memory 128Mi to 384Mi"})

	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		got <- string(buf[:n])
	}()

	select {
	case frame := <-got:
		if !strings.HasPrefix(frame, "data: ") {
			t.Fatalf("expected an SSE data frame, got %q", frame)
		}
		if !strings.Contains(frame, "\"class\":\"heal\"") || !strings.Contains(frame, "128Mi") {
			t.Fatalf("event content lost in transit: %q", frame)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event arrived over the stream")
	}
}

func TestClosingTheViewerReleasesItsSubscription(t *testing.T) {
	// A dashboard left open and closed repeatedly must not leak a channel per
	// visit; podsmedic is a long-running process.
	s, stream := newTestServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
		cancel()
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if stream.Subscribers() == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected every subscription released, %d still attached", stream.Subscribers())
}

func TestServeWithEmptyAddressIsDisabled(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.Serve(context.Background(), ""); err != nil {
		t.Fatalf("an empty address means off, not an error: %v", err)
	}
}
