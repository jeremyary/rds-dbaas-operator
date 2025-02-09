/*
Copyright 2022.

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
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	v1 "k8s.io/api/apps/v1"
	rbac "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	label "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	dbaasoperator "github.com/RHEcosystemAppEng/dbaas-operator/api/v1beta1"
)

const (
	providerKind   = "DBaaSProvider"
	providerCRName = "rds-registration"

	relatedToLabelName  = "related-to"
	relatedToLabelValue = "dbaas-operator"
	typeLabelName       = "type"
	typeLabelValue      = "dbaas-provider-registration"

	dbaasproviderCRFile = "rds_registration.yaml"
)

var labels = map[string]string{relatedToLabelName: relatedToLabelValue, typeLabelName: typeLabelValue}

type DBaaSProviderReconciler struct {
	client.Client
	*runtime.Scheme
	Clientset                *kubernetes.Clientset
	DBaaSProviderCRFilePath  string
	operatorNameVersion      string
	operatorInstallNamespace string
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;create;update;delete;watch
// +kubebuilder:rbac:groups=dbaas.redhat.com,resources=dbaasproviders,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dbaas.redhat.com,resources=dbaasproviders/status,verbs=get;update;patch

func (r *DBaaSProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx, "DBaaSProvider", req.NamespacedName, "during", "DBaaSProvider Reconciler")

	// due to predicate filtering, we'll only reconcile this operator's own deployment when it's seen the first time
	// meaning we have a reconcile entry-point on operator start-up, so now we can create a cluster-scoped resource
	// owned by the operator's ClusterRole to ensure cleanup on uninstall

	dep := &v1.Deployment{}
	if err := r.Get(ctx, req.NamespacedName, dep); err != nil {
		if errors.IsNotFound(err) {
			// CR deleted since request queued, child objects getting GC'd, no requeue
			logger.Info("deployment not found, deleted, no requeue")
			return ctrl.Result{}, nil
		}
		// error fetching deployment, requeue and try again
		logger.Error(err, "error fetching Deployment CR")
		return ctrl.Result{}, err
	}

	isCrdInstalled, err := r.checkCrdInstalled(dbaasoperator.GroupVersion.String(), providerKind)
	if err != nil {
		logger.Error(err, "error discovering GVK")
		return ctrl.Result{}, err
	}
	if !isCrdInstalled {
		logger.Info("CRD not found, requeueing with rate limiter")
		// returning with 'Requeue: true' will invoke our custom rate limiter seen in SetupWithManager below
		return ctrl.Result{Requeue: true}, nil
	}

	// RDS controller registration custom resource isn't present,so create now with ClusterRole owner for GC
	opts := &client.ListOptions{
		LabelSelector: label.SelectorFromSet(map[string]string{
			"olm.owner":      r.operatorNameVersion,
			"olm.owner.kind": "ClusterServiceVersion",
		}),
	}
	clusterRoleList := &rbac.ClusterRoleList{}
	if err := r.List(context.Background(), clusterRoleList, opts); err != nil {
		logger.Error(err, "unable to list ClusterRoles to seek potential operand owners")
		return ctrl.Result{}, err
	}

	if len(clusterRoleList.Items) < 1 {
		err := errors.NewNotFound(
			schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "ClusterRole"}, "potentialOwner")
		logger.Error(err, "could not find ClusterRole owned by CSV to inherit operand")
		return ctrl.Result{}, err
	}

	instance := &dbaasoperator.DBaaSProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: providerCRName,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, instance, func() error {
		provider, err := readProviderCRFile(filepath.Join(r.DBaaSProviderCRFilePath, dbaasproviderCRFile))
		if err != nil {
			return err
		}
		bridgeProviderCR(instance, provider, clusterRoleList)
		return nil
	})
	if err != nil {
		logger.Error(err, "error while creating or updating new cluster-scoped resource")
		return ctrl.Result{}, err
	}
	logger.Info("cluster-scoped resource created or updated")

	return ctrl.Result{}, nil
}

// bridgeProviderCR CR for RDS registration
func bridgeProviderCR(instance *dbaasoperator.DBaaSProvider, provider *dbaasoperator.DBaaSProvider, clusterRoleList *rbac.ClusterRoleList) {
	instance.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
		{

			APIVersion:         "rbac.authorization.k8s.io/v1",
			Kind:               "ClusterRole",
			UID:                clusterRoleList.Items[0].GetUID(),
			Name:               clusterRoleList.Items[0].Name,
			Controller:         pointer.BoolPtr(true),
			BlockOwnerDeletion: pointer.BoolPtr(false),
		},
	}
	instance.ObjectMeta.Labels = labels
	instance.Spec = provider.Spec
}

func readProviderCRFile(file string) (*dbaasoperator.DBaaSProvider, error) {
	d, err := ioutil.ReadFile(filepath.Clean(file))
	if err != nil {
		return nil, err
	}
	jsonData, err := yaml.ToJSON(d)
	if err != nil {
		return nil, err
	}
	provider := &dbaasoperator.DBaaSProvider{}
	if err := json.Unmarshal(jsonData, provider); err != nil {
		return nil, err
	}

	return provider, nil
}

// CheckCrdInstalled checks whether dbaas provider CRD, has been created yet
func (r *DBaaSProviderReconciler) checkCrdInstalled(groupVersion, kind string) (bool, error) {
	resources, err := r.Clientset.Discovery().ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, r := range resources.APIResources {
		if r.Kind == kind {
			return true, nil
		}
	}
	return false, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DBaaSProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	logger := log.FromContext(context.Background(), "DBaaSProvider", "Manager", "during", "DBaaSProviderReconciler setup")

	// envVar set in controller-manager's Deployment YAML
	if operatorInstallNamespace, found := os.LookupEnv("INSTALL_NAMESPACE"); !found {
		err := fmt.Errorf("INSTALL_NAMESPACE must be set")
		logger.Error(err, "error fetching envVar")
		return err
	} else {
		r.operatorInstallNamespace = operatorInstallNamespace
	}

	// envVar set for all operators
	if operatorNameEnvVar, found := os.LookupEnv("OPERATOR_CONDITION_NAME"); !found {
		err := fmt.Errorf("OPERATOR_CONDITION_NAME must be set")
		logger.Error(err, "error fetching envVar")
		return err
	} else {
		r.operatorNameVersion = operatorNameEnvVar
	}

	customRateLimiter := workqueue.NewItemExponentialFailureRateLimiter(30*time.Second, 30*time.Minute)

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{RateLimiter: customRateLimiter}).
		For(&v1.Deployment{}).
		WithEventFilter(r.ignoreOtherDeployments()).
		Complete(r)
}

//ignoreOtherDeployments  only on a 'create' event is issued for the deployment
func (r *DBaaSProviderReconciler) ignoreOtherDeployments() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return r.evaluatePredicateObject(e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

func (r *DBaaSProviderReconciler) evaluatePredicateObject(obj client.Object) bool {
	lbls := obj.GetLabels()
	if obj.GetNamespace() == r.operatorInstallNamespace {
		if val, keyFound := lbls["olm.owner.kind"]; keyFound {
			if val == "ClusterServiceVersion" {
				if val, keyFound := lbls["olm.owner"]; keyFound {
					return val == r.operatorNameVersion
				}
			}
		}
	}
	return false
}
