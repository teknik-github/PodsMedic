package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/teknik-github/PodsMedic/internal/k8s"
	"github.com/teknik-github/PodsMedic/internal/live"
	"github.com/teknik-github/PodsMedic/internal/metrics"
	"github.com/teknik-github/PodsMedic/internal/rightsize"
)

// rightsizeStateKey is the single ConfigMap data key holding the usage history.
const rightsizeStateKey = "observations.json"

// observeUsage folds this sweep's usage samples into the sizing history.
//
// It reuses the samples already gathered for prediction, so enabling this costs
// no extra API call. The join is against the sweep's pod list because
// k8s.ContainerUsage carries limits but not requests, and a request is exactly
// what a sizing judgement is made against.
func (a *Agent) observeUsage(st *sweepState) {
	if a.rightsize == nil || len(st.usage) == 0 {
		return
	}

	// Index the containers' declared requests by pod and container name.
	type spec struct{ reqCPU, reqMem, limCPU, limMem int64 }
	specs := make(map[string]spec, len(st.pods))
	workloads := make(map[string]string, len(st.pods))
	for i := range st.pods {
		pod := &st.pods[i]
		workloads[pod.Namespace+"/"+pod.Name] = live.WorkloadOf(pod)
		for j := range pod.Spec.Containers {
			c := &pod.Spec.Containers[j]
			specs[pod.Namespace+"/"+pod.Name+"/"+c.Name] = spec{
				reqCPU: c.Resources.Requests.Cpu().MilliValue(),
				reqMem: c.Resources.Requests.Memory().Value(),
				limCPU: c.Resources.Limits.Cpu().MilliValue(),
				limMem: c.Resources.Limits.Memory().Value(),
			}
		}
	}

	samples := make([]rightsize.Sample, 0, len(st.usage))
	for _, u := range st.usage {
		sp, ok := specs[u.Namespace+"/"+u.Pod+"/"+u.Container]
		if !ok {
			continue // an init container or one that has since gone
		}
		samples = append(samples, rightsize.Sample{
			Namespace: u.Namespace,
			// Keyed by workload, not pod: a rollout replaces every pod, and a
			// per-pod history would reset with it.
			Workload:        workloads[u.Namespace+"/"+u.Pod],
			Container:       u.Container,
			CPUMilli:        u.CPUMilli,
			MemBytes:        u.UsageBytes,
			RequestCPUMilli: sp.reqCPU,
			RequestMemBytes: sp.reqMem,
			LimitCPUMilli:   sp.limCPU,
			LimitMemBytes:   sp.limMem,
		})
	}

	now := time.Now()
	a.rightsize.Observe(samples, now)
	// A workload that has been gone longer than the observation window will
	// never produce a finding again, so its history is dead weight.
	a.rightsize.Forget(now.Add(-2 * a.cfg.RightsizeMinWindow))
	metrics.RightsizeTracked.Set(float64(a.rightsize.Tracking()))
}

// rightsizeOptions are the thresholds a container must clear to be judged.
func (a *Agent) rightsizeOptions() rightsize.Options {
	return rightsize.Options{
		MinSamples: a.cfg.RightsizeMinSamples,
		MinWindow:  a.cfg.RightsizeMinWindow,
		OverRatio:  a.cfg.RightsizeOverRatio,
		Headroom:   a.cfg.RightsizeHeadroom,
	}
}

// loadRightsize restores the usage history from a previous run. Without this
// the observation window restarts on every deploy and the report never has
// enough evidence to say anything.
func (a *Agent) loadRightsize(ctx context.Context) {
	if a.rightsize == nil || a.rightsizeCM == "" {
		return
	}
	data, err := a.kube.GetConfigMap(ctx, k8s.Namespace(), a.rightsizeCM)
	if err != nil {
		a.log.Error("rightsize: load failed", "err", err)
		return
	}
	blob := data[rightsizeStateKey]
	if blob == "" {
		return
	}
	var list []rightsize.Observation
	if err := json.Unmarshal([]byte(blob), &list); err != nil {
		a.log.Error("rightsize: decode failed", "err", err)
		return
	}
	a.rightsize.Restore(list)
	a.log.Info("rightsizing history restored", "containers", len(list))
}

// persistRightsize writes the usage history when it has changed.
func (a *Agent) persistRightsize(ctx context.Context) {
	if a.rightsize == nil || a.rightsizeCM == "" || !a.rightsize.Dirty() {
		return
	}
	blob, err := json.Marshal(a.rightsize.Snapshot())
	if err != nil {
		a.log.Error("rightsize: encode failed", "err", err)
		return
	}
	if err := a.kube.PutConfigMap(ctx, k8s.Namespace(), a.rightsizeCM, map[string]string{rightsizeStateKey: string(blob)}); err != nil {
		a.log.Error("rightsize: save failed", "err", err)
		return
	}
	a.rightsize.ClearDirty()
}
