/*
Copyright 2021.

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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	webappv1 "my.domain/appservice/api/v1"
)

// AppServiceReconciler reconciles a AppService object
type AppServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=webapp.my.domain,resources=appservices,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=webapp.my.domain,resources=appservices/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=webapp.my.domain,resources=appservices/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the AppService object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *AppServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reqLogger := log.FromContext(ctx)
	reqLogger.Info("Reconciling AppService")

	// 获取这个AppService crd ,这里是检查这个 crd资源是否存在
	instance := &webappv1.AppService{}
	err := r.Client.Get(context.TODO(), req.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	if instance.DeletionTimestamp != nil {
		return reconcile.Result{}, err
	}

	// 下面是检查这个crd资源对应的 dep和svc是否存在

	// 如果不存在，则创建关联资源
	// 如果存在，判断是否需要更新
	//   如果需要更新，则直接更新
	//   如果不需要更新，则正常返回

	deploy := &appsv1.Deployment{}
	if err := r.Client.Get(context.TODO(), req.NamespacedName, deploy); err != nil && errors.IsNotFound(err) {
		// 创建关联资源
		// 1. 创建 Deploy
		deploy := NewDeploy(instance)
		if err := r.Client.Create(context.TODO(), deploy); err != nil {
			reqLogger.Error(err, "dep.create.err")
			return reconcile.Result{}, err
		}
		// 2. 创建 Service
		service := NewService(instance)
		if err := r.Client.Create(context.TODO(), service); err != nil {
			reqLogger.Error(err, "svc.create.err")
			return reconcile.Result{}, err
		}
		// 3. 关联 Annotations
		data, err := json.Marshal(instance.Spec)
		if err != nil {
			reqLogger.Error(err, "instance.Spec.json.Marshal.err")
			return reconcile.Result{}, nil
		}
		reqLogger.Info("create.instance.Spec.data.string", "spce", string(data))
		if instance.Annotations != nil {
			instance.Annotations["spec"] = string(data)
		} else {
			instance.Annotations = map[string]string{"spec": string(data)}
		}

		if err := r.Client.Update(context.TODO(), instance); err != nil {
			reqLogger.Error(err, "dep.update.err")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, nil
	}

	oldspec := webappv1.AppServiceSpec{}
	if err := json.Unmarshal([]byte(instance.Annotations["spec"]), &oldspec); err != nil {
		reqLogger.Error(err, "oldspec.json.Unmarshal.err")
		return reconcile.Result{}, err
	}

	if !reflect.DeepEqual(instance.Spec, oldspec) {
		specOldData := instance.Annotations["spec"]
		specNewData, _ := json.Marshal(instance.Spec)
		reqLogger.Info("update.instance.Spec.data.diff", "specOldData", specOldData, "specNewData", string(specNewData))

		// 更新关联资源
		newDeploy := NewDeploy(instance)
		oldDeploy := &appsv1.Deployment{}
		if err := r.Client.Get(context.TODO(), req.NamespacedName, oldDeploy); err != nil {
			reqLogger.Error(err, "oldDeploy.get.err")
			return reconcile.Result{}, err
		}
		oldDeploy.Spec = newDeploy.Spec
		if err := r.Client.Update(context.TODO(), oldDeploy); err != nil {
			reqLogger.Error(err, "oldDeploy.update.err")
			return reconcile.Result{}, err
		}

		newService := NewService(instance)
		oldService := &corev1.Service{}
		if err := r.Client.Get(context.TODO(), req.NamespacedName, oldService); err != nil {
			reqLogger.Error(err, "oldService.get.err")
			return reconcile.Result{}, err
		}
		oldService.Spec = newService.Spec
		if err := r.Client.Update(context.TODO(), oldService); err != nil {
			reqLogger.Error(err, "oldService.update.err")
			return reconcile.Result{}, err
		}

		// 3. 关联 Annotations
		data, err := json.Marshal(instance.Spec)
		if err != nil {
			reqLogger.Error(err, "instance.Spec.json.Marshal.err")
			return reconcile.Result{}, nil
		}
		reqLogger.Info("update.instance.Spec.data.string", "spce", string(data))
		if instance.Annotations != nil {
			instance.Annotations["spec"] = string(data)
		} else {
			instance.Annotations = map[string]string{"spec": string(data)}
		}

		if err := r.Client.Update(context.TODO(), instance); err != nil {
			reqLogger.Error(err, "dep.update.err")
			return reconcile.Result{}, nil
		}

		return reconcile.Result{}, nil

	}

	return reconcile.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AppServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&webappv1.AppService{}).
		Complete(r)
}
