/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/datastore"
	poolutils "github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/pool"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/common"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

type InferencePoolReconciler struct {
	client.Client
	Datastore datastore.Datastore
	PoolGKNN  common.GKNN
}

func (c *InferencePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("group", c.PoolGKNN.Group).V(logutil.DEFAULT)
	ctx = ctrl.LoggerInto(ctx, logger)

	logger.Info("Reconciling InferencePool", "namespace", req.Namespace, "name", req.Name)

	// 1. Initialize a generic client.Object based on the group.
	var obj client.Object
	switch c.PoolGKNN.Group {
	case v1.GroupName:
		obj = &v1.InferencePool{}
	case v1alpha2.GroupName:
		obj = &v1alpha2.InferencePool{}
	default:
		// Handle unsupported groups gracefully.
		return ctrl.Result{}, fmt.Errorf("cannot reconcile InferencePool - unsupported API group: %s", c.PoolGKNN.Group)
	}

	// 2. Perform a single, generic fetch for the object.
	if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("InferencePool not found. Removing pool from datastore")
			c.Datastore.PoolDelete(req.Namespace + "/" + req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("unable to get InferencePool - %w", err)
	}

	// 3. Perform common checks using the client.Object interface.
	if !obj.GetDeletionTimestamp().IsZero() {
		logger.Info("InferencePool is marked for deletion. Removing pool from datastore")
		c.Datastore.PoolDelete(obj.GetNamespace() + "/" + obj.GetName()) // remove the pool from the datastore
		return ctrl.Result{}, nil
	}

	var (
		endpointPool *poolutils.EndpointPool
		err          error
	)

	switch pool := obj.(type) {
	case *v1.InferencePool:
		endpointPool, err = poolutils.InferencePoolToEndpointPool(ctx, c.Client, pool)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to convert InferencePool v1 to EndPointPool - %w", err)
		}
	case *v1alpha2.InferencePool:
		endpointPool, err = poolutils.AlphaInferencePoolToEndpointPool(ctx, c.Client, pool)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to convert InferencePool v1alpha2 to EndPointPool - %w", err)
		}
	default:
		return ctrl.Result{}, fmt.Errorf("unsupported API group: %s", c.PoolGKNN.Group)
	}

	if endpointPool != nil {
		if err := c.Datastore.PoolSet(ctx, c.Client, endpointPool); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add endpoint into the datastore: - %w", err)
		}
	}

	return ctrl.Result{}, nil
}

// ErrInferencePoolCRDNotServed is returned by SetupWithManager when the API
// server does not serve the InferencePool kind of the configured group. The
// controller is then not registered.
var ErrInferencePoolCRDNotServed = errors.New("InferencePool CRD is not served")

// SetupWithManager registers the reconciler for the InferencePool group in
// PoolGKNN, after checking that the API server serves that kind.
//
// The check is what keeps a missing CRD from becoming a crash loop. A
// controller for an unserved kind cannot sync its source: once the replica wins
// the lease it fails after CacheSyncTimeout and the manager exits, so the
// failure shows only after election. When the kind is not served this returns
// an error wrapping ErrInferencePoolCRDNotServed and registers nothing; any
// other lookup error is returned as is.
func (c *InferencePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	var obj client.Object
	var gv schema.GroupVersion
	switch c.PoolGKNN.Group {
	case v1alpha2.GroupName:
		obj, gv = &v1alpha2.InferencePool{}, v1alpha2.SchemeGroupVersion
	case v1.GroupName:
		obj, gv = &v1.InferencePool{}, v1.SchemeGroupVersion
	default:
		return fmt.Errorf("unknown group %s", c.PoolGKNN.Group)
	}

	gk := schema.GroupKind{Group: gv.Group, Kind: inferencePoolKind}
	if _, err := mgr.GetRESTMapper().RESTMapping(gk, gv.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return fmt.Errorf("%w: %s %s: %w", ErrInferencePoolCRDNotServed, gv, inferencePoolKind, err)
		}
		return fmt.Errorf("looking up %s %s: %w", gv, inferencePoolKind, err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(obj, builder.WithPredicates(inferencePoolPredicate())).
		// The EPP metrics port comes from the endpoint picker's Service, not
		// from the pool, so a change to it bumps no pool generation. Before the
		// predicate it reached Reconcile only by accident: the next status write
		// on the pool, or the cache resync hours later. Watching the Service
		// makes it prompt. The manager already caches Services (Reconcile and
		// the scrape source read them through it), so this adds no informer.
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(c.poolsForService)).
		Complete(c)
}

// poolsForService maps an endpoint picker Service to the pools of this
// reconciler's group, in the same namespace, that reference it.
func (c *InferencePoolReconciler) poolsForService(ctx context.Context, svc client.Object) []reconcile.Request {
	var refs []types.NamespacedName
	switch c.PoolGKNN.Group {
	case v1.GroupName:
		var pools v1.InferencePoolList
		if err := c.List(ctx, &pools, client.InNamespace(svc.GetNamespace())); err != nil {
			log.FromContext(ctx).Error(err, "Listing InferencePools for a changed endpoint picker Service",
				"namespace", svc.GetNamespace(), "service", svc.GetName())
			return nil
		}
		for i := range pools.Items {
			if string(pools.Items[i].Spec.EndpointPickerRef.Name) == svc.GetName() {
				refs = append(refs, client.ObjectKeyFromObject(&pools.Items[i]))
			}
		}
	case v1alpha2.GroupName:
		var pools v1alpha2.InferencePoolList
		if err := c.List(ctx, &pools, client.InNamespace(svc.GetNamespace())); err != nil {
			log.FromContext(ctx).Error(err, "Listing InferencePools for a changed endpoint picker Service",
				"namespace", svc.GetNamespace(), "service", svc.GetName())
			return nil
		}
		for i := range pools.Items {
			if string(pools.Items[i].Spec.ExtensionRef.Name) == svc.GetName() {
				refs = append(refs, client.ObjectKeyFromObject(&pools.Items[i]))
			}
		}
	}
	reqs := make([]reconcile.Request, 0, len(refs))
	for _, nn := range refs {
		reqs = append(reqs, reconcile.Request{NamespacedName: nn})
	}
	return reqs
}

// inferencePoolKind is the kind SetupWithManager checks for and watches.
const inferencePoolKind = "InferencePool"

// inferencePoolPredicate admits creates, deletes and spec changes only.
//
// Of the pool, Reconcile reads nothing but the spec (selector, endpoint picker
// reference); the rest of what it reads, the endpoint picker Service, has its
// own watch in SetupWithManager. It rebuilds the pool's scrape source on every
// call, so a status write by a gateway controller, the common update, rebuilt
// it for nothing. Setting a
// deletionTimestamp bumps metadata.generation, so a pending deletion still gets
// through, and a failed reconcile is requeued without passing the predicate.
func inferencePoolPredicate() predicate.Predicate {
	return predicate.GenerationChangedPredicate{}
}
