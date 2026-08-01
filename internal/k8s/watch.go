package k8s

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// watchResync is how often the informer re-lists as a safety net against a
// missed watch event. Updates it generates carry an unchanged object, which the
// transition logic reports as nothing, so a long period costs nothing but a
// list.
const watchResync = 10 * time.Minute

// WatchPods streams pod changes to onChange until the context is cancelled.
//
// This is the only place podsmedic watches rather than polls, and it exists for
// the live view alone: the sweep stays on its interval, because everything it
// does — diagnosis, healing, verification — is deliberately paced. A display
// that only moved once a minute would be a worse lie than no display, so the
// view gets its own feed.
//
// onChange receives (old, current). Either may be nil: a nil old is a pod
// appearing, a nil current is one going away. It is called from informer
// goroutines and must not block.
func (c *Client) WatchPods(ctx context.Context, namespaces []string, onChange func(old, cur *corev1.Pod)) error {
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if p, ok := obj.(*corev1.Pod); ok {
				onChange(nil, p)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			old, _ := oldObj.(*corev1.Pod)
			cur, ok := newObj.(*corev1.Pod)
			if ok {
				onChange(old, cur)
			}
		},
		DeleteFunc: func(obj any) {
			// A watch that fell behind delivers the last known state wrapped in a
			// tombstone rather than the object itself.
			if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tomb.Obj
			}
			if p, ok := obj.(*corev1.Pod); ok {
				onChange(p, nil)
			}
		},
	}

	var factories []informers.SharedInformerFactory
	if len(namespaces) == 0 {
		factories = append(factories, informers.NewSharedInformerFactory(c.cs, watchResync))
	} else {
		// One factory per namespace: the shared factory scopes to a single
		// namespace at a time, and watching everything then filtering would cache
		// pods the operator deliberately excluded.
		for _, ns := range namespaces {
			factories = append(factories, informers.NewSharedInformerFactoryWithOptions(
				c.cs, watchResync, informers.WithNamespace(ns)))
		}
	}

	for _, f := range factories {
		if _, err := f.Core().V1().Pods().Informer().AddEventHandler(handler); err != nil {
			return err
		}
		f.Start(ctx.Done())
	}
	for _, f := range factories {
		f.WaitForCacheSync(ctx.Done())
	}

	<-ctx.Done()
	return ctx.Err()
}
