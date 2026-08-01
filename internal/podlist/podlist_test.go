package podlist

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var now = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func pod(ns, name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name,
			CreationTimestamp: metav1.NewTime(now.Add(-2 * time.Hour)),
		},
		Spec:   corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func running(ns, name string, ready bool) *corev1.Pod {
	p := pod(ns, name, corev1.PodRunning)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Ready: ready}}
	return p
}

func waiting(ns, name, reason string) *corev1.Pod {
	p := pod(ns, name, corev1.PodRunning)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "app", Ready: false, RestartCount: 7,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
	}}
	return p
}

func list(ps ...*corev1.Pod) []corev1.Pod {
	out := make([]corev1.Pod, 0, len(ps))
	for _, p := range ps {
		out = append(out, *p)
	}
	return out
}

func find(l Listing, name string) (Status, bool) {
	for _, s := range l.Matched {
		if s.Name == name {
			return s, true
		}
	}
	return Status{}, false
}

func TestRunningAndReadyIsHealthy(t *testing.T) {
	l := Summarize(list(running("api", "web-1", true)), Options{}, now)
	s, _ := find(l, "web-1")
	if s.State != "Running" || !s.Healthy {
		t.Fatalf("want a healthy Running pod, got %+v", s)
	}
	if len(l.Unhealthy) != 0 {
		t.Fatalf("nothing should need attention, got %+v", l.Unhealthy)
	}
}

func TestRunningButNotReadyIsNotHealthy(t *testing.T) {
	// Phase Running is not the same as working. A pod whose readiness probe
	// fails serves no traffic, and reporting it as Running is the single most
	// misleading thing this package could do.
	l := Summarize(list(running("api", "web-1", false)), Options{}, now)
	s, _ := find(l, "web-1")
	if s.Healthy {
		t.Fatal("a pod that is not ready is not healthy")
	}
	if s.State != "NotReady" {
		t.Fatalf("want NotReady, got %q", s.State)
	}
}

func TestWaitingReasonBeatsThePhase(t *testing.T) {
	// CrashLoopBackOff lives in the container's waiting reason while the phase
	// still says Running. Printing the phase would hide the actual failure.
	for _, reason := range []string{"CrashLoopBackOff", "ImagePullBackOff", "CreateContainerConfigError"} {
		l := Summarize(list(waiting("api", "web-1", reason)), Options{}, now)
		s, _ := find(l, "web-1")
		if s.State != reason {
			t.Errorf("want %q, got %q", reason, s.State)
		}
		if s.Healthy {
			t.Errorf("%s must not read as healthy", reason)
		}
		if s.Restarts != 7 {
			t.Errorf("restart count lost: %d", s.Restarts)
		}
	}
}

func TestPodInitializingIsNotReportedAsTrouble(t *testing.T) {
	// Every pod passes through it on the way up; surfacing it would flag the
	// whole cluster during a rollout.
	l := Summarize(list(waiting("api", "web-1", "PodInitializing")), Options{}, now)
	s, _ := find(l, "web-1")
	if s.State == "PodInitializing" {
		t.Fatalf("PodInitializing is normal startup, got state %q", s.State)
	}
}

func TestCompletedJobIsHealthy(t *testing.T) {
	// A finished Job is not a broken pod. This project already got this wrong
	// once in the live view, where completed Jobs showed as permanently
	// degraded.
	l := Summarize(list(pod("kube-system", "helm-install", corev1.PodSucceeded)), Options{}, now)
	s, _ := find(l, "helm-install")
	if s.State != "Completed" || !s.Healthy {
		t.Fatalf("a completed Job must not read as a problem, got %+v", s)
	}
}

func TestEvictedIsDistinguishedFromFailed(t *testing.T) {
	// They call for different responses: eviction is the node's doing, a plain
	// failure is the workload's.
	p := pod("api", "web-1", corev1.PodFailed)
	p.Status.Reason = "Evicted"
	l := Summarize(list(p), Options{}, now)
	s, _ := find(l, "web-1")
	if s.State != "Evicted" || s.Healthy {
		t.Fatalf("want an unhealthy Evicted pod, got %+v", s)
	}
}

func TestTerminatingIsNotAFailure(t *testing.T) {
	// A pod being deleted on purpose reports whatever its containers happened to
	// be doing when the kubelet started killing them. Flagging that would make
	// every rollout look like an incident.
	p := running("api", "web-1", false)
	del := metav1.NewTime(now)
	p.DeletionTimestamp = &del
	l := Summarize(list(p), Options{}, now)
	s, _ := find(l, "web-1")
	if s.State != "Terminating" || !s.Healthy {
		t.Fatalf("want a healthy Terminating pod, got %+v", s)
	}
}

func TestPreviousOOMKillIsSurfaced(t *testing.T) {
	// A container restarted after an OOM kill is Running again and otherwise
	// invisible until it happens a second time.
	p := pod("api", "web-1", corev1.PodRunning)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "app", Ready: false, RestartCount: 3,
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"},
		},
	}}
	l := Summarize(list(p), Options{}, now)
	s, _ := find(l, "web-1")
	if s.State != "OOMKilled" {
		t.Fatalf("want OOMKilled surfaced, got %q", s.State)
	}
}

func TestFilterMatchesNamespaceOrName(t *testing.T) {
	pods := list(
		running("longhorn-system", "csi-plugin-a", true),
		running("api", "web-1", true),
		running("api", "longhorn-client", true),
	)
	if got := Summarize(pods, Options{Filter: "longhorn"}, now); len(got.Matched) != 2 {
		t.Fatalf("filter should match a namespace and a name, got %d", len(got.Matched))
	}
	if got := Summarize(pods, Options{Filter: "API"}, now); len(got.Matched) != 2 {
		t.Fatalf("filter should be case-insensitive, got %d", len(got.Matched))
	}
	if got := Summarize(pods, Options{Filter: "all"}, now); len(got.Matched) != 3 {
		t.Fatalf(`"all" should mean no filter, got %d`, len(got.Matched))
	}
}

func TestUnhealthyPodsSortFirst(t *testing.T) {
	pods := list(
		running("a", "healthy", true),
		waiting("z", "broken", "CrashLoopBackOff"),
	)
	l := Summarize(pods, Options{}, now)
	if l.Matched[0].Name != "broken" {
		t.Fatalf("the failure must lead the list, got %q", l.Matched[0].Name)
	}
}

func TestTextLeadsWithFailuresAndSummarisesTheRest(t *testing.T) {
	pods := list(
		running("api", "web-1", true),
		running("api", "web-2", true),
		waiting("api", "broken", "CrashLoopBackOff"),
	)
	out := Summarize(pods, Options{}, now).Text()
	if !strings.Contains(out, "Needs attention (1)") {
		t.Fatalf("the failure should be called out: %q", out)
	}
	if !strings.Contains(out, "CrashLoopBackOff") || !strings.Contains(out, "api/broken") {
		t.Fatalf("the failing pod must be named with its reason: %q", out)
	}
	// Unfiltered, healthy pods are counted rather than listed — thirty healthy
	// lines would bury the one that matters.
	if strings.Contains(out, "api/web-1") {
		t.Fatalf("healthy pods should be summarised, not listed: %q", out)
	}
}

func TestFilteredTextListsHealthyPodsToo(t *testing.T) {
	// Asking about one namespace means wanting to see it, not a count.
	pods := list(running("api", "web-1", true), running("db", "pg-0", true))
	out := Summarize(pods, Options{Filter: "api"}, now).Text()
	if !strings.Contains(out, "api/web-1") {
		t.Fatalf("a filtered listing should name its pods: %q", out)
	}
	if strings.Contains(out, "db/pg-0") {
		t.Fatalf("the filter leaked: %q", out)
	}
}

func TestHealthyClusterSaysSoPlainly(t *testing.T) {
	out := Summarize(list(running("api", "web-1", true)), Options{}, now).Text()
	if !strings.Contains(out, "Nothing is failing") {
		t.Fatalf("a clean cluster should say so: %q", out)
	}
}

func TestEmptyMatchExplainsItself(t *testing.T) {
	// "No pods match" alone reads as "the cluster is empty", which is a very
	// different thing from "your filter was wrong".
	out := Summarize(list(running("api", "web-1", true)), Options{Filter: "nope"}, now).Text()
	if !strings.Contains(out, "watching 1 pod") {
		t.Fatalf("an empty match must distinguish itself from an empty cluster: %q", out)
	}
}

func TestListingIsCapped(t *testing.T) {
	var pods []corev1.Pod
	for i := 0; i < 50; i++ {
		pods = append(pods, *waiting("api", "broken-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "CrashLoopBackOff"))
	}
	out := Summarize(pods, Options{Max: 5}, now).Text()
	if !strings.Contains(out, "…and 45 more") {
		t.Fatalf("a long list must be capped and say so: %q", out)
	}
}

func TestStateCountsAreStablyOrdered(t *testing.T) {
	// Two replies about the same cluster must read the same.
	pods := list(
		running("a", "p1", true), running("a", "p2", true),
		pod("a", "p3", corev1.PodSucceeded),
	)
	first := Summarize(pods, Options{}, now).Text()
	for i := 0; i < 5; i++ {
		if got := Summarize(pods, Options{}, now).Text(); got != first {
			t.Fatalf("output is not deterministic:\n%q\n%q", first, got)
		}
	}
	if !strings.Contains(first, "2 Running, 1 Completed") {
		t.Fatalf("want commonest state first: %q", first)
	}
}
