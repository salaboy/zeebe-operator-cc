/*

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

package controllers

import (
	"context"
	cc "github.com/camunda-community-hub/camunda-cloud-go-client/pkg/cc/client"
	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"log"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"time"

	zeebev1 "zeebe.io/m/v2/api/v1"
)

const (
	interval = 10 * time.Second
)

var ccClient cc.CCClient

func (r *ZeebeClusterReconciler) WaitForClusterStateChange(clusterId string, currentStatus cc.ClusterStatus) (cc.ClusterStatus, error) {

	wait.PollImmediateUntil(interval, func() (bool, error) {

		resp, err := ccClient.GetClusterDetails(clusterId)

		if err != nil {

			return false, err
		}

		log := r.Log.WithValues("ClusterId: ", clusterId, "CurrentState: ", currentStatus)
		log.Info("Reported by Camunda Cloud: ", "Cluster State: ", resp.Ready)

		if resp.Ready == currentStatus.Ready {
			return false, nil
		}
		return true, nil

	}, nil)
	return ccClient.GetClusterDetails(clusterId)

}

// ZeebeClusterReconciler reconciles a ZeebeCluster object
type ZeebeClusterReconciler struct {
	client.Client
	Log logr.Logger
}

func ignoreNotFound(err error) error {
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

// +kubebuilder:rbac:groups=zeebe.io.zeebe,resources=zeebeclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=zeebe.io.zeebe,resources=zeebeclusters/status,verbs=get;update;patch

func (r *ZeebeClusterReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("zeebecluster > ", req.NamespacedName)

	var zeebeCluster zeebev1.ZeebeCluster

	log.Info(">>> Fetching Info about resources: " + req.NamespacedName.Name)

	err := r.Get(ctx, req.NamespacedName, &zeebeCluster)
	if err != nil {
		// handle error
		// The cluster doesn't exist.. NOOP
		//log.Error(err, "Failed to get Zeebe Cluster")
	}
	// name of your custom finalizer
	myFinalizerName := "zeebecluster.cloud.camunda.com"

	if zeebeCluster.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object.
		if !containsString(zeebeCluster.ObjectMeta.Finalizers, myFinalizerName) {
			zeebeCluster.ObjectMeta.Finalizers = append(zeebeCluster.ObjectMeta.Finalizers, myFinalizerName)
			if err := r.Update(context.Background(), &zeebeCluster); err != nil {
				return reconcile.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if containsString(zeebeCluster.ObjectMeta.Finalizers, myFinalizerName) {
			// our finalizer is present, so lets handle our external dependency
			if err := r.deleteExternalDependency(&zeebeCluster); err != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried
				return reconcile.Result{}, err
			}

			// remove our finalizer from the list and update it.
			zeebeCluster.ObjectMeta.Finalizers = removeString(zeebeCluster.ObjectMeta.Finalizers, myFinalizerName)
			if err := r.Update(context.Background(), &zeebeCluster); err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}
	// Create cluster if resource doesn't provide a cluster Id
	log.Info("Checking Cluster ID for Creating Cluster Before Creation: " + zeebeCluster.Spec.ClusterId + "Owner : " + zeebeCluster.Spec.Owner)
	if zeebeCluster.Spec.ClusterId == "" && zeebeCluster.Spec.Owner != "CC" {
		log.Info("Creating Cluster: " + zeebeCluster.Spec.ClusterId)
		clusterId, err := ccClient.CreateClusterWithParams(req.NamespacedName.Name, zeebeCluster.Spec.PlanName,
			zeebeCluster.Spec.ChannelName, zeebeCluster.Spec.GenerationName, zeebeCluster.Spec.Region)

		if err != nil {
			log.Error(err, "failed to create cluster")
			return reconcile.Result{}, err
		}
		log.Info("Updating Zeebe Cluster with: ", "ClusterId", clusterId)
		zeebeCluster.Spec.ClusterId = clusterId

		if err := r.Update(context.Background(), &zeebeCluster); err != nil {
			return reconcile.Result{}, err
		}
	}

	log.Info("Checking  for registering workers: ", "Cluster ID", zeebeCluster.Spec.ClusterId, " Tracking on: ", zeebeCluster.Spec.Track)
	if zeebeCluster.Spec.ClusterId != "" {
		name, err := ccClient.GetClusterByName(zeebeCluster.Name)
		if err != nil {
			log.Error(err, "failed to fetch cluster data")
			return reconcile.Result{}, err
		}
		modified := false
		if zeebeCluster.Spec.PlanName != name.ClusterPlantType.Name {
			zeebeCluster.Spec.PlanName = name.ClusterPlantType.Name
			modified = true
		}

		if zeebeCluster.Spec.GenerationName != name.Generation.Name {
			zeebeCluster.Spec.GenerationName = name.Generation.Name
			modified = true
		}

		if zeebeCluster.Spec.ChannelName != name.Channel.Name {
			zeebeCluster.Spec.ChannelName = name.Channel.Name
			modified = true
		}
		if zeebeCluster.Spec.Region != name.ClusterPlantType.K8sContext.Name {
			zeebeCluster.Spec.Region = name.ClusterPlantType.K8sContext.Name
			modified = true
		}
		if modified {
			if err := r.Update(context.Background(), &zeebeCluster); err != nil {
				return reconcile.Result{}, err
			}
		}
		log.Info("Registering worker to check cluster status: " + zeebeCluster.Spec.ClusterId)
		go workerPollCCClusterDetails(zeebeCluster.Spec.ClusterId, r, zeebeCluster)
	}

	return ctrl.Result{}, nil
}

var events = make(chan event.GenericEvent)

func workerPollCCClusterDetails(clusterId string, r *ZeebeClusterReconciler, zeebeCluster zeebev1.ZeebeCluster) {

	ticker := time.NewTicker(10000 * time.Millisecond)

	for {
		select {
		case <-ticker.C:
			resp, err := ccClient.GetClusterDetails(clusterId)
			if err != nil {
				r.Log.Error(err, "fetching cluster status details failed...")
			}
			r.Log.Info("Worker ("+clusterId+")", "ClusterId: ", clusterId, "Cluster Name: ",
				zeebeCluster.Name, "Cluster Namespace", zeebeCluster.Namespace, "Cluster State: ", resp.Ready)
			if diff := cmp.Diff(zeebeCluster.Status.ClusterStatus, resp); diff != "" {
				r.Log.Info("Cluster " + zeebeCluster.Name + "  Status changed: " + diff)
				zeebeCluster.Status.ClusterStatus = resp
				if err := r.Status().Update(context.Background(), &zeebeCluster); err != nil {
					//Status updated already...
					//r.Log.Error(err, "failed to update cluster status")
				} else {
					r.Log.Info("Status updated for", "clusterId", clusterId, "status", zeebeCluster.Status.ClusterStatus)
				}
			}
		}
	}

}

func (r *ZeebeClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {

	controller, err := ctrl.NewControllerManagedBy(mgr).For(&zeebev1.ZeebeCluster{}).Build(r)

	controller.Watch(
		&source.Channel{Source: events},
		&handler.EnqueueRequestForObject{},
	)
	return err
}

func (r *ZeebeClusterReconciler) deleteExternalDependency(zeebeCluster *zeebev1.ZeebeCluster) error {
	log.Printf("Trying to delete the cluster in camunda cloud")
	if zeebeCluster.Status.ClusterStatus.Ready != "Not Found" {
		deleted, err := ccClient.DeleteCluster(zeebeCluster.Spec.ClusterId)
		if err != nil {
			log.Printf("Failed to delete cluster: cluster not found %s", err)
		}
		if deleted {
			log.Printf("Cluster in camunda cloud deleted")
		}
	} else {
		log.Printf("Cluster in Not Found State, nothing to delete remotely")
	}

	return nil

}

//
// Helper functions to check and remove string from a slice of strings.
//
func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) (result []string) {
	for _, item := range slice {
		if item == s {
			continue
		}
		result = append(result, item)
	}
	return
}
