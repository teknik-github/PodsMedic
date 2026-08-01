package k8s

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/peceldev/podsmedic/internal/nodes"
)

// NodeStates reads every node's conditions and counts the pods placed on each,
// for the pure checker in internal/nodes.
//
// It takes the sweep's already-listed pods rather than listing again, but does
// list nodes itself even though ClusterCapacity also does. That second list is
// deliberate: node health has to work whether or not auto-heal is on, and
// ClusterCapacity is only gathered when it is. Nodes are few and the object is
// small, so the cost is a rounding error against one sweep's pod list.
//
// The pod count uses the whole passed-in list, which on a namespace-scoped run
// undercounts what is really on the node. The number is context for a human
// ("this much of what I watch is at risk"), never an input to a decision, so an
// undercount misleads nobody into acting wrongly.
func (c *Client) NodeStates(ctx context.Context, pods []corev1.Pod) ([]nodes.State, error) {
	list, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	placed := map[string]int{}
	for i := range pods {
		if pods[i].Spec.NodeName != "" && holdsResources(&pods[i]) {
			placed[pods[i].Spec.NodeName]++
		}
	}

	out := make([]nodes.State, 0, len(list.Items))
	for i := range list.Items {
		n := &list.Items[i]
		state := nodes.State{
			Name:           n.Name,
			Unschedulable:  n.Spec.Unschedulable,
			Pods:           placed[n.Name],
			KubeletVersion: n.Status.NodeInfo.KubeletVersion,
			Conditions:     make([]nodes.Condition, 0, len(n.Status.Conditions)),
		}
		for _, cond := range n.Status.Conditions {
			state.Conditions = append(state.Conditions, nodes.Condition{
				Type: string(cond.Type),
				// ConditionUnknown is not True, and for Ready that is the whole
				// point: an unreachable kubelet reports Unknown, which must read
				// as "not ready" rather than as "no opinion".
				Active:  cond.Status == corev1.ConditionTrue,
				Reason:  cond.Reason,
				Message: cond.Message,
				Since:   cond.LastTransitionTime.Time,
			})
		}
		out = append(out, state)
	}
	return out, nil
}
