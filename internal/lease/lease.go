// Package lease runs podsmedic under Kubernetes leader election, so more than
// one replica can exist without two of them healing the same cluster.
//
// # Why podsmedic needed this
//
// The circuit breaker and the dedupe caches are deliberately in memory. That is
// a good trade for state that only has to survive between sweeps, but it means
// two replicas cannot safely run at once: each would have its own idea of how
// many times a workload had been healed, and the per-workload limits that stop
// a heal loop would both be half-enforced. So podsmedic has always documented
// "exactly one replica", which makes a node failure a total outage of the thing
// that is supposed to notice node failures.
//
// Leader election resolves that without touching the in-memory design: the
// standby holds no state at all, because it does nothing. On failover the new
// leader starts with empty caches, which is exactly the state a restart already
// produces and which the persisted ConfigMaps are there to soften.
//
// # Losing the lease is fatal on purpose
//
// When leadership is lost, Run returns an error rather than waiting to win it
// back. Another replica now owns the cluster, and this process's caches
// describe a period it no longer governs. Exiting hands the kubelet a clean
// restart, and the standby that took over is already correct. Trying to resume
// in place would mean two processes with two different histories both believing
// they were authoritative at some point.
package lease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Options configures the election.
type Options struct {
	// Name is the Lease object; Namespace is podsmedic's own.
	Name      string
	Namespace string
	// Identity distinguishes this replica. Empty falls back to the pod name and
	// then the hostname.
	Identity string

	// LeaseDuration is how long a lease is honoured by a non-leader that has not
	// heard a renewal. RenewDeadline is how long the leader keeps trying to renew
	// before giving up. RetryPeriod is the gap between attempts.
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

// Defaults match the Kubernetes control plane's own, which are tuned for the
// same trade: fast enough that a failover is measured in seconds, slow enough
// that an API server hiccup does not cause one.
const (
	DefaultLeaseDuration = 15 * time.Second
	DefaultRenewDeadline = 10 * time.Second
	DefaultRetryPeriod   = 2 * time.Second
)

// WithDefaults fills in anything unset and resolves the identity.
func (o Options) WithDefaults() Options {
	if o.Name == "" {
		o.Name = "podsmedic-leader"
	}
	if o.LeaseDuration <= 0 {
		o.LeaseDuration = DefaultLeaseDuration
	}
	if o.RenewDeadline <= 0 {
		o.RenewDeadline = DefaultRenewDeadline
	}
	if o.RetryPeriod <= 0 {
		o.RetryPeriod = DefaultRetryPeriod
	}
	if o.Identity == "" {
		o.Identity = identity()
	}
	return o
}

// Validate checks the durations relate to each other correctly.
//
// The ordering is not a style preference: a RenewDeadline at or above the
// LeaseDuration means the leader can still believe it holds a lease that
// another replica has already taken, and both would heal at once. client-go
// rejects this too, but it does so by panicking at startup, which is a poor way
// to learn about a typo in an env var.
func (o Options) Validate() error {
	if o.Namespace == "" {
		return errors.New("namespace is required to hold the lease")
	}
	if o.LeaseDuration <= o.RenewDeadline {
		return fmt.Errorf("lease duration (%s) must be longer than the renew deadline (%s), "+
			"or a leader could still think it holds a lease another replica has taken",
			o.LeaseDuration, o.RenewDeadline)
	}
	if o.RenewDeadline <= o.RetryPeriod {
		return fmt.Errorf("renew deadline (%s) must be longer than the retry period (%s)",
			o.RenewDeadline, o.RetryPeriod)
	}
	return nil
}

// ErrLostLeadership is returned when the lease was lost while the process was
// still meant to be running. The caller should exit rather than continue.
var ErrLostLeadership = errors.New("lost cluster leadership")

// leadShutdownGrace is how long Run waits for the leader's work to unwind after
// leadership is lost, before giving up on a clean stop.
const leadShutdownGrace = 15 * time.Second

// Run campaigns for the lease and calls lead while this replica holds it.
//
// lead is given a context that is cancelled the moment leadership is lost, so
// everything it starts must stop with it. Run returns nil on normal shutdown
// (the parent context ended) and ErrLostLeadership if the lease went to someone
// else.
func Run(ctx context.Context, cs kubernetes.Interface, o Options, log *slog.Logger, lead func(context.Context) error) error {
	o = o.WithDefaults()
	if err := o.Validate(); err != nil {
		return err
	}

	var (
		started atomic.Bool
		done    = make(chan error, 1)
	)

	el, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock: &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Name: o.Name, Namespace: o.Namespace},
			Client:     cs.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{Identity: o.Identity},
		},
		// Hand the lease back on a clean shutdown, so a rolling update fails over
		// in milliseconds instead of waiting out the lease duration.
		ReleaseOnCancel: true,
		LeaseDuration:   o.LeaseDuration,
		RenewDeadline:   o.RenewDeadline,
		RetryPeriod:     o.RetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leadCtx context.Context) {
				started.Store(true)
				log.Info("became leader", "identity", o.Identity, "lease", o.Namespace+"/"+o.Name)
				done <- lead(leadCtx)
			},
			OnStoppedLeading: func() {
				log.Warn("lost leadership", "identity", o.Identity)
			},
			OnNewLeader: func(id string) {
				if id != o.Identity {
					log.Info("standing by", "leader", id, "identity", o.Identity)
				}
			},
		},
	})
	if err != nil {
		return fmt.Errorf("leader election: %w", err)
	}

	log.Info("campaigning for leadership", "identity", o.Identity,
		"lease", o.Namespace+"/"+o.Name, "leaseDuration", o.LeaseDuration)
	el.Run(ctx)

	// el.Run returns once this replica is no longer leading, but the lead
	// callback runs in its own goroutine and may still be unwinding.
	if started.Load() {
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
		case <-time.After(leadShutdownGrace):
			log.Warn("leader work did not stop within the grace period", "grace", leadShutdownGrace)
		}
	}

	if ctx.Err() != nil {
		return nil // ordinary shutdown
	}
	return ErrLostLeadership
}

// identity names this replica. The downward-API pod name is preferred because
// it is stable and meaningful in `kubectl get lease -o yaml`; the hostname is
// the same thing inside a pod and something sensible outside one.
func identity() string {
	if name := os.Getenv("POD_NAME"); name != "" {
		return name
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "podsmedic"
}
