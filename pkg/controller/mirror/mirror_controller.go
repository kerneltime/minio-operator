/*
 * Copyright (C) 2019, MinIO, Inc.
 *
 * This code is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License, version 3,
 * as published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License, version 3,
 * along with this program.  If not, see <http://www.gnu.org/licenses/>
 *
 */

package mirror

import (
	"fmt"
	"time"

	"github.com/golang/glog"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	queue "k8s.io/client-go/util/workqueue"

	clientset "github.com/minio/minio-operator/pkg/client/clientset/versioned"
	minioscheme "github.com/minio/minio-operator/pkg/client/clientset/versioned/scheme"
	informers "github.com/minio/minio-operator/pkg/client/informers/externalversions/miniooperator.min.io/v1beta1"
	listers "github.com/minio/minio-operator/pkg/client/listers/miniooperator.min.io/v1beta1"
)

const controllerAgentName = "minio-operator"

const (
	// SuccessSynced is used as part of the Event 'reason' when a MirrorInstace is synced
	SuccessSynced = "Synced"
	// ErrResourceExists is used as part of the Event 'reason' when a MirrorInstace fails
	// to sync due to a StatefulSet of the same name already existing.
	ErrResourceExists = "ErrResourceExists"
	// MessageResourceExists is the message used for Events when a MirrorInstace
	// fails to sync due to a StatefulSet already existing
	MessageResourceExists = "Resource %q already exists and is not managed by MinIO"
	// MessageResourceSynced is the message used for an Event fired when a MirrorInstace
	// is synced successfully
	MessageResourceSynced = "MirrorInstace synced successfully"
)

// Controller struct watches the Kubernetes API for changes to MirrorInstace resources
type Controller struct {
	// kubeClientSet is a standard kubernetes clientset
	kubeClientSet kubernetes.Interface
	// minioClientSet is a clientset for our own API group
	minioClientSet clientset.Interface

	// delpoymentLister is able to list/get Deployments from a shared
	// informer's store.
	delpoymentLister appslisters.DeploymentLister
	// deploymentListerSynced returns true if the Deployment shared informer
	// has synced at least once.
	deploymentListerSynced cache.InformerSynced

	// mirrorLister lists MirrorInstance from a shared informer's
	// store.
	mirrorLister listers.MirrorInstanceLister
	// mirrorListerSynced returns true if the StatefulSet shared informer
	// has synced at least once.
	mirrorListerSynced cache.InformerSynced

	// queue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue queue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
}

// NewController returns a new sample controller
func NewController(
	kubeClientSet kubernetes.Interface,
	minioClientSet clientset.Interface,
	deploymentInformer appsinformers.DeploymentInformer,
	mirrorInformer informers.MirrorInstanceInformer,
) *Controller {

	// Create event broadcaster
	// Add minio-controller types to the default Kubernetes Scheme so Events can be
	// logged for minio-controller types.
	minioscheme.AddToScheme(scheme.Scheme)
	glog.V(4).Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClientSet.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		kubeClientSet:          kubeClientSet,
		minioClientSet:         minioClientSet,
		delpoymentLister:       deploymentInformer.Lister(),
		deploymentListerSynced: deploymentInformer.Informer().HasSynced,
		mirrorLister:           mirrorInformer.Lister(),
		mirrorListerSynced:     mirrorInformer.Informer().HasSynced,
		workqueue:              queue.NewNamedRateLimitingQueue(queue.DefaultControllerRateLimiter(), "MinIOInstances"),
		recorder:               recorder,
	}

	glog.Info("Setting up event handlers")
	// Set up an event handler for when MirrorInstace resources change
	mirrorInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueMirrorInstance,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueMirrorInstance(new)
		},
	})
	// Set up an event handler for when Deployment resources change. This
	// handler will lookup the owner of the given Deployment, and if it is
	// owned by a MirrorInstance resource will enqueue that MirrorInstance resource for
	// processing. This way, we don't need to implement custom logic for
	// handling Deployment resources. More info on this pattern:
	// https://github.com/kubernetes/community/blob/8cafef897a22026d42f5e5bb3f104febe7e29830/contributors/devel/controllers.md
	deploymentInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleObject,
		UpdateFunc: func(old, new interface{}) {
			newDepl := new.(*appsv1.StatefulSet)
			oldDepl := old.(*appsv1.StatefulSet)
			if newDepl.ResourceVersion == oldDepl.ResourceVersion {
				// Periodic resync will send update events for all known StatefulSet.
				// Two different versions of the same StatefulSet will always have different RVs.
				return
			}
			controller.handleObject(new)
		},
		DeleteFunc: controller.handleObject,
	})
	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it w	ill shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	glog.Info("Starting MirrorInstace controller")

	// Wait for the caches to be synced before starting workers
	glog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.deploymentListerSynced, c.mirrorListerSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	glog.Info("Starting workers")
	// Launch two workers to process MirrorInstace resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	glog.Info("Started workers")
	<-stopCh
	glog.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}
	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// MirrorInstace resource to be synced.
		if err := c.syncHandler(key); err != nil {
			return fmt.Errorf("error syncing '%s': %s", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		glog.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}
	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the MirrorInstace resource
// with the current status of the resource.
func (c *Controller) syncHandler(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("Invalid resource key: %s", key))
		return nil
	}

	// nsName := types.NamespacedName{Namespace: namespace, Name: name}

	// Get the MirrorInstace resource with this namespace/name
	mi, err := c.mirrorLister.MirrorInstances(namespace).Get(name)
	if err != nil {
		// The MirrorInstace resource may no longer exist, in which case we stop processing.
		if errors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("MirrorInstace '%s' in work queue no longer exists", key))
			return nil
		}
		return err
	}

	// If an error occurs during Get/Create, we'll requeue the item so we can
	// attempt processing again later. This could have been caused by a
	// temporary network failure, or any other transient reason.
	if err != nil {
		return err
	}

	// Get the Deployment with the name specified in MirrorInstace.spec
	//d, err := c.delpoymentLister.Deployments(mi.Namespace).Get(mi.Name)
	// If the resource doesn't exist, we'll create it
	if errors.IsNotFound(err) {
		//d = deployments.NewForCluster(mi)
		//_, err = c.kubeClientSet.AppsV1().Deployments(mi.Namespace).Create(d)
	}

	// If an error occurs during Get/Create, we'll requeue the item so we can
	// attempt processing again later. This could have been caused by a
	// temporary network failure, or any other transient reason.
	if err != nil {
		return err
	}

	// If an error occurs during Update, we'll requeue the item so we can
	// attempt processing again later. This could have been caused by a
	// temporary network failure, or any other transient reason.
	if err != nil {
		return err
	}

	c.recorder.Event(mi, corev1.EventTypeNormal, SuccessSynced, MessageResourceSynced)
	return nil
}

// enqueueMinIOInstance takes a MirrorInstace resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than MirrorInstace.
func (c *Controller) enqueueMirrorInstance(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.AddRateLimited(key)
}

// handleObject will take any resource implementing metav1.Object and attempt
// to find the MirrorInstace resource that 'owns' it. It does this by looking at the
// objects metadata.ownerReferences field for an appropriate OwnerReference.
// It then enqueues that MirrorInstace resource to be processed. If the object does not
// have an appropriate OwnerReference, it will simply be skipped.
func (c *Controller) handleObject(obj interface{}) {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
		glog.V(4).Infof("Recovered deleted object '%s' from tombstone", object.GetName())
	}
	glog.V(4).Infof("Processing object: %s", object.GetName())
	if ownerRef := metav1.GetControllerOf(object); ownerRef != nil {
		// If this object is not owned by a MirrorInstace, we should not do anything more
		// with it.
		if ownerRef.Kind != "MirrorInstance" {
			return
		}

		mirrorInstance, err := c.mirrorLister.MirrorInstances(object.GetNamespace()).Get(ownerRef.Name)
		if err != nil {
			glog.V(4).Infof("ignoring orphaned object '%s' of minioInstance '%s'", object.GetSelfLink(), ownerRef.Name)
			return
		}

		c.enqueueMirrorInstance(mirrorInstance)
		return
	}
}
