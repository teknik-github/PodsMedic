package lease

import (
	"strings"
	"testing"
	"time"
)

func opts() Options {
	return Options{
		Name: "podsmedic-leader", Namespace: "podsmedic", Identity: "pod-a",
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
	}
}

func TestDefaultsAreUsable(t *testing.T) {
	o := Options{Namespace: "podsmedic"}.WithDefaults()
	if err := o.Validate(); err != nil {
		t.Fatalf("the defaults must be a valid configuration: %v", err)
	}
	if o.Name == "" || o.Identity == "" {
		t.Fatalf("defaults left something empty: %+v", o)
	}
}

func TestRenewDeadlineMustBeInsideTheLease(t *testing.T) {
	// This is the ordering that matters: if a leader may keep trying to renew
	// for as long as (or longer than) the lease is honoured, it can still
	// believe it leads after another replica has taken over — and two replicas
	// healing the same cluster is the exact failure leader election exists to
	// prevent.
	o := opts()
	o.RenewDeadline = o.LeaseDuration
	err := o.Validate()
	if err == nil {
		t.Fatal("an equal renew deadline and lease duration must be rejected")
	}
	if !strings.Contains(err.Error(), "another replica") {
		t.Fatalf("the error should explain the risk, got %q", err)
	}

	o.RenewDeadline = o.LeaseDuration + time.Second
	if o.Validate() == nil {
		t.Fatal("a renew deadline longer than the lease must be rejected")
	}
}

func TestRetryPeriodMustBeInsideTheRenewDeadline(t *testing.T) {
	o := opts()
	o.RetryPeriod = o.RenewDeadline
	if o.Validate() == nil {
		t.Fatal("a retry period that leaves no room for a retry must be rejected")
	}
}

func TestNamespaceIsRequired(t *testing.T) {
	// A lease with no namespace would silently land in "default", where the
	// service account cannot write it and the failure would look like a
	// permissions bug rather than a configuration one.
	o := opts()
	o.Namespace = ""
	if o.Validate() == nil {
		t.Fatal("a lease needs a namespace to live in")
	}
}

func TestExplicitValuesSurviveDefaulting(t *testing.T) {
	o := opts().WithDefaults()
	if o.LeaseDuration != 15*time.Second || o.Identity != "pod-a" || o.Name != "podsmedic-leader" {
		t.Fatalf("defaulting overwrote an explicit value: %+v", o)
	}
}

func TestIdentityIsNeverEmpty(t *testing.T) {
	// An empty identity makes every replica look like the same holder, so the
	// lease would never actually change hands.
	if got := identity(); got == "" {
		t.Fatal("identity must always resolve to something")
	}
}
