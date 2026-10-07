package envtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Scope owns only the namespace and objects it successfully created. Register
// cleanup after assertions, even on failure: envtest has no garbage collector.
type Scope struct {
	Client    client.Client
	Namespace *corev1.Namespace
	objects   []client.Object
	timeout   time.Duration
}

func (e *Environment) NewScope(ctx context.Context, prefix string) (*Scope, *corev1.Namespace, error) {
	namespace := &corev1.Namespace{GenerateName: prefix}
	if err := e.Client.Create(ctx, namespace); err != nil {
		return nil, nil, err
	}
	scope := &Scope{Client: e.Client, Namespace: namespace.DeepCopy(), timeout: e.timeout}
	return scope, namespace, nil
}

func (s *Scope) Create(ctx context.Context, object client.Object) error {
	if object.GetNamespace() != s.Namespace.Name {
		return fmt.Errorf("object namespace %q is outside scope %q", object.GetNamespace(), s.Namespace.Name)
	}
	if err := s.Client.Create(ctx, object); err != nil {
		return err
	}
	s.objects = append(s.objects, object.DeepCopyObject().(client.Object))
	return nil
}

// WaitGone uses the uncached API so stale cache data cannot mask deletion.
func (s *Scope) WaitGone(ctx context.Context, object client.Object) error {
	uid := object.GetUID()
	current := object.DeepCopyObject().(client.Object)
	condition := func(ctx context.Context) (bool, error) {
		err := s.Client.Get(ctx, client.ObjectKeyFromObject(object), current)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return err == nil && current.GetUID() != uid, err
	}
	return wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, s.timeout, true, condition)
}

func (s *Scope) Cleanup(ctx context.Context) error {
	var cleanupErr error
	for _, tracked := range slices.Backward(s.objects) {
		object := tracked.DeepCopyObject().(client.Object)
		uid := tracked.GetUID()
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			if err := s.Client.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
				return client.IgnoreNotFound(err)
			}
			if object.GetUID() != uid {
				return fmt.Errorf("ownership changed for %T %s", object, client.ObjectKeyFromObject(object))
			}
			if len(object.GetFinalizers()) > 0 {
				object.SetFinalizers(nil)
				return s.Client.Update(ctx, object)
			}
			return nil
		})
		if err == nil {
			err = client.IgnoreNotFound(s.Client.Delete(ctx, object, client.Preconditions{UID: &uid}))
		}
		if err == nil {
			err = s.WaitGone(ctx, object)
		}
		cleanupErr = errors.Join(cleanupErr, err)
	}
	// Preserve the namespace if any object could not safely be cleaned.
	if cleanupErr != nil {
		return cleanupErr
	}
	// Namespace deletion also needs explicit finalization without a namespace controller.
	namespace := s.Namespace.DeepCopy()
	uid := namespace.UID
	if err := client.IgnoreNotFound(s.Client.Delete(ctx, namespace, client.Preconditions{UID: &uid})); err != nil {
		return errors.Join(cleanupErr, err)
	}
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := s.Client.Get(ctx, client.ObjectKeyFromObject(namespace), namespace); err != nil {
			return client.IgnoreNotFound(err)
		}
		if namespace.UID != uid {
			return fmt.Errorf("namespace ownership changed for %s", namespace.Name)
		}
		namespace.Spec.Finalizers = nil
		return s.Client.SubResource("finalize").Update(ctx, namespace)
	})
	if err == nil {
		err = s.WaitGone(ctx, namespace)
	}
	return errors.Join(cleanupErr, err)
}

// Diagnostics deliberately emits metadata only. Secrets, DSNs, credentials, and
// arbitrary spec/status messages never enter test artifacts.
func (s *Scope) Diagnostics(ctx context.Context, writer io.Writer) error {
	for _, tracked := range s.objects {
		object := tracked.DeepCopyObject().(client.Object)
		err := s.Client.Get(ctx, client.ObjectKeyFromObject(object), object)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(writer, "%T %s/%s generation=%d resourceVersion=%s finalizers=%v deleting=%t\n",
			object, object.GetNamespace(), object.GetName(), object.GetGeneration(),
			object.GetResourceVersion(), object.GetFinalizers(), object.GetDeletionTimestamp() != nil)
		if err != nil {
			return err
		}
	}
	return nil
}
