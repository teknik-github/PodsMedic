package agent

import (
	"context"
	"fmt"

	"github.com/teknik-github/PodsMedic/internal/breaker"
	"github.com/teknik-github/PodsMedic/internal/detect"
	"github.com/teknik-github/PodsMedic/internal/live"
	"github.com/teknik-github/PodsMedic/internal/metrics"
)

// clusterOpts builds the cluster-wide bounds from config.
func (a *Agent) clusterOpts() breaker.ClusterOptions {
	return breaker.ClusterOptions{
		MaxPerSweep:       a.cfg.HealMaxPerSweep,
		SurgeRatio:        a.cfg.HealSurgeRatio,
		SurgeMinWorkloads: a.cfg.HealSurgeMinWorkloads,
	}
}

// workloadFailureCounts reduces this sweep's problems to distinct failing
// workloads, against the total the sweep saw.
//
// Counting workloads rather than pods is the point: one Deployment with thirty
// crash-looping replicas is a single failure, and counting it as thirty would
// trip the surge brake on exactly the case podsmedic handles best.
func workloadFailureCounts(problems []detect.Problem, st *sweepState) (failing, total int) {
	if st == nil {
		return 0, 0
	}
	all := map[string]bool{}
	for i := range st.pods {
		pod := &st.pods[i]
		key := pod.Namespace + "/" + live.WorkloadOf(pod)
		if live.WorkloadOf(pod) == "" {
			key = pod.Namespace + "/" + pod.Name
		}
		all[key] = true
	}

	bad := map[string]bool{}
	for _, p := range problems {
		key := p.Namespace + "/" + p.Pod
		for i := range st.pods {
			pod := &st.pods[i]
			if pod.Namespace == p.Namespace && pod.Name == p.Pod {
				if w := live.WorkloadOf(pod); w != "" {
					key = p.Namespace + "/" + w
				}
				break
			}
		}
		bad[key] = true
	}
	return len(bad), len(all)
}

// announceSurge tells a human once per outage, not once per sweep.
func (a *Agent) announceSurge(ctx context.Context, why string) {
	if a.surgeAnnounced {
		return
	}
	a.surgeAnnounced = true
	metrics.SurgeTrips.Inc()
	a.log.Warn("cluster-wide heal suspension", "reason", why)
	msg := fmt.Sprintf("Healing suspended cluster-wide: %s. podsmedic keeps watching and alerting; it will resume on its own once the failure rate drops.", why)
	if err := a.notifier.Notice(ctx, msg); err != nil {
		a.log.Error("surge notice failed", "err", err)
	}
}
