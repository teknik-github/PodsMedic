package live

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var at = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func pod(name string) *corev1.Pod {
	yes := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "api",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-6b4f9c7d5", Controller: &yes}},
		},
		Spec:   corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func ready(p *corev1.Pod, v bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if v {
		status = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	return p
}

func container(p *corev1.Pod, name string, restarts int32) *corev1.Pod {
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: name, RestartCount: restarts}}
	return p
}

func classes(evs []Event) []Class {
	out := make([]Class, len(evs))
	for i, e := range evs {
		out[i] = e.Class
	}
	return out
}

func has(evs []Event, c Class) bool {
	for _, e := range evs {
		if e.Class == c {
			return true
		}
	}
	return false
}

// --- Transitions -----------------------------------------------------------

func TestNoEventsWhenNothingChanged(t *testing.T) {
	// The bar that matters: a watch fires on every status write, and Kubernetes
	// writes pod status constantly. An identical pod must produce silence, or the
	// display never stops flickering.
	old := ready(container(pod("web-1"), "app", 3), true)
	cur := ready(container(pod("web-1"), "app", 3), true)

	if got := Transitions(old, cur, at); len(got) != 0 {
		t.Fatalf("expected no events, got %v", classes(got))
	}
}

func TestRestartNamesTheTerminationReason(t *testing.T) {
	old := ready(container(pod("web-1"), "app", 3), true)
	cur := ready(container(pod("web-1"), "app", 4), true)
	cur.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137},
	}

	got := Transitions(old, cur, at)
	if len(got) != 1 {
		t.Fatalf("expected one event, got %v", classes(got))
	}
	// An OOM kill is a failure, not routine churn, so it reads red not amber.
	if got[0].Class != ClassProblem {
		t.Fatalf("expected an OOM restart to be a problem, got %s", got[0].Class)
	}
	if got[0].Reason != "OOMKilled" {
		t.Fatalf("expected the termination reason, got %q", got[0].Reason)
	}
	if got[0].Detail == "" {
		t.Fatal("expected a human detail line")
	}
}

func TestOrdinaryRestartIsNotAProblem(t *testing.T) {
	old := ready(container(pod("web-1"), "app", 1), true)
	cur := ready(container(pod("web-1"), "app", 2), true)
	cur.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0},
	}

	got := Transitions(old, cur, at)
	if !has(got, ClassRestart) || has(got, ClassProblem) {
		t.Fatalf("expected a plain restart, got %v", classes(got))
	}
}

func TestRestartCountGoingDownIsNotARestart(t *testing.T) {
	// A replaced pod reuses the container name with a fresh counter. Treating
	// that as a restart would invent an event that never happened.
	old := ready(container(pod("web-1"), "app", 7), true)
	cur := ready(container(pod("web-1"), "app", 0), true)

	if has(Transitions(old, cur, at), ClassRestart) {
		t.Fatal("a lower restart count is not a restart")
	}
}

func TestReadinessFlipsBothWays(t *testing.T) {
	broke := Transitions(ready(pod("web-1"), true), ready(pod("web-1"), false), at)
	if !has(broke, ClassProblem) {
		t.Fatalf("losing readiness should be a problem, got %v", classes(broke))
	}
	fixed := Transitions(ready(pod("web-1"), false), ready(pod("web-1"), true), at)
	if !has(fixed, ClassRecovery) {
		t.Fatalf("regaining readiness should be a recovery, got %v", classes(fixed))
	}
}

func TestWaitingReasonEmitsOnceOnEntry(t *testing.T) {
	waiting := func(reason string) *corev1.Pod {
		p := ready(pod("web-1"), false)
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "app",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
		}}
		return p
	}

	entered := Transitions(waiting(""), waiting("CrashLoopBackOff"), at)
	if !has(entered, ClassProblem) {
		t.Fatalf("entering CrashLoopBackOff should emit, got %v", classes(entered))
	}
	// Still in the same state on the next watch update: silence.
	stayed := Transitions(waiting("CrashLoopBackOff"), waiting("CrashLoopBackOff"), at)
	for _, e := range stayed {
		if e.Reason == "CrashLoopBackOff" {
			t.Fatal("a re-reported waiting reason must not emit again")
		}
	}
}

func TestStartupWaitingReasonsAreIgnored(t *testing.T) {
	// Every rollout passes through these. Drawing them would bury real failures.
	for _, reason := range []string{"ContainerCreating", "PodInitializing"} {
		before := ready(pod("web-1"), false)
		cur := ready(pod("web-1"), false)
		cur.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "app",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
		}}
		for _, e := range Transitions(before, cur, at) {
			if e.Reason == reason {
				t.Fatalf("%s is routine startup and must not emit", reason)
			}
		}
	}
}

func TestAppearingAndDisappearing(t *testing.T) {
	// A new pod is not news — a rollout creates many.
	if got := Transitions(nil, ready(pod("web-1"), true), at); len(got) != 0 {
		t.Fatalf("a new pod should be quiet, got %v", classes(got))
	}
	got := Transitions(ready(pod("web-1"), true), nil, at)
	if len(got) != 1 || got[0].Class != ClassGone {
		t.Fatalf("expected one gone event, got %v", classes(got))
	}
	if got[0].Pod != "web-1" {
		t.Fatalf("the gone event must identify the pod, got %+v", got[0])
	}
	if len(Transitions(nil, nil, at)) != 0 {
		t.Fatal("two nils is not an event")
	}
}

func TestEvictionIsAProblem(t *testing.T) {
	old := ready(pod("web-1"), true)
	cur := ready(pod("web-1"), false)
	cur.Status.Phase = corev1.PodFailed
	cur.Status.Reason = "Evicted"

	got := Transitions(old, cur, at)
	if !has(got, ClassProblem) {
		t.Fatalf("expected a problem, got %v", classes(got))
	}
}

// --- Workload grouping -----------------------------------------------------

func TestWorkloadCollapsesReplicaSetHash(t *testing.T) {
	// Successive rollouts must collapse to one node in the view, not sprout a
	// new one per ReplicaSet.
	if got := WorkloadOf(pod("web-1")); got != "Deployment/web" {
		t.Fatalf("got %q, want %q", got, "Deployment/web")
	}
}

func TestWorkloadKeepsNonHashNames(t *testing.T) {
	yes := true
	cases := map[string]string{
		"StatefulSet/db":  "db",
		"DaemonSet/agent": "agent",
	}
	for want, name := range cases {
		p := pod("x")
		kind := want[:len(want)-len(name)-1]
		p.OwnerReferences = []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &yes}}
		if got := WorkloadOf(p); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}

	// A hand-named ReplicaSet has no generated suffix to trim.
	p := pod("x")
	p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "legacy", Controller: &yes}}
	if got := WorkloadOf(p); got != "ReplicaSet/legacy" {
		t.Fatalf("got %q, want %q", got, "ReplicaSet/legacy")
	}
}

func TestWorkloadOfBarePod(t *testing.T) {
	p := pod("x")
	p.OwnerReferences = nil
	if got := WorkloadOf(p); got != "" {
		t.Fatalf("a bare pod has no workload, got %q", got)
	}
	if WorkloadOf(nil) != "" {
		t.Fatal("nil pod must not panic")
	}
}

// --- Stream ----------------------------------------------------------------

func TestStreamRetainsBoundedRecentEvents(t *testing.T) {
	s := NewStream(3)
	for i := 0; i < 10; i++ {
		s.Publish(Event{Class: ClassProblem, Pod: string(rune('a' + i))})
	}
	got := s.Recent()
	if len(got) != 3 {
		t.Fatalf("expected the ring capped at 3, got %d", len(got))
	}
	// Oldest first, and the oldest retained is the 8th published.
	if got[0].Pod != "h" || got[2].Pod != "j" {
		t.Fatalf("expected h,i,j oldest-first, got %s,%s,%s", got[0].Pod, got[1].Pod, got[2].Pod)
	}
}

func TestSubscriberReceivesPublishedEvents(t *testing.T) {
	s := NewStream(10)
	ch, cancel := s.Subscribe()
	defer cancel()

	s.Publish(Event{Class: ClassHeal, Pod: "web-1"})
	select {
	case e := <-ch:
		if e.Class != ClassHeal || e.Pod != "web-1" {
			t.Fatalf("unexpected event %+v", e)
		}
		if e.At.IsZero() {
			t.Fatal("publish should stamp a time when the caller did not")
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber received nothing")
	}
}

func TestPublishNeverBlocksOnASlowSubscriber(t *testing.T) {
	// The load-bearing property: this is called from the sweep and from an
	// informer callback. A browser tab on a sleeping laptop must not stall either.
	s := NewStream(10)
	_, cancel := s.Subscribe() // never drained
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10_000; i++ {
			s.Publish(Event{Class: ClassProblem})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that stopped reading")
	}
	if s.Dropped() == 0 {
		t.Fatal("expected dropped events to be counted, not silently lost")
	}
}

func TestCancelDetachesSubscriber(t *testing.T) {
	s := NewStream(10)
	ch, cancel := s.Subscribe()
	if s.Subscribers() != 1 {
		t.Fatalf("expected 1 subscriber, got %d", s.Subscribers())
	}
	cancel()
	if s.Subscribers() != 0 {
		t.Fatalf("expected 0 subscribers after cancel, got %d", s.Subscribers())
	}
	if _, open := <-ch; open {
		t.Fatal("the channel should be closed so the reader unblocks")
	}
	cancel() // must be safe twice: an HTTP handler may defer it and also return early
}

func TestNilStreamIsSafe(t *testing.T) {
	// Callers publish unconditionally; the UI being off must not mean nil checks
	// scattered through the agent.
	var s *Stream
	s.Publish(Event{Class: ClassHeal})
}

func TestClassOrigin(t *testing.T) {
	for _, c := range []Class{ClassDiagnose, ClassHeal, ClassVerify, ClassRollback, ClassDeclined} {
		if !c.FromPodsmedic() {
			t.Fatalf("%s should be a podsmedic action", c)
		}
	}
	for _, c := range []Class{ClassProblem, ClassRestart, ClassRecovery, ClassGone} {
		if c.FromPodsmedic() {
			t.Fatalf("%s should be a cluster event", c)
		}
	}
}

func TestEventKeyGroupsByWorkload(t *testing.T) {
	withWorkload := Event{Namespace: "api", Pod: "web-1", Workload: "Deployment/web"}
	if got := withWorkload.Key(); got != "api/Deployment/web" {
		t.Fatalf("got %q", got)
	}
	bare := Event{Namespace: "api", Pod: "lonely"}
	if got := bare.Key(); got != "api/lonely" {
		t.Fatalf("a bare pod should key on itself, got %q", got)
	}
}

// --- Suppressor ------------------------------------------------------------

func TestSuppressorDropsIdenticalRepeats(t *testing.T) {
	// The case that motivated it: the heal-retry loop re-declines the same
	// workload for the same reason every sweep.
	s := NewSuppressor(10 * time.Minute)
	decline := Event{Class: ClassDeclined, Namespace: "api", Workload: "Deployment/web",
		Reason: "declined", Detail: "no safe automated action"}

	if !s.Allow(decline, at) {
		t.Fatal("the first occurrence must get through")
	}
	for i := 1; i <= 5; i++ {
		if s.Allow(decline, at.Add(time.Duration(i)*time.Minute)) {
			t.Fatalf("repeat %d within the window should be dropped", i)
		}
	}
	if !s.Allow(decline, at.Add(11*time.Minute)) {
		t.Fatal("after the window it is news again")
	}
}

func TestSuppressorKeepsGenuinelyDifferentEvents(t *testing.T) {
	s := NewSuppressor(10 * time.Minute)
	base := Event{Class: ClassDeclined, Namespace: "api", Workload: "Deployment/web", Reason: "declined"}

	if !s.Allow(base, at) {
		t.Fatal("first should pass")
	}
	// A different workload, reason, or detail is a different thing to report.
	other := base
	other.Workload = "Deployment/api"
	if !s.Allow(other, at) {
		t.Fatal("a different workload must not be suppressed")
	}
	changed := base
	changed.Detail = "namespace not in the heal allowlist"
	if !s.Allow(changed, at) {
		t.Fatal("a different reason must not be suppressed")
	}
}

func TestSuppressorForgetsOldKeys(t *testing.T) {
	// A long-running agent must not accumulate a key per workload that ever
	// hiccuped.
	s := NewSuppressor(time.Minute)
	for i := 0; i < 500; i++ {
		s.Allow(Event{Class: ClassDeclined, Pod: string(rune(i))}, at)
	}
	s.Allow(Event{Class: ClassDeclined, Pod: "later"}, at.Add(2*time.Minute))

	s.mu.Lock()
	n := len(s.seen)
	s.mu.Unlock()
	if n > 2 {
		t.Fatalf("expected expired keys dropped, %d retained", n)
	}
}

func TestNilSuppressorAllowsEverything(t *testing.T) {
	var s *Suppressor
	if !s.Allow(Event{Class: ClassHeal}, at) {
		t.Fatal("a nil suppressor must not block")
	}
}
