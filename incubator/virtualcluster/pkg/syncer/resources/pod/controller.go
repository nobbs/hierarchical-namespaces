/*
Copyright 2019 The Kubernetes Authors.

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

package pod

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreinformers "k8s.io/client-go/informers/core/v1"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	"github.com/kubernetes-sigs/multi-tenancy/incubator/virtualcluster/pkg/syncer/apis/config"
	"github.com/kubernetes-sigs/multi-tenancy/incubator/virtualcluster/pkg/syncer/constants"
	"github.com/kubernetes-sigs/multi-tenancy/incubator/virtualcluster/pkg/syncer/conversion"
	"github.com/kubernetes-sigs/multi-tenancy/incubator/virtualcluster/pkg/syncer/manager"
	mc "github.com/kubernetes-sigs/multi-tenancy/incubator/virtualcluster/pkg/syncer/mccontroller"
	"github.com/kubernetes-sigs/multi-tenancy/incubator/virtualcluster/pkg/syncer/reconciler"
)

type controller struct {
	// syncer configuration
	config *config.SyncerConfiguration
	// super master pod client
	client v1core.CoreV1Interface
	// super master informer/listers/synced functions
	informer      coreinformers.Interface
	podLister     listersv1.PodLister
	podSynced     cache.InformerSynced
	serviceLister listersv1.ServiceLister
	serviceSynced cache.InformerSynced
	secretLister  listersv1.SecretLister
	secretSynced  cache.InformerSynced
	// Connect to all tenant master pod informers
	multiClusterPodController *mc.MultiClusterController
	// UWS queue
	workers int
	queue   workqueue.RateLimitingInterface
	// Checker timer
	periodCheckerPeriod time.Duration
	// Cluster vNode PodMap and GCMap, needed for vNode garbage collection
	sync.Mutex
	clusterVNodePodMap map[string]map[string]map[string]struct{}
	clusterVNodeGCMap  map[string]map[string]VNodeGCStatus
}

type VirtulNodeDeletionPhase string

const (
	VNodeQuiescing VirtulNodeDeletionPhase = "Quiescing"
	VNodeDeleting  VirtulNodeDeletionPhase = "Deleting"
)

type VNodeGCStatus struct {
	QuiesceStartTime metav1.Time
	Phase            VirtulNodeDeletionPhase
}

func Register(
	config *config.SyncerConfiguration,
	client v1core.CoreV1Interface,
	informer coreinformers.Interface,
	controllerManager *manager.ControllerManager,
) {
	c, _, err := NewPodController(config, client, informer, nil)
	if err != nil {
		klog.Errorf("failed to create multi cluster Pod controller %v", err)
		return
	}
	controllerManager.AddResourceSyncer(c)
}

func NewPodController(config *config.SyncerConfiguration, client v1core.CoreV1Interface, informer coreinformers.Interface, options *mc.Options) (manager.ResourceSyncer, *mc.MultiClusterController, error) {
	c := &controller{
		config:              config,
		client:              client,
		informer:            informer,
		queue:               workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "super_master_pod"),
		workers:             constants.UwsControllerWorkerHigh,
		periodCheckerPeriod: 60 * time.Second,
		clusterVNodePodMap:  make(map[string]map[string]map[string]struct{}),
		clusterVNodeGCMap:   make(map[string]map[string]VNodeGCStatus),
	}

	if options == nil {
		options = &mc.Options{Reconciler: c}
	}
	options.MaxConcurrentReconciles = constants.DwsControllerWorkerHigh
	multiClusterPodController, err := mc.NewMCController("tenant-masters-pod-controller", &v1.Pod{}, *options)
	if err != nil {
		klog.Errorf("failed to create multi cluster pod controller %v", err)
		return nil, nil, err
	}
	c.multiClusterPodController = multiClusterPodController

	c.serviceLister = informer.Services().Lister()
	c.secretLister = informer.Secrets().Lister()
	c.podLister = informer.Pods().Lister()
	if options.IsFake {
		c.serviceSynced = func() bool { return true }
		c.secretSynced = func() bool { return true }
		c.podSynced = func() bool { return true }
	} else {
		c.serviceSynced = informer.Services().Informer().HasSynced
		c.secretSynced = informer.Secrets().Informer().HasSynced
		c.podSynced = informer.Pods().Informer().HasSynced
	}
	informer.Pods().Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *v1.Pod:
					return assignedPod(t)
				case cache.DeletedFinalStateUnknown:
					if pod, ok := t.Obj.(*v1.Pod); ok {
						return assignedPod(pod)
					}
					utilruntime.HandleError(fmt.Errorf("unable to convert object %T to *v1.Pod in %T", obj, c))
					return false
				default:
					utilruntime.HandleError(fmt.Errorf("unable to handle object in %T: %T", c, obj))
					return false
				}
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc: c.enqueuePod,
				UpdateFunc: func(oldObj, newObj interface{}) {
					newPod := newObj.(*v1.Pod)
					oldPod := oldObj.(*v1.Pod)
					if newPod.ResourceVersion == oldPod.ResourceVersion {
						// Periodic resync will send update events for all known Deployments.
						// Two different versions of the same Deployment will always have different RVs.
						return
					}

					c.enqueuePod(newObj)
				},
				DeleteFunc: c.enqueuePod,
			},
		},
	)
	return c, multiClusterPodController, nil
}

func (c *controller) enqueuePod(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return
	}

	clusterName, _ := conversion.GetVirtualOwner(pod)
	if clusterName == "" {
		return
	}

	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %v: %v", obj, err))
		return
	}

	c.queue.Add(reconciler.UwsRequest{Key: key})
}

// c.Mutex needs to be Locked before calling addToClusterVNodeGCMap
func (c *controller) addToClusterVNodeGCMap(cluster string, nodeName string) {
	if _, exist := c.clusterVNodeGCMap[cluster]; !exist {
		c.clusterVNodeGCMap[cluster] = make(map[string]VNodeGCStatus)
	}
	c.clusterVNodeGCMap[cluster][nodeName] = VNodeGCStatus{
		QuiesceStartTime: metav1.Now(),
		Phase:            VNodeQuiescing,
	}
}

// c.Mutex needs to be Locked before calling removeQuiescingNodeFromClusterVNodeGCMap
func (c *controller) removeQuiescingNodeFromClusterVNodeGCMap(cluster string, nodeName string) bool {
	if _, exist := c.clusterVNodeGCMap[cluster]; exist {
		if _, exist := c.clusterVNodeGCMap[cluster][nodeName]; exist {
			if c.clusterVNodeGCMap[cluster][nodeName].Phase == VNodeQuiescing {
				delete(c.clusterVNodeGCMap[cluster], nodeName)
				return true
			} else {
				return false
			}
		}
	}
	return true
}

func (c *controller) updateClusterVNodePodMap(clusterName, nodeName, requestUID string, event reconciler.EventType) {
	c.Lock()
	defer c.Unlock()
	if event == reconciler.UpdateEvent {
		if _, exist := c.clusterVNodePodMap[clusterName]; !exist {
			c.clusterVNodePodMap[clusterName] = make(map[string]map[string]struct{})
		}
		if _, exist := c.clusterVNodePodMap[clusterName][nodeName]; !exist {
			c.clusterVNodePodMap[clusterName][nodeName] = make(map[string]struct{})
		}
		c.clusterVNodePodMap[clusterName][nodeName][requestUID] = struct{}{}
		if !c.removeQuiescingNodeFromClusterVNodeGCMap(clusterName, nodeName) {
			// We have consistency issue here. TODO: add to metrics
			klog.Errorf("Cluster %s has vPods in vNode %s which is being GCed!", clusterName, nodeName)
		}
	} else { // delete
		if _, exist := c.clusterVNodePodMap[clusterName][nodeName]; exist {
			if _, exist := c.clusterVNodePodMap[clusterName][nodeName][requestUID]; exist {
				delete(c.clusterVNodePodMap[clusterName][nodeName], requestUID)
			} else {
				klog.Warningf("Deleted pod %s of cluster (%s) is not found in clusterVNodePodMap", requestUID, clusterName)
			}

			// If vNode does not have any Pod left, put it into gc map
			if len(c.clusterVNodePodMap[clusterName][nodeName]) == 0 {
				c.addToClusterVNodeGCMap(clusterName, nodeName)
				delete(c.clusterVNodePodMap[clusterName], nodeName)
			}
		} else {
			klog.Warningf("The nodename %s of deleted pod %s in cluster (%s) is not found in clusterVNodePodMap", nodeName, requestUID, clusterName)
		}
	}
}

func (c *controller) AddCluster(cluster mc.ClusterInterface) {
	klog.Infof("tenant-masters-pod-controller watch cluster %s for pod resource", cluster.GetClusterName())
	err := c.multiClusterPodController.WatchClusterResource(cluster, mc.WatchOptions{})
	if err != nil {
		klog.Errorf("failed to watch cluster %s pod event: %v", cluster.GetClusterName(), err)
	}
}

func (c *controller) RemoveCluster(cluster mc.ClusterInterface) {
	klog.Infof("tenant-masters-pod-controller stop watching cluster %s for pod resource", cluster.GetClusterName())
	c.multiClusterPodController.TeardownClusterResource(cluster)
}

// assignedPod selects pods that are assigned (scheduled and running).
func assignedPod(pod *v1.Pod) bool {
	return len(pod.Spec.NodeName) != 0
}
