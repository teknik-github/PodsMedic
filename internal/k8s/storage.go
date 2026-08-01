package k8s

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// isNotFound distinguishes "this claim does not exist" — itself a complete
// diagnosis — from a read that failed for any other reason.
func isNotFound(err error) bool { return errors.IsNotFound(err) }

// ClaimSummary is one PersistentVolumeClaim a pod mounts, as a human would read
// it: what it asked for, whether it got it, and what the cluster said about why
// not.
//
// Events are the point of this type. A Pending claim's pod status says only
// "unbound"; the *reason* — no such StorageClass, provisioner quota exhausted,
// no matching volume — is only ever in the claim's own events, which nothing
// else in the bundle would surface.
type ClaimSummary struct {
	// Volume is the pod's volume name; ClaimName is the PVC it points at. They
	// differ often enough that showing both saves a lookup.
	Volume    string `json:"volume"`
	ClaimName string `json:"claimName"`
	// Missing is set when the claim does not exist at all, which is a complete
	// diagnosis on its own.
	Missing bool `json:"missing,omitempty"`
	// Unreadable is set when the claim could not be read (usually absent RBAC),
	// so an empty summary is not mistaken for an empty claim.
	Unreadable string `json:"unreadable,omitempty"`

	Phase        string         `json:"phase,omitempty"` // Pending | Bound | Lost
	StorageClass string         `json:"storageClass,omitempty"`
	Requested    string         `json:"requested,omitempty"`
	Capacity     string         `json:"capacity,omitempty"`
	AccessModes  []string       `json:"accessModes,omitempty"`
	VolumeName   string         `json:"boundVolume,omitempty"`
	VolumeMode   string         `json:"volumeMode,omitempty"`
	Age          string         `json:"age,omitempty"`
	Events       []EventSummary `json:"events,omitempty"`

	// Volume detail, present only once bound.
	VolumeNodeAffinity string `json:"boundVolumeNodeAffinity,omitempty"`
	ReclaimPolicy      string `json:"boundVolumeReclaimPolicy,omitempty"`
}

// claims gathers a summary of every PVC the pod mounts. Best-effort throughout:
// a claim that cannot be read is reported as unreadable rather than dropped, so
// the model never mistakes missing RBAC for a healthy volume.
func (c *Client) claims(ctx context.Context, pod *corev1.Pod, maxEvents int) []ClaimSummary {
	var out []ClaimSummary
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		out = append(out, c.claim(ctx, pod.Namespace, v.Name, v.PersistentVolumeClaim.ClaimName, maxEvents))
	}
	return out
}

func (c *Client) claim(ctx context.Context, namespace, volume, name string, maxEvents int) ClaimSummary {
	s := ClaimSummary{Volume: volume, ClaimName: name}

	pvc, err := c.cs.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		// A claim that does not exist is the answer, not an error. Anything else
		// is reported so it is not read as "the claim is fine".
		if isNotFound(err) {
			s.Missing = true
			return s
		}
		s.Unreadable = err.Error()
		return s
	}

	s.Phase = string(pvc.Status.Phase)
	s.VolumeName = pvc.Spec.VolumeName
	if pvc.Spec.StorageClassName != nil {
		s.StorageClass = *pvc.Spec.StorageClassName
	}
	if pvc.Spec.VolumeMode != nil {
		s.VolumeMode = string(*pvc.Spec.VolumeMode)
	}
	if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		s.Requested = q.String()
	}
	if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		s.Capacity = q.String()
	}
	for _, m := range pvc.Spec.AccessModes {
		s.AccessModes = append(s.AccessModes, string(m))
	}
	if !pvc.CreationTimestamp.IsZero() {
		s.Age = time.Since(pvc.CreationTimestamp.Time).Round(time.Second).String()
	}
	s.Events = c.objectEvents(ctx, namespace, "PersistentVolumeClaim", name, maxEvents)

	if pvc.Spec.VolumeName != "" {
		c.describeBoundVolume(ctx, &s, pvc.Spec.VolumeName)
	}
	return s
}

// describeBoundVolume adds the few PV fields that explain a mount failure on an
// already-bound claim — chiefly node affinity, which is what pins a local or
// zonal volume to one node and strands a pod scheduled elsewhere.
func (c *Client) describeBoundVolume(ctx context.Context, s *ClaimSummary, name string) {
	pv, err := c.cs.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return // optional detail; the claim summary already stands on its own
	}
	s.ReclaimPolicy = string(pv.Spec.PersistentVolumeReclaimPolicy)
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return
	}
	var terms []string
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			terms = append(terms, fmt.Sprintf("%s %s %v", expr.Key, expr.Operator, expr.Values))
		}
	}
	sort.Strings(terms)
	for i, t := range terms {
		if i > 0 {
			s.VolumeNodeAffinity += "; "
		}
		s.VolumeNodeAffinity += t
	}
}

// objectEvents returns recent events for one object, newest first. It mirrors
// the pod event collection but keys on an arbitrary kind/name, so a PVC's
// ProvisioningFailed messages reach the bundle.
func (c *Client) objectEvents(ctx context.Context, namespace, kind, name string, max int) []EventSummary {
	list, err := c.cs.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.kind=%s,involvedObject.name=%s", kind, name),
	})
	if err != nil {
		return nil
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool { return eventTime(items[i]).After(eventTime(items[j])) })
	if max > 0 && len(items) > max {
		items = items[:max]
	}

	out := make([]EventSummary, 0, len(items))
	for _, e := range items {
		out = append(out, EventSummary{
			Type:    e.Type,
			Reason:  e.Reason,
			Age:     time.Since(eventTime(e)).Round(time.Second).String(),
			Count:   e.Count,
			Message: e.Message,
		})
	}
	return out
}

// CreatePVC creates a PersistentVolumeClaim. It is the only create podsmedic
// performs, and it is create-only by construction: an already-existing claim
// returns AlreadyExists and nothing is overwritten.
//
// A dry run asks the API server to validate the object — catching a missing
// StorageClass or a malformed size — without persisting it.
func (c *Client) CreatePVC(ctx context.Context, namespace, name, size, storageClass, accessMode string, dryRun bool) error {
	quantity, err := resource.ParseQuantity(size)
	if err != nil {
		return fmt.Errorf("parse claim size %q: %w", size, err)
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			// Labelled so an operator can tell at a glance which claims podsmedic
			// created, and delete them as a set if the change was unwanted.
			Labels: map[string]string{"app.kubernetes.io/created-by": "podsmedic"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.PersistentVolumeAccessMode(accessMode)},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: quantity},
			},
		},
	}
	// An empty class means "use the cluster default", which is expressed by
	// omitting the field — not by setting it to "".
	if storageClass != "" {
		pvc.Spec.StorageClassName = &storageClass
	}

	opts := metav1.CreateOptions{FieldManager: "podsmedic"}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	_, err = c.cs.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, opts)
	return err
}
