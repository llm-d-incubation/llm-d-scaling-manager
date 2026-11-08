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

package datastore

import (
	"context"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source/pod"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	poolutil "github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/pool"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	// ErrPoolNotSynced is returned when an EndpointPool is not found in the datastore
	ErrPoolNotSynced = errors.New("EndpointPool not found in datastore")
	errPoolIsNull    = errors.New("EndpointPool object is nil, does not exist")
)

// eppMetricsTokenPath is where the controller Deployment mounts the EPP metrics
// token (config/base/manager/deployment.yaml).
const eppMetricsTokenPath = "/var/run/secrets/epp-metrics/token"

// getEPPMetricsToken reads the EPP metrics token from eppMetricsTokenPath.
// This token is used for authenticating with EPP pods when scraping metrics.
// Returns empty string if the file cannot be read.
func getEPPMetricsToken() string {
	const tokenPath = eppMetricsTokenPath

	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		// Log the error to make misconfiguration/permissions issues visible
		ctrl.Log.Error(err, "Failed to read EPP metrics token - EPP authentication will be disabled",
			"path", tokenPath)
		return ""
	}

	// Trim whitespace and newlines from the token
	token := strings.TrimSpace(string(tokenBytes))

	// Log token info for debugging (without exposing the actual token)
	if token == "" {
		ctrl.Log.V(logging.DEBUG).Info("EPP metrics token file is empty", "path", tokenPath)
	} else {
		ctrl.Log.V(logging.DEBUG).Info("EPP metrics token loaded successfully",
			"path", tokenPath,
			"tokenLength", len(token))
	}

	return token
}

// The datastore is a local cache of relevant data for the given InferencePool (currently all pulled from k8s-api)
// It also tracks namespaces that should be watched for ConfigMaps (namespaces with Variant or InferencePool resources).
type Datastore interface {
	// InferencePool operations
	PoolSet(ctx context.Context, client client.Client, pool *poolutil.EndpointPool) error
	PoolGet(name string) (*poolutil.EndpointPool, error)
	PoolGetMetricsSource(name string) source.MetricsSource
	PoolList() []*poolutil.EndpointPool
	PoolGetFromLabels(namespace string, labels map[string]string) (*poolutil.EndpointPool, error)
	PoolDelete(name string)

	// Clears the store state, happens when the pool gets deleted.
	Clear()

	// Namespace tracking operations
	// Track a resource in a namespace (e.g., Variant or InferencePool)
	// Idempotent: tracking the same resource multiple times has no effect.
	NamespaceTrack(resourceType, resourceName, namespace string)
	// Untrack a resource from a namespace.
	// When the namespace has no more tracked resources, it is removed from tracking.
	NamespaceUntrack(resourceType, resourceName, namespace string)
	// IsNamespaceTracked returns true if the namespace has any tracked resources.
	IsNamespaceTracked(namespace string) bool
	// ListTrackedNamespaces returns all namespaces that have tracked resources.
	ListTrackedNamespaces() []string
}

func NewDatastore(cfg *config.Config) Datastore {
	store := &datastore{
		pools:      &sync.Map{},
		registry:   source.NewSourceRegistry(),
		config:     cfg,
		namespaces: make(map[string]map[string]map[string]struct{}),
	}
	return store
}

type datastore struct {
	pools    *sync.Map
	registry *source.SourceRegistry
	config   *config.Config // Unified configuration (injected from main.go)

	// namespacesMu guards namespaces. One lock, not nested sync.Maps: Untrack's
	// "is the namespace now empty, then drop it" is a check-then-act that must
	// not interleave with a Track into the same namespace, or the Track lands in
	// a map that is then detached and the namespace reads as untracked.
	namespacesMu sync.RWMutex
	namespaces   map[string]map[string]map[string]struct{} // namespace -> resourceType -> resource names
}

// Datastore operations
func (ds *datastore) PoolSet(ctx context.Context, client client.Client, pool *poolutil.EndpointPool) error {
	if pool == nil {
		return errPoolIsNull
	}

	namespacedName := pool.Namespace + "/" + pool.Name

	// Create or update pod source using the EPP metrics token read by getEPPMetricsToken()
	// from the mounted token file at /var/run/secrets/epp-metrics/token. This token
	// is mounted from the epp-metrics service account with minimal privileges and is
	// used for authenticating with EPP pods when scraping metrics.
	//
	// The file is also handed over as BearerTokenFile, which the source re-reads on
	// every scrape: the copy read here is only the fallback. Without that, a
	// rotated token -- the Secret re-created, or a projected token refreshed by
	// the kubelet -- reached no scrape until the pool happened to be set again,
	// and every EPP request in between was rejected with the old one.
	podConfig := pod.PodScrapingSourceConfig{
		ServiceName:      pool.EndpointPicker.ServiceName,
		ServiceNamespace: pool.EndpointPicker.Namespace,
		MetricsPort:      pool.EndpointPicker.MetricsPortNumber,
		BearerToken:      getEPPMetricsToken(),
		BearerTokenFile:  eppMetricsTokenPath,
	}

	podSource, err := pod.NewPodScrapingSource(ctx, client, podConfig)
	if err != nil {
		return err
	}

	// Update or register in registry (idempotent operation)
	if err := ds.registry.Update(namespacedName, podSource); err != nil {
		return err
	}

	// Store in the datastore with namespace-scoped key
	ds.pools.Store(pool.Namespace+"/"+pool.Name, pool)
	return nil
}

func (ds *datastore) PoolGet(namespacedName string) (*poolutil.EndpointPool, error) {

	pool, exist := ds.pools.Load(namespacedName)
	if !exist {
		return nil, ErrPoolNotSynced
	}

	epp := pool.(*poolutil.EndpointPool)
	return epp, nil
}

func (ds *datastore) PoolGetMetricsSource(namespacedName string) source.MetricsSource {
	source := ds.registry.Get(namespacedName)
	return source
}

func (ds *datastore) PoolGetFromLabels(namespace string, labels map[string]string) (*poolutil.EndpointPool, error) {
	exist := false
	var ep *poolutil.EndpointPool

	ds.pools.Range(func(k, v any) bool {
		ep = v.(*poolutil.EndpointPool)

		// Filter by namespace first to avoid cross-namespace matches
		if ep.Namespace != namespace {
			return true // Continue iteration
		}

		found := poolutil.IsSubset(ep.Selector, labels)
		if found {
			exist = true
			return false // Stop iteration - found a match
		}
		return true
	})

	if !exist {
		return nil, ErrPoolNotSynced
	}
	return ep, nil
}

func (ds *datastore) PoolList() []*poolutil.EndpointPool {
	res := []*poolutil.EndpointPool{}
	ds.pools.Range(func(k, v any) bool {
		res = append(res, v.(*poolutil.EndpointPool))
		return true
	})

	return res
}

func (ds *datastore) PoolDelete(namespacedName string) {
	ds.pools.Delete(namespacedName)
	// Clean up the metrics source from the registry
	if err := ds.registry.Unregister(namespacedName); err != nil {
		ctrl.Log.V(logging.DEBUG).Info("Failed to unregister metrics source", "namespacedName", namespacedName, "error", err)
	}
}

func (ds *datastore) Clear() {
	ds.pools.Clear()
}

// Namespace tracking operations

// NamespaceTrack adds a resource to the namespace tracker.
// Idempotent: tracking the same resource multiple times (e.g., on retry) has no effect.
// Thread-safe: the whole update runs under namespacesMu.
func (ds *datastore) NamespaceTrack(resourceType, resourceName, namespace string) {
	if namespace == "" || resourceName == "" || resourceType == "" {
		return
	}

	ds.namespacesMu.Lock()
	defer ds.namespacesMu.Unlock()

	types := ds.namespaces[namespace]
	if types == nil {
		types = make(map[string]map[string]struct{})
		ds.namespaces[namespace] = types
	}
	names := types[resourceType]
	if names == nil {
		names = make(map[string]struct{})
		types[resourceType] = names
	}
	names[resourceName] = struct{}{}
}

// NamespaceUntrack removes a resource from the namespace tracker.
// When the namespace has no more tracked resources, it is removed from tracking.
// Thread-safe: the removal and the emptiness check run under one lock, so a
// concurrent NamespaceTrack is either seen by the check or lands afterwards.
func (ds *datastore) NamespaceUntrack(resourceType, resourceName, namespace string) {
	if namespace == "" || resourceName == "" || resourceType == "" {
		return
	}

	ds.namespacesMu.Lock()
	defer ds.namespacesMu.Unlock()

	types, ok := ds.namespaces[namespace]
	if !ok {
		return
	}
	names, ok := types[resourceType]
	if !ok {
		return
	}
	delete(names, resourceName)
	if len(names) == 0 {
		delete(types, resourceType)
	}
	if len(types) == 0 {
		delete(ds.namespaces, namespace)
	}
}

// IsNamespaceTracked returns true if the namespace has any tracked resources.
// Thread-safe.
func (ds *datastore) IsNamespaceTracked(namespace string) bool {
	if namespace == "" {
		return false
	}

	ds.namespacesMu.RLock()
	defer ds.namespacesMu.RUnlock()
	_, exists := ds.namespaces[namespace]
	return exists
}

// ListTrackedNamespaces returns all namespaces that have tracked resources.
// Thread-safe.
func (ds *datastore) ListTrackedNamespaces() []string {
	ds.namespacesMu.RLock()
	defer ds.namespacesMu.RUnlock()
	return slices.Collect(maps.Keys(ds.namespaces))
}
