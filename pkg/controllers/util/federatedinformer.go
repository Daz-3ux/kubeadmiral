/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

This file may have been modified by The KubeAdmiral Authors
("KubeAdmiral Modifications"). All KubeAdmiral Modifications
are Copyright 2023 The KubeAdmiral Authors.
*/

package util

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	pkgruntime "k8s.io/apimachinery/pkg/runtime"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	fedcorev1a1 "github.com/kubewharf/kubeadmiral/pkg/apis/core/v1alpha1"
	"github.com/kubewharf/kubeadmiral/pkg/client/generic"
	"github.com/kubewharf/kubeadmiral/pkg/controllers/common"
	"github.com/kubewharf/kubeadmiral/pkg/controllers/util/managedlabel"
	"github.com/kubewharf/kubeadmiral/pkg/controllers/util/schema"
)

const (
	clusterSyncPeriod = 10 * time.Minute
)

// An object with an origin information.
type FederatedObject struct {
	Object      interface{}
	ClusterName string
}

// FederatedReadOnlyStore is an overlay over multiple stores created in federated clusters.
type FederatedReadOnlyStore interface {
	// Returns all items in the store.
	List() ([]FederatedObject, error)

	// Returns all items from a cluster.
	ListFromCluster(clusterName string) ([]interface{}, error)

	// GetKeyFor returns the key under which the item would be put in the store.
	GetKeyFor(item interface{}) string

	// GetByKey returns the item stored under the given key in the specified cluster (if exist).
	GetByKey(clusterName string, key string) (interface{}, bool, error)

	// Returns the items stored under the given key in all clusters.
	GetFromAllClusters(key string) ([]FederatedObject, error)

	// Checks whether stores for all clusters form the lists (and only these) are there and
	// are synced. This is only a basic check whether the data inside of the store is usable.
	// It is not a full synchronization/locking mechanism it only tries to ensure that out-of-sync
	// issues occur less often.	All users of the interface should assume
	// that there may be significant delays in content updates of all kinds and write their
	// code that it doesn't break if something is slightly out-of-sync.
	ClustersSynced(clusters []*fedcorev1a1.FederatedCluster) bool

	// Checks whether the store for the specified cluster is there and synced.
	ClusterSynced(clusterName string) bool
}

// An interface to retrieve both KubeFedCluster resources and clients
// to access the clusters they represent.
type RegisteredClustersView interface {
	// GetClientForCluster returns a client for the cluster, if present.
	GetClientForCluster(clusterName string) (generic.Client, error)

	// GetUnreadyClusters returns a list of all clusters that are not ready yet.
	GetUnreadyClusters() ([]*fedcorev1a1.FederatedCluster, error)

	// GetReadyClusters returns all clusters for which the sub-informers are run.
	GetReadyClusters() ([]*fedcorev1a1.FederatedCluster, error)

	// GetJoinedClusters returns a list of all joined clusters.
	GetJoinedClusters() ([]*fedcorev1a1.FederatedCluster, error)

	// GetReadyCluster returns the cluster with the given name, if found.
	GetReadyCluster(name string) (*fedcorev1a1.FederatedCluster, bool, error)

	// GetCluster returns the cluster with the given name, if found.
	GetCluster(name string) (*fedcorev1a1.FederatedCluster, bool, error)

	// ClustersSynced returns true if the view is synced (for the first time).
	ClustersSynced() bool
}

// FederatedInformer provides access to clusters registered with a
// KubeFed control plane and watches a given resource type in
// registered clusters.
//
// Whenever a new cluster is registered with KubeFed, an informer is
// created for it using TargetInformerFactory. Informers are stopped
// when a cluster is either put offline of deleted. It is assumed that
// some controller keeps an eye on the cluster list and thus the
// clusters in ETCD are up to date.
type FederatedInformer interface {
	RegisteredClustersView

	// Returns a store created over all stores from target informers.
	GetTargetStore() FederatedReadOnlyStore

	// Starts all the processes.
	Start()

	// Stops all the processes inside the informer.
	Stop()
}

// A function that should be used to create an informer on the target object. Store should use
// cache.DeletionHandlingMetaNamespaceKeyFunc as a keying function.
type TargetInformerFactory func(*fedcorev1a1.FederatedCluster, *restclient.Config) (cache.Store, cache.Controller, error)

// A structure with cluster lifecycle handler functions. Cluster is available (and ClusterAvailable is fired)
// when it is created in federated etcd and ready. Cluster becomes unavailable (and ClusterUnavailable is fired)
// when it is either deleted or becomes not ready. When cluster spec (IP)is modified both ClusterAvailable
// and ClusterUnavailable are fired.
type ClusterLifecycleHandlerFuncs struct {
	// Fired when the cluster becomes available.
	ClusterAvailable func(*fedcorev1a1.FederatedCluster)
	// Fired when the cluster becomes unavailable. The second arg contains data that was present
	// in the cluster before deletion.
	ClusterUnavailable func(*fedcorev1a1.FederatedCluster, []interface{})
}

// Builds a FederatedInformer for the given configuration.
func NewFederatedInformer(
	config *ControllerConfig,
	fedClient generic.Client,
	restConfig *restclient.Config,
	apiResource *metav1.APIResource,
	triggerFunc func(pkgruntime.Object),
	clusterLifecycle *ClusterLifecycleHandlerFuncs,
) (FederatedInformer, error) {
	targetInformerFactory := func(
		cluster *fedcorev1a1.FederatedCluster,
		clusterConfig *restclient.Config,
	) (cache.Store, cache.Controller, error) {
		resourceClient, err := NewResourceClient(clusterConfig, apiResource)
		if err != nil {
			return nil, nil, err
		}
		targetNamespace := NamespaceForCluster(cluster.Name, config.TargetNamespace)
		extraTags := map[string]string{"member_cluster": cluster.Name}
		store, controller := NewManagedResourceInformer(
			resourceClient,
			targetNamespace,
			triggerFunc,
			extraTags,
			config.Metrics,
		)
		return store, controller, nil
	}

	federatedInformer := &federatedInformerImpl{
		targetInformerFactory: targetInformerFactory,
		configFactory: func(cluster *fedcorev1a1.FederatedCluster) (*restclient.Config, error) {
			clusterConfig, err := BuildClusterConfigWithGenericClient(
				cluster,
				fedClient,
				restConfig,
				config.FedSystemNamespace,
			)
			if err != nil {
				return nil, err
			}
			if clusterConfig == nil {
				return nil, errors.Errorf("Unable to load configuration for cluster %q", cluster.Name)
			}
			restclient.AddUserAgent(clusterConfig, restConfig.UserAgent)
			return clusterConfig, nil
		},
		targetInformers: make(map[string]informer),
		clusterClients:  make(map[string]generic.Client),
	}

	getClusterData := func(name string) []interface{} {
		data, err := federatedInformer.GetTargetStore().ListFromCluster(name)
		if err != nil {
			klog.Errorf("Failed to list %s content: %v", name, err)
			return make([]interface{}, 0)
		}
		return data
	}

	var err error
	federatedInformer.clusterInformer.store, federatedInformer.clusterInformer.controller, err = NewGenericInformerWithEventHandler(
		config.KubeConfig,
		"",
		&fedcorev1a1.FederatedCluster{},
		clusterSyncPeriod,
		&cache.ResourceEventHandlerFuncs{
			DeleteFunc: func(old interface{}) {
				oldCluster, ok := old.(*fedcorev1a1.FederatedCluster)
				if ok {
					var data []interface{}
					if clusterLifecycle.ClusterUnavailable != nil {
						data = getClusterData(oldCluster.Name)
					}
					federatedInformer.deleteCluster(oldCluster)
					if clusterLifecycle.ClusterUnavailable != nil {
						clusterLifecycle.ClusterUnavailable(oldCluster, data)
					}
				}
			},
			AddFunc: func(cur interface{}) {
				curCluster, ok := cur.(*fedcorev1a1.FederatedCluster)
				if !ok {
					klog.Errorf("Cluster %v not added; incorrect type", curCluster.Name)
				} else if IsClusterReady(&curCluster.Status) {
					federatedInformer.addCluster(curCluster)
					klog.Infof("Cluster %v is ready", curCluster.Name)
					if clusterLifecycle.ClusterAvailable != nil {
						clusterLifecycle.ClusterAvailable(curCluster)
					}
				} else {
					klog.Infof("Cluster %v not added; it is not ready.", curCluster.Name)
				}
			},
			UpdateFunc: func(old, cur interface{}) {
				oldCluster, ok := old.(*fedcorev1a1.FederatedCluster)
				if !ok {
					klog.Errorf("Internal error: Cluster %v not updated.  Old cluster not of correct type.", old)
					return
				}
				curCluster, ok := cur.(*fedcorev1a1.FederatedCluster)
				if !ok {
					klog.Errorf("Internal error: Cluster %v not updated.  New cluster not of correct type.", cur)
					return
				}
				if oldCluster.DeletionTimestamp == nil && curCluster.DeletionTimestamp != nil {
					// TODO: review the semantics of marked for deletion - we need to have event handlers
					// for when a cluster is marked for deletion to perform cleanup (clusterUnavailable might not
					// be the mostappropriate),because of this we should also not delete the cluster from the informer
					if clusterLifecycle.ClusterUnavailable != nil {
						data := getClusterData(oldCluster.Name)
						clusterLifecycle.ClusterUnavailable(oldCluster, data)
					}
				} else if IsClusterReady(&oldCluster.Status) != IsClusterReady(&curCluster.Status) ||
					!reflect.DeepEqual(oldCluster.Spec, curCluster.Spec) ||
					!reflect.DeepEqual(oldCluster.ObjectMeta.Labels, curCluster.ObjectMeta.Labels) ||
					!reflect.DeepEqual(oldCluster.ObjectMeta.Annotations, curCluster.ObjectMeta.Annotations) {
					var data []interface{}
					if clusterLifecycle.ClusterUnavailable != nil {
						data = getClusterData(oldCluster.Name)
					}
					federatedInformer.deleteCluster(oldCluster)
					if clusterLifecycle.ClusterUnavailable != nil {
						clusterLifecycle.ClusterUnavailable(oldCluster, data)
					}

					if IsClusterReady(&curCluster.Status) {
						federatedInformer.addCluster(curCluster)
						if clusterLifecycle.ClusterAvailable != nil {
							clusterLifecycle.ClusterAvailable(curCluster)
						}
					}
				} else {
					// klog.V(7).Infof("Cluster %v not updated to %v as ready status and specs are identical", oldCluster, curCluster)
				}
			},
		},
		config.Metrics,
	)
	return federatedInformer, err
}

func IsClusterReady(clusterStatus *fedcorev1a1.FederatedClusterStatus) bool {
	for _, condition := range clusterStatus.Conditions {
		if condition.Type == fedcorev1a1.ClusterReady {
			if condition.Status == corev1.ConditionTrue {
				return true
			}
		}
	}
	return false
}

func IsClusterJoined(clusterStatus *fedcorev1a1.FederatedClusterStatus) bool {
	for _, condition := range clusterStatus.Conditions {
		if condition.Type == fedcorev1a1.ClusterJoined {
			if condition.Status == corev1.ConditionTrue {
				return true
			}
		}
	}
	return false
}

type informer struct {
	controller cache.Controller
	store      cache.Store
	stopChan   chan struct{}
}

type federatedInformerImpl struct {
	sync.Mutex

	// Informer on federated clusters.
	clusterInformer informer

	// Target informers factory
	targetInformerFactory TargetInformerFactory

	// Structures returned by targetInformerFactory
	targetInformers map[string]informer

	// Retrieves configuration to access a cluster.
	configFactory func(*fedcorev1a1.FederatedCluster) (*restclient.Config, error)

	// Caches cluster clients (reduces client discovery and secret retrieval)
	clusterClients map[string]generic.Client
}

// *federatedInformerImpl implements FederatedInformer interface.
var _ FederatedInformer = &federatedInformerImpl{}

type federatedStoreImpl struct {
	federatedInformer *federatedInformerImpl
}

func (f *federatedInformerImpl) Stop() {
	klog.V(4).Infof("Stopping federated informer.")
	f.Lock()
	defer f.Unlock()

	klog.V(4).Infof("... Closing cluster informer channel.")
	close(f.clusterInformer.stopChan)
	for key, informer := range f.targetInformers {
		klog.V(4).Infof("... Closing informer channel for %q.", key)
		close(informer.stopChan)
		// Remove each informer after it has been stopped to prevent
		// subsequent cluster deletion from attempting to double close
		// an informer's stop channel.
		delete(f.targetInformers, key)
	}
}

func (f *federatedInformerImpl) Start() {
	f.Lock()
	defer f.Unlock()

	f.clusterInformer.stopChan = make(chan struct{})
	go f.clusterInformer.controller.Run(f.clusterInformer.stopChan)
}

// GetClientForCluster returns a client for the cluster, if present.
func (f *federatedInformerImpl) GetClientForCluster(clusterName string) (generic.Client, error) {
	f.Lock()
	defer f.Unlock()

	// return cached client if one exists (to prevent frequent secret retrieval and rest discovery)
	if client, ok := f.clusterClients[clusterName]; ok {
		return client, nil
	}
	config, err := f.getConfigForClusterUnlocked(clusterName)
	if err != nil {
		return nil, errors.Wrap(err, "Client creation failed")
	}
	client, err := generic.New(config)
	if err != nil {
		return client, err
	}
	f.clusterClients[clusterName] = client
	return client, nil
}

func (f *federatedInformerImpl) getConfigForClusterUnlocked(clusterName string) (*restclient.Config, error) {
	// No locking needed. Will happen in f.GetCluster.
	klog.V(4).Infof("Getting config for cluster %q", clusterName)
	if cluster, found, err := f.getReadyClusterUnlocked(clusterName); found && err == nil {
		return f.configFactory(cluster)
	} else if err != nil {
		return nil, err
	}
	return nil, errors.Errorf("cluster %q not found", clusterName)
}

func (f *federatedInformerImpl) GetUnreadyClusters() ([]*fedcorev1a1.FederatedCluster, error) {
	f.Lock()
	defer f.Unlock()

	items := f.clusterInformer.store.List()
	result := make([]*fedcorev1a1.FederatedCluster, 0, len(items))
	for _, item := range items {
		if cluster, ok := item.(*fedcorev1a1.FederatedCluster); ok {
			if !IsClusterReady(&cluster.Status) {
				result = append(result, cluster)
			}
		} else {
			return nil, errors.Errorf("wrong data in FederatedInformerImpl cluster store: %v", item)
		}
	}
	return result, nil
}

// GetReadyClusters returns all clusters for which the sub-informers are run.
func (f *federatedInformerImpl) GetReadyClusters() ([]*fedcorev1a1.FederatedCluster, error) {
	return f.getJoinedClusters(true)
}

// GetJoinedClusters returns all joined clusters regardless of ready state.
func (f *federatedInformerImpl) GetJoinedClusters() ([]*fedcorev1a1.FederatedCluster, error) {
	return f.getJoinedClusters(false)
}

// getJoinedClusters returns only ready clusters if onlyReady is true and all joined clusters otherwise.
func (f *federatedInformerImpl) getJoinedClusters(onlyReady bool) ([]*fedcorev1a1.FederatedCluster, error) {
	f.Lock()
	defer f.Unlock()

	items := f.clusterInformer.store.List()
	result := make([]*fedcorev1a1.FederatedCluster, 0, len(items))
	for _, item := range items {
		if cluster, ok := item.(*fedcorev1a1.FederatedCluster); ok {
			if IsClusterJoined(&cluster.Status) && (!onlyReady || IsClusterReady(&cluster.Status)) {
				result = append(result, cluster)
			}
		} else {
			return nil, errors.Errorf("wrong data in FederatedInformerImpl cluster store: %v", item)
		}
	}
	return result, nil
}

func (f *federatedInformerImpl) GetCluster(name string) (*fedcorev1a1.FederatedCluster, bool, error) {
	f.Lock()
	defer f.Unlock()
	return f.getClusterUnlocked(name)
}

func (f *federatedInformerImpl) getClusterUnlocked(key string) (*fedcorev1a1.FederatedCluster, bool, error) {
	if obj, exist, err := f.clusterInformer.store.GetByKey(key); exist && err == nil {
		if cluster, ok := obj.(*fedcorev1a1.FederatedCluster); ok {
			return cluster, true, nil
		}
		return nil, false, errors.Errorf("wrong data in FederatedInformerImpl cluster store: %v", obj)
	} else {
		return nil, false, err
	}
}

// GetCluster returns the cluster with the given name, if found.
func (f *federatedInformerImpl) GetReadyCluster(name string) (*fedcorev1a1.FederatedCluster, bool, error) {
	f.Lock()
	defer f.Unlock()
	return f.getReadyClusterUnlocked(name)
}

func (f *federatedInformerImpl) getReadyClusterUnlocked(key string) (*fedcorev1a1.FederatedCluster, bool, error) {
	if cluster, exist, err := f.getClusterUnlocked(key); exist && err == nil {
		if IsClusterReady(&cluster.Status) {
			return cluster, true, nil
		}
		return nil, false, nil
	} else {
		return nil, false, err
	}
}

// Synced returns true if the view is synced (for the first time)
func (f *federatedInformerImpl) ClustersSynced() bool {
	return f.clusterInformer.controller.HasSynced()
}

// Adds the given cluster to federated informer.
func (f *federatedInformerImpl) addCluster(cluster *fedcorev1a1.FederatedCluster) {
	f.Lock()
	defer f.Unlock()
	name := cluster.Name
	if config, err := f.getConfigForClusterUnlocked(name); err == nil {
		store, controller, err := f.targetInformerFactory(cluster, config)
		if err != nil {
			// TODO: create also an event for cluster.
			klog.Errorf("Failed to create an informer for cluster %q: %v", cluster.Name, err)
			return
		}
		targetInformer := informer{
			controller: controller,
			store:      store,
			stopChan:   make(chan struct{}),
		}
		f.targetInformers[name] = targetInformer
		go targetInformer.controller.Run(targetInformer.stopChan)
	} else {
		// TODO: create also an event for cluster.
		klog.Errorf("Failed to create a client for cluster: %v", err)
	}
}

// Removes the cluster from federated informer.
func (f *federatedInformerImpl) deleteCluster(cluster *fedcorev1a1.FederatedCluster) {
	f.Lock()
	defer f.Unlock()
	name := cluster.Name
	if targetInformer, found := f.targetInformers[name]; found {
		close(targetInformer.stopChan)
	}
	delete(f.targetInformers, name)
	delete(f.clusterClients, name)
}

// Returns a store created over all stores from target informers.
func (f *federatedInformerImpl) GetTargetStore() FederatedReadOnlyStore {
	return &federatedStoreImpl{
		federatedInformer: f,
	}
}

// Returns all items in the store.
func (fs *federatedStoreImpl) List() ([]FederatedObject, error) {
	fs.federatedInformer.Lock()
	defer fs.federatedInformer.Unlock()

	result := make([]FederatedObject, 0)
	for clusterName, targetInformer := range fs.federatedInformer.targetInformers {
		for _, value := range targetInformer.store.List() {
			result = append(result, FederatedObject{ClusterName: clusterName, Object: value})
		}
	}
	return result, nil
}

// Returns all items in the given cluster.
func (fs *federatedStoreImpl) ListFromCluster(clusterName string) ([]interface{}, error) {
	fs.federatedInformer.Lock()
	defer fs.federatedInformer.Unlock()

	result := make([]interface{}, 0)
	if targetInformer, found := fs.federatedInformer.targetInformers[clusterName]; found {
		values := targetInformer.store.List()
		result = append(result, values...)
	}
	return result, nil
}

// GetByKey returns the item stored under the given key in the specified cluster (if exist).
func (fs *federatedStoreImpl) GetByKey(clusterName string, key string) (interface{}, bool, error) {
	fs.federatedInformer.Lock()
	defer fs.federatedInformer.Unlock()
	if targetInformer, found := fs.federatedInformer.targetInformers[clusterName]; found {
		return targetInformer.store.GetByKey(key)
	}
	return nil, false, fmt.Errorf("cluster %s not found", clusterName)
}

// Returns the items stored under the given key in all clusters.
func (fs *federatedStoreImpl) GetFromAllClusters(key string) ([]FederatedObject, error) {
	fs.federatedInformer.Lock()
	defer fs.federatedInformer.Unlock()

	result := make([]FederatedObject, 0)
	for clusterName, targetInformer := range fs.federatedInformer.targetInformers {
		value, exist, err := targetInformer.store.GetByKey(key)
		if err != nil {
			return nil, err
		}
		if exist {
			result = append(result, FederatedObject{ClusterName: clusterName, Object: value})
		}
	}
	return result, nil
}

// GetKeyFor returns the key under which the item would be put in the store.
func (fs *federatedStoreImpl) GetKeyFor(item interface{}) string {
	// TODO: support other keying functions.
	key, _ := cache.DeletionHandlingMetaNamespaceKeyFunc(item)
	return key
}

// Checks whether stores for all clusters form the lists (and only these) are there and
// are synced.
func (fs *federatedStoreImpl) ClustersSynced(clusters []*fedcorev1a1.FederatedCluster) bool {
	// Get the list of informers to check under a lock and check it outside.
	okSoFar, informersToCheck := func() (bool, []informer) {
		fs.federatedInformer.Lock()
		defer fs.federatedInformer.Unlock()

		if len(fs.federatedInformer.targetInformers) != len(clusters) {
			return false, []informer{}
		}
		informersToCheck := make([]informer, 0, len(clusters))
		for _, cluster := range clusters {
			if targetInformer, found := fs.federatedInformer.targetInformers[cluster.Name]; found {
				informersToCheck = append(informersToCheck, targetInformer)
			} else {
				return false, []informer{}
			}
		}
		return true, informersToCheck
	}()

	if !okSoFar {
		return false
	}
	for _, informerToCheck := range informersToCheck {
		if !informerToCheck.controller.HasSynced() {
			return false
		}
	}
	return true
}

func (fs *federatedStoreImpl) ClusterSynced(clusterName string) bool {
	fs.federatedInformer.Lock()
	defer fs.federatedInformer.Unlock()

	if targetInformer, found := fs.federatedInformer.targetInformers[clusterName]; found {
		return targetInformer.controller.HasSynced()
	}

	return false
}

// GetClusterObject is a helper function to get a cluster object. GetClusterObject first attempts to get the object from
// the federated informer with the given key. However, if the cache for the cluster is not synced, it will send a GET
// request to the cluster's apiserver to retrieve the object directly.
func GetClusterObject(
	ctx context.Context,
	informer FederatedInformer,
	clusterName string,
	qualifedName common.QualifiedName,
	apiResource metav1.APIResource,
) (*unstructured.Unstructured, bool, error) {
	if informer.GetTargetStore().ClusterSynced(clusterName) {
		clusterObj, exists, err := informer.GetTargetStore().GetByKey(clusterName, qualifedName.String())
		if err != nil || !exists {
			return nil, exists, err
		}

		return clusterObj.(*unstructured.Unstructured), exists, err
	}

	client, err := informer.GetClientForCluster(clusterName)
	if err != nil {
		return nil, false, fmt.Errorf("failed to get client for cluster %s: %w", clusterName, err)
	}

	clusterObj := &unstructured.Unstructured{}
	gvk := schema.APIResourceToGVK(&apiResource)
	clusterObj.SetKind(gvk.Kind)
	clusterObj.SetAPIVersion(gvk.GroupVersion().String())

	err = client.Get(ctx, clusterObj, qualifedName.Namespace, qualifedName.Name)
	// the NotFound error includes the resource does not exist and the api path does not exist
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("failed to get object %s with client: %w", qualifedName.String(), err)
	}
	if !managedlabel.HasManagedLabel(clusterObj) {
		return nil, false, nil
	}

	return clusterObj, true, nil
}
