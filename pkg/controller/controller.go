/*
Copyright 2019 eterna2

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"os/exec"
	"reflect"
	"time"

	"github.com/golang/glog"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"github.com/e2fyi/k8s-helm-operator/pkg/apis/helmoperator.e2.fyi/v1alpha1"
	crdclientset "github.com/e2fyi/k8s-helm-operator/pkg/client/clientset/versioned"
	crdscheme "github.com/e2fyi/k8s-helm-operator/pkg/client/clientset/versioned/scheme"
	crdinformers "github.com/e2fyi/k8s-helm-operator/pkg/client/informers/externalversions"
	crdlisters "github.com/e2fyi/k8s-helm-operator/pkg/client/listers/helmoperator.e2.fyi/v1alpha1"
	"github.com/e2fyi/k8s-helm-operator/pkg/util"
)

const (
	sparkExecutorIDLabel      = "spark-exec-id"
	podAlreadyExistsErrorCode = "code=409"
	queueTokenRefillRate      = 50
	queueTokenBucketSize      = 500
	maximumUpdateRetries      = 3
)

var (
	keyFunc     = cache.DeletionHandlingMetaNamespaceKeyFunc
	execCommand = exec.Command
)

// Controller manages instances of HelmOperation.
type Controller struct {
	crdClient         crdclientset.Interface
	kubeClient        clientset.Interface
	queue             workqueue.RateLimitingInterface
	cacheSynced       cache.InformerSynced
	recorder          record.EventRecorder
	applicationLister crdlisters.HelmOperationLister
	podLister         v1.PodLister
}

// NewController creates a new Controller.
func NewController(
	crdClient crdclientset.Interface,
	kubeClient clientset.Interface,
	crdInformerFactory crdinformers.SharedInformerFactory,
	podInformerFactory informers.SharedInformerFactory,
	namespace string,
	ingressURLFormat string) *Controller {
	crdscheme.AddToScheme(scheme.Scheme)

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.V(2).Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: kubeClient.CoreV1().Events(namespace),
	})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, apiv1.EventSource{Component: "helm-operator"})

	return newHelmOperationController(crdClient, kubeClient, crdInformerFactory, podInformerFactory, recorder)
}

func newHelmOperationController(
	crdClient crdclientset.Interface,
	kubeClient clientset.Interface,
	crdInformerFactory crdinformers.SharedInformerFactory,
	podInformerFactory informers.SharedInformerFactory,
	eventRecorder record.EventRecorder) *Controller {
	queue := workqueue.NewNamedRateLimitingQueue(&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(queueTokenRefillRate), queueTokenBucketSize)},
		"spark-application-controller")

	controller := &Controller{
		crdClient:        crdClient,
		kubeClient:       kubeClient,
		recorder:         eventRecorder,
		queue:            queue
	}

	crdInformer := crdInformerFactory.Helmoperator().V1alpha1().HelmOperations()
	crdInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.onAdd,
		UpdateFunc: controller.onUpdate,
		DeleteFunc: controller.onDelete,
	})
	controller.applicationLister = crdInformer.Lister()

	podsInformer := podInformerFactory.Core().V1().Pods()
	helmPodEventHandler := newHelmPodEventHandler(controller.queue.AddRateLimited, controller.applicationLister)
	podsInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    helmPodEventHandler.onPodAdded,
		UpdateFunc: helmPodEventHandler.onPodUpdated,
		DeleteFunc: helmPodEventHandler.onPodDeleted,
	})
	controller.podLister = podsInformer.Lister()

	controller.cacheSynced = func() bool {
		return crdInformer.Informer().HasSynced() && podsInformer.Informer().HasSynced()
	}

	return controller
}

// Start starts the Controller by registering a watcher for HelmOperation objects.
func (c *Controller) Start(workers int, stopCh <-chan struct{}) error {
	glog.Info("Starting the workers of the HelmOperation controller")
	for i := 0; i < workers; i++ {
		// runWorker will loop until "something bad" happens. Until will then rekick
		// the worker after one second.
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	// Wait for all involved caches to be synced, before processing items from the queue is started.
	if !cache.WaitForCacheSync(stopCh, c.cacheSynced) {
		return fmt.Errorf("timed out waiting for cache to sync")
	}
	return nil
}

// Stop stops the controller.
func (c *Controller) Stop() {
	glog.Info("Stopping the HelmOperation controller")
	c.queue.ShutDown()
}

// Callback function called when a new HelmOperation object gets created.
func (c *Controller) onAdd(obj interface{}) {
	app := obj.(*v1alpha1.HelmOperation)
	glog.Infof("HelmOperation %s/%s was added, enqueueing it for submission", app.Namespace, app.Name)
	c.enqueue(app)
}

func (c *Controller) onUpdate(oldObj, newObj interface{}) {
	oldApp := oldObj.(*v1alpha1.HelmOperation)
	newApp := newObj.(*v1alpha1.HelmOperation)

	// The spec has changed. This is currently best effort as we can potentially miss updates
	// and end up in an inconsistent state.
	if !reflect.DeepEqual(oldApp.Spec, newApp.Spec) {
		// Force-set the application status to Invalidating which handles clean-up and application re-run.
		if _, err := c.updateApplicationStatusWithRetries(newApp, func(status *v1alpha1.HelmOperationStatus) {
			status.OpState.State = v1alpha1.InvalidatingState
		}); err != nil {
			c.recorder.Eventf(
				newApp,
				apiv1.EventTypeWarning,
				"HelmOperationSpecUpdateFailed",
				"failed to process spec update for HelmOperation %s: %v",
				newApp.Name,
				err)
			return
		}

		c.recorder.Eventf(
			newApp,
			apiv1.EventTypeNormal,
			"HelmOperationSpecUpdateProcessed",
			"Successfully processed spec update for HelmOperation %s",
			newApp.Name)
	}

	glog.V(2).Infof("HelmOperation %s/%s was updated, enqueueing it", newApp.Namespace, newApp.Name)
	c.enqueue(newApp)
}

func (c *Controller) onDelete(obj interface{}) {
	var app *v1alpha1.HelmOperation
	switch obj.(type) {
	case *v1alpha1.HelmOperation:
		app = obj.(*v1alpha1.HelmOperation)
	case cache.DeletedFinalStateUnknown:
		deletedObj := obj.(cache.DeletedFinalStateUnknown).Obj
		app = deletedObj.(*v1alpha1.HelmOperation)
	}

	if app != nil {
		c.handleHelmOperationDeletion(app)
		c.recorder.Eventf(
			app,
			apiv1.EventTypeNormal,
			"HelmOperationDeleted",
			"HelmOperation %s was deleted",
			app.Name)
	}
}

// runWorker runs a single controller worker.
func (c *Controller) runWorker() {
	defer utilruntime.HandleCrash()
	for c.processNextItem() {
	}
}

func (c *Controller) processNextItem() bool {
	key, quit := c.queue.Get()

	if quit {
		return false
	}
	defer c.queue.Done(key)

	glog.V(2).Infof("Starting processing key: %q", key)
	defer glog.V(2).Infof("Ending processing key: %q", key)
	err := c.syncHelmOperation(key.(string))
	if err == nil {
		// Successfully processed the key or the key was not found so tell the queue to stop tracking
		// history for your key. This will reset things like failure counts for per-item rate limiting.
		c.queue.Forget(key)
		return true
	}

	// There was a failure so be sure to report it. This method allows for pluggable error handling
	// which can be used for things like cluster-monitoring
	utilruntime.HandleError(fmt.Errorf("failed to sync HelmOperation %q: %v", key, err))
	return true
}

func (c *Controller) getAppPods(app *v1alpha1.HelmOperation) ([]*apiv1.Pod, error) {
	// Fetch all the pods for the current run of the application.
	selector := labels.SelectorFromSet(labels.Set(getResourceLabels(app)))
	pods, err := c.podLister.Pods(app.Namespace).List(selector)
	if err != nil {
		return nil, fmt.Errorf("failed to get pods for HelmOperation %s/%s: %v", app.Namespace, app.Name, err)
	}
	return pods, nil
}

func (c *Controller) getAndUpdateOpState(app *v1alpha1.HelmOperation) error {
	// if err := c.getAndUpdateDriverState(app); err != nil {
	// 	return err
	// }
	// if err := c.getAndUpdateExecutorState(app); err != nil {
	// 	return err
	// }
	return nil
}

func (c *Controller) handleHelmOperationDeletion(app *v1alpha1.HelmOperation) {
	// HelmOperation deletion requested, lets delete driver pod.
	if err := c.deleteHelmResources(app); err != nil {
		glog.Errorf("failed to delete resources associated with deleted HelmOperation %s/%s: %v", app.Namespace, app.Name, err)
	}
}


func (c *Controller) syncHelmOperation(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("failed to get the namespace and name from key %s: %v", key, err)
	}
	app, err := c.getHelmOperation(namespace, name)
	if err != nil {
		return err
	}
	if app == nil {
		// HelmOperation not found.
		return nil
	}
	if !app.DeletionTimestamp.IsZero() {
		c.handleHelmOperationDeletion(app)
		return nil
	}

	appToUpdate := app.DeepCopy()

	// Take action based on application state.
	switch appToUpdate.Status.OpState.State {

	case v1alpha1.NewState:
		c.recordHelmOperationEvent(appToUpdate)
		appToUpdate = c.submitHelmOperation(appToUpdate)

	case v1alpha1.SucceedingState:
		appToUpdate.Status.OpState.State = v1alpha1.CompletedState
		c.recordHelmOperationEvent(appToUpdate)

	case v1alpha1.FailingState:
		appToUpdate.Status.OpState.State = v1alpha1.FailedState
		c.recordHelmOperationEvent(appToUpdate)

	case v1alpha1.FailedSubmissionState:
		appToUpdate.Status.OpState.State = v1alpha1.FailedState
		c.recordHelmOperationEvent(appToUpdate)

	case v1alpha1.InvalidatingState:
		// Invalidate the current run and enqueue the HelmOperation for re-execution.
		if err := c.deleteHelmResources(appToUpdate); err != nil {
			glog.Errorf("failed to delete resources associated with HelmOperation %s/%s: %v",
				appToUpdate.Namespace, appToUpdate.Name, err)
			return err
		}
		c.clearStatus(&appToUpdate.Status)

	case v1alpha1.SubmittedState, v1alpha1.RunningState, v1alpha1.UnknownState:
		if err := c.getAndUpdateOpState(appToUpdate); err != nil {
			return err
		}
	}

	if appToUpdate != nil {
		glog.V(2).Infof("Trying to update HelmOperation %s/%s, from: [%v] to [%v]", app.Namespace, app.Name, app.Status, appToUpdate.Status)
		err = c.updateStatus(app, appToUpdate)
		if err != nil {
			glog.Errorf("failed to update HelmOperation %s/%s: %v", app.Namespace, app.Name, err)
			return err
		}
	}

	return nil
}

// submitHelmOperation creates a new submission for the given HelmOperation.
func (c *Controller) submitHelmOperation(app *v1alpha1.HelmOperation) *v1alpha1.HelmOperation {

	submissionID := uuid.New().String()
	submissionCmdArgs, err := buildSubmissionCommandArgs(app, submissionID)
	if err != nil {
		app.Status = v1alpha1.HelmOperationStatus{
			OpState: v1alpha1.ApplicationState{
				State:        v1alpha1.FailedSubmissionState,
				ErrorMessage: err.Error(),
			},
			SubmissionAttempts:        app.Status.SubmissionAttempts + 1,
			LastSubmissionAttemptTime: metav1.Now(),
		}
		return app
	}

	// Try submitting the application by running spark-submit.
	submitted, err := runSparkSubmit(newSubmission(submissionCmdArgs, app))
	if err != nil {
		app.Status = v1alpha1.HelmOperationStatus{
			OpState: v1alpha1.ApplicationState{
				State:        v1alpha1.FailedSubmissionState,
				ErrorMessage: err.Error(),
			},
			SubmissionAttempts:        app.Status.SubmissionAttempts + 1,
			LastSubmissionAttemptTime: metav1.Now(),
		}
		c.recordHelmOperationEvent(app)
		glog.Errorf("failed to run spark-submit for HelmOperation %s/%s: %v", app.Namespace, app.Name, err)
		return app
	}
	if !submitted {
		// The application may not have been submitted even if err == nil, e.g., when some
		// state update caused an attempt to re-submit the application, in which case no
		// error gets returned from runSparkSubmit. If this is the case, we simply return.
		return app
	}

	glog.Infof("HelmOperation %s/%s has been submitted", app.Namespace, app.Name)
	app.Status = v1alpha1.HelmOperationStatus{
		SubmissionID: submissionID,
		OpState: v1alpha1.ApplicationState{
			State: v1alpha1.SubmittedState,
		},
		SubmissionAttempts:        app.Status.SubmissionAttempts + 1,
		ExecutionAttempts:         app.Status.ExecutionAttempts + 1,
		LastSubmissionAttemptTime: metav1.Now(),
	}
	c.recordHelmOperationEvent(app)

	service, err := createSparkUIService(app, c.kubeClient)
	if err != nil {
		glog.Errorf("failed to create UI service for HelmOperation %s/%s: %v", app.Namespace, app.Name, err)
	} else {
		app.Status.DriverInfo.WebUIServiceName = service.serviceName
		app.Status.DriverInfo.WebUIPort = service.nodePort
		// Create UI Ingress if ingress-format is set.
		if c.ingressURLFormat != "" {
			ingress, err := createSparkUIIngress(app, *service, c.ingressURLFormat, c.kubeClient)
			if err != nil {
				glog.Errorf("failed to create UI Ingress for HelmOperation %s/%s: %v", app.Namespace, app.Name, err)
			} else {
				app.Status.DriverInfo.WebUIIngressAddress = ingress.ingressURL
				app.Status.DriverInfo.WebUIIngressName = ingress.ingressName
			}
		}
	}
	return app
}

func (c *Controller) updateApplicationStatusWithRetries(
	original *v1alpha1.HelmOperation,
	updateFunc func(status *v1alpha1.HelmOperationStatus)) (*v1alpha1.HelmOperation, error) {
	toUpdate := original.DeepCopy()

	var lastUpdateErr error
	for i := 0; i < maximumUpdateRetries; i++ {
		updateFunc(&toUpdate.Status)
		if reflect.DeepEqual(original.Status, toUpdate.Status) {
			return toUpdate, nil
		}
		_, err := c.crdClient.Sparkoperatorv1alpha1().HelmOperations(toUpdate.Namespace).Update(toUpdate)
		if err == nil {
			return toUpdate, nil
		}

		lastUpdateErr = err

		// Failed to update to the API server.
		// Get the latest version from the API server first and re-apply the update.
		name := toUpdate.Name
		toUpdate, err = c.crdClient.Sparkoperatorv1alpha1().HelmOperations(toUpdate.Namespace).Get(name,
			metav1.GetOptions{})
		if err != nil {
			glog.Errorf("failed to get HelmOperation %s/%s: %v", original.Namespace, name, err)
			return nil, err
		}
	}

	if lastUpdateErr != nil {
		glog.Errorf("failed to update HelmOperation %s/%s: %v", original.Namespace, original.Name, lastUpdateErr)
		return nil, lastUpdateErr
	}

	return toUpdate, nil
}

// updateStatus updates the status of the HelmOperation.
func (c *Controller) updateStatus(oldApp, newApp *v1alpha1.HelmOperation) error {
	// Skip update if nothing changed.
	if reflect.DeepEqual(oldApp, newApp) {
		return nil
	}

	updatedApp, err := c.updateApplicationStatusWithRetries(oldApp, func(status *v1alpha1.HelmOperationStatus) {
		*status = newApp.Status
	})

	// Export metrics if the update was successful.
	if err == nil && c.metrics != nil {
		c.metrics.exportMetrics(oldApp, updatedApp)
	}

	return err
}

func (c *Controller) getHelmOperation(namespace string, name string) (*v1alpha1.HelmOperation, error) {
	app, err := c.applicationLister.HelmOperations(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return app, nil
}

// Delete the driver pod and optional UI resources (Service/Ingress) created for the application.
func (c *Controller) deleteHelmResources(app *v1alpha1.HelmOperation) error {
	err := c.getAndUpdateDriverState(app)
	if err != nil {
		return err
	}

	driverPodName := app.Status.DriverInfo.PodName
	if driverPodName != "" {
		glog.V(2).Infof("Deleting pod %s in namespace %s", driverPodName, app.Namespace)
		err := c.kubeClient.CoreV1().Pods(app.Namespace).Delete(driverPodName, metav1.NewDeleteOptions(0))
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	sparkUIServiceName := app.Status.DriverInfo.WebUIServiceName
	if sparkUIServiceName != "" {
		glog.V(2).Infof("Deleting Spark UI Service %s in namespace %s", sparkUIServiceName, app.Namespace)
		err := c.kubeClient.CoreV1().Services(app.Namespace).Delete(sparkUIServiceName, metav1.NewDeleteOptions(0))
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	sparkUIIngressName := app.Status.DriverInfo.WebUIIngressName
	if sparkUIIngressName != "" {
		glog.V(2).Infof("Deleting Spark UI Ingress %s in namespace %s", sparkUIIngressName, app.Namespace)
		err := c.kubeClient.Extensionsv1alpha1().Ingresses(app.Namespace).Delete(sparkUIIngressName, metav1.NewDeleteOptions(0))
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	return nil
}

// Validate that any Spark resources (driver/Service/Ingress) created for the application have been deleted.
func (c *Controller) validateSparkResourceDeletion(app *v1alpha1.HelmOperation) bool {
	driverPodName := app.Status.DriverInfo.PodName
	if driverPodName != "" {
		_, err := c.kubeClient.CoreV1().Pods(app.Namespace).Get(driverPodName, metav1.GetOptions{})
		if err == nil || !errors.IsNotFound(err) {
			return false
		}
	}

	sparkUIServiceName := app.Status.DriverInfo.WebUIServiceName
	if sparkUIServiceName != "" {
		_, err := c.kubeClient.CoreV1().Services(app.Namespace).Get(sparkUIServiceName, metav1.GetOptions{})
		if err == nil || !errors.IsNotFound(err) {
			return false
		}
	}

	sparkUIIngressName := app.Status.DriverInfo.WebUIIngressName
	if sparkUIIngressName != "" {
		_, err := c.kubeClient.Extensionsv1alpha1().Ingresses(app.Namespace).Get(sparkUIIngressName, metav1.GetOptions{})
		if err == nil || !errors.IsNotFound(err) {
			return false
		}
	}

	return true
}

func (c *Controller) enqueue(obj interface{}) {
	key, err := keyFunc(obj)
	if err != nil {
		glog.Errorf("failed to get key for %v: %v", obj, err)
		return
	}

	c.queue.AddRateLimited(key)
}

// Return IP of the node. If no External IP is found, Internal IP will be returned
func (c *Controller) getNodeIP(nodeName string) string {
	node, err := c.kubeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
	if err != nil {
		glog.Errorf("failed to get node %s", nodeName)
		return ""
	}

	for _, address := range node.Status.Addresses {
		if address.Type == apiv1.NodeExternalIP {
			return address.Address
		}
	}
	for _, address := range node.Status.Addresses {
		if address.Type == apiv1.NodeInternalIP {
			return address.Address
		}
	}
	return ""
}

func (c *Controller) recordHelmOperationEvent(app *v1alpha1.HelmOperation) {
	switch app.Status.OpState.State {
	case v1alpha1.NewState:
		c.recorder.Eventf(
			app,
			apiv1.EventTypeNormal,
			"HelmOperationAdded",
			"HelmOperation %s was added, enqueuing it for submission",
			app.Name)
	case v1alpha1.SubmittedState:
		c.recorder.Eventf(
			app,
			apiv1.EventTypeNormal,
			"HelmOperationSubmitted",
			"HelmOperation %s was submitted successfully",
			app.Name)
	case v1alpha1.FailedSubmissionState:
		c.recorder.Eventf(
			app,
			apiv1.EventTypeWarning,
			"HelmOperationSubmissionFailed",
			"failed to submit HelmOperation %s: %s",
			app.Name,
			app.Status.OpState.ErrorMessage)
	case v1alpha1.CompletedState:
		c.recorder.Eventf(
			app,
			apiv1.EventTypeNormal,
			"HelmOperationCompleted",
			"HelmOperation %s completed",
			app.Name)
	case v1alpha1.FailedState:
		c.recorder.Eventf(
			app,
			apiv1.EventTypeWarning,
			"HelmOperationFailed",
			"HelmOperation %s failed: %s",
			app.Name,
			app.Status.OpState.ErrorMessage)
	case v1alpha1.PendingRerunState:
		c.recorder.Eventf(
			app,
			apiv1.EventTypeWarning,
			"HelmOperationPendingRerun",
			"HelmOperation %s is pending rerun",
			app.Name)
	}
}

func (c *Controller) recordDriverEvent(app *v1alpha1.HelmOperation, phase apiv1.PodPhase, name string) {
	switch phase {
	case apiv1.PodSucceeded:
		c.recorder.Eventf(app, apiv1.EventTypeNormal, "SparkDriverCompleted", "Driver %s completed", name)
	case apiv1.PodPending:
		c.recorder.Eventf(app, apiv1.EventTypeNormal, "SparkDriverPending", "Driver %s is pending", name)
	case apiv1.PodRunning:
		c.recorder.Eventf(app, apiv1.EventTypeNormal, "SparkDriverRunning", "Driver %s is running", name)
	case apiv1.PodFailed:
		c.recorder.Eventf(app, apiv1.EventTypeWarning, "SparkDriverFailed", "Driver %s failed", name)
	case apiv1.PodUnknown:
		c.recorder.Eventf(app, apiv1.EventTypeWarning, "SparkDriverUnknownState", "Driver %s in unknown state", name)
	}
}

func (c *Controller) recordExecutorEvent(app *v1alpha1.HelmOperation, state v1alpha1.ExecutorState, name string) {
	switch state {
	case v1alpha1.ExecutorCompletedState:
		c.recorder.Eventf(app, apiv1.EventTypeNormal, "SparkExecutorCompleted", "Executor %s completed", name)
	case v1alpha1.ExecutorPendingState:
		c.recorder.Eventf(app, apiv1.EventTypeNormal, "SparkExecutorPending", "Executor %s is pending", name)
	case v1alpha1.ExecutorRunningState:
		c.recorder.Eventf(app, apiv1.EventTypeNormal, "SparkExecutorRunning", "Executor %s is running", name)
	case v1alpha1.ExecutorFailedState:
		c.recorder.Eventf(app, apiv1.EventTypeWarning, "SparkExecutorFailed", "Executor %s failed", name)
	case v1alpha1.ExecutorUnknownState:
		c.recorder.Eventf(app, apiv1.EventTypeWarning, "SparkExecutorUnknownState", "Executor %s in unknown state", name)
	}
}

func (c *Controller) clearStatus(status *v1alpha1.HelmOperationStatus) {
	if status.OpState.State == v1alpha1.InvalidatingState {
		status.HelmOperationID = ""
		status.SubmissionAttempts = 0
		status.ExecutionAttempts = 0
		status.LastSubmissionAttemptTime = metav1.Time{}
		status.TerminationTime = metav1.Time{}
		status.ExecutorState = nil
	} else if status.OpState.State == v1alpha1.PendingRerunState {
		status.HelmOperationID = ""
		status.DriverInfo = v1alpha1.DriverInfo{}
		status.ExecutorState = nil
	}
}