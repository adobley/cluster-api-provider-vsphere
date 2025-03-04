/*
Copyright 2021 The Kubernetes Authors.

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
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/remote"
	clusterutilv1 "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vmwarev1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/vmware/v1beta1"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	vmwarecontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context/vmware"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/record"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
)

// +kubebuilder:rbac:groups=vmware.infrastructure.cluster.x-k8s.io,resources=providerserviceaccounts,verbs=get;list;watch;
// +kubebuilder:rbac:groups=vmware.infrastructure.cluster.x-k8s.io,resources=providerserviceaccounts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete

const (
	// ProviderServiceAccountControllerName defines the controller used when creating clients.
	ProviderServiceAccountControllerName = "provider-serviceaccount-controller"
	kindProviderServiceAccount           = "ProviderServiceAccount"
	systemServiceAccountPrefix           = "system.serviceaccount"
)

// AddServiceAccountProviderControllerToManager adds this controller to the provided manager.
func AddServiceAccountProviderControllerToManager(ctx context.Context, controllerManagerCtx *capvcontext.ControllerManagerContext, mgr manager.Manager, tracker *remote.ClusterCacheTracker, options controller.Options) error {
	var (
		controlledType     = &vmwarev1.ProviderServiceAccount{}
		controlledTypeName = reflect.TypeOf(controlledType).Elem().Name()

		controllerNameShort = fmt.Sprintf("%s-controller", strings.ToLower(controlledTypeName))
		controllerNameLong  = fmt.Sprintf("%s/%s/%s", controllerManagerCtx.Namespace, controllerManagerCtx.Name, controllerNameShort)
	)

	r := &ServiceAccountReconciler{
		Client:                    controllerManagerCtx.Client,
		Recorder:                  record.New(mgr.GetEventRecorderFor(controllerNameLong)),
		remoteClusterCacheTracker: tracker,
	}

	clusterToInfraFn := clusterToSupervisorInfrastructureMapFunc(ctx, controllerManagerCtx.Client)

	return ctrl.NewControllerManagedBy(mgr).For(controlledType).
		WithOptions(options).
		// Watch a ProviderServiceAccount
		Watches(
			&vmwarev1.ProviderServiceAccount{},
			handler.EnqueueRequestsFromMapFunc(r.providerServiceAccountToVSphereCluster),
		).
		// Watches the secrets to re-enqueue once the service account token is set
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToVSphereCluster),
		).
		// Watches clusters and reconciles the vSphereCluster
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
				requests := clusterToInfraFn(ctx, o)
				if len(requests) == 0 {
					return nil
				}

				log := ctrl.LoggerFrom(ctx).WithValues("VSphereCluster", klog.KRef(requests[0].Namespace, requests[0].Name))
				ctx = ctrl.LoggerInto(ctx, log)

				c := &vmwarev1.VSphereCluster{}
				if err := r.Client.Get(ctx, requests[0].NamespacedName, c); err != nil {
					log.V(4).Error(err, "Failed to get VSphereCluster")
					return nil
				}

				if annotations.IsExternallyManaged(c) {
					log.V(4).Info("VSphereCluster is externally managed, skipping mapping.")
					return nil
				}
				return requests
			}),
		).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), controllerManagerCtx.WatchFilterValue)).
		Complete(r)
}

func clusterToSupervisorInfrastructureMapFunc(ctx context.Context, c client.Client) handler.MapFunc {
	gvk := vmwarev1.GroupVersion.WithKind(reflect.TypeOf(&vmwarev1.VSphereCluster{}).Elem().Name())
	return clusterutilv1.ClusterToInfrastructureMapFunc(ctx, gvk, c, &vmwarev1.VSphereCluster{})
}

// ServiceAccountReconciler reconciles changes to ProviderServiceAccounts.
type ServiceAccountReconciler struct {
	Client                    client.Client
	Recorder                  record.Recorder
	remoteClusterCacheTracker *remote.ClusterCacheTracker
}

func (r *ServiceAccountReconciler) Reconcile(ctx context.Context, req reconcile.Request) (_ reconcile.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)
	log.V(4).Info("Starting Reconcile")

	vsphereClusterKey := client.ObjectKey{Namespace: req.Namespace, Name: req.Name}

	// Note: This reconciler reconciles ProviderServiceAccounts so we have to add VSphereCluster ourselves.
	log = log.WithValues("VSphereCluster", klog.KRef(vsphereClusterKey.Namespace, vsphereClusterKey.Name))
	ctx = ctrl.LoggerInto(ctx, log)

	// Get the vSphereCluster for this request.
	vsphereCluster := &vmwarev1.VSphereCluster{}
	if err := r.Client.Get(ctx, vsphereClusterKey, vsphereCluster); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("VSphereCluster not found, won't reconcile")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// Create the patch helper.
	patchHelper, err := patch.NewHelper(vsphereCluster, r.Client)
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(
			err,
			"failed to init patch helper for %s %s/%s",
			vsphereCluster.GroupVersionKind(),
			vsphereCluster.Namespace,
			vsphereCluster.Name)
	}

	// Create the cluster context for this request.
	clusterContext := &vmwarecontext.ClusterContext{
		VSphereCluster: vsphereCluster,
		PatchHelper:    patchHelper,
	}

	// Always issue a patch when exiting this function so changes to the
	// resource are patched back to the API server.
	defer func() {
		if err := clusterContext.Patch(ctx); err != nil {
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	if !vsphereCluster.DeletionTimestamp.IsZero() {
		return r.ReconcileDelete(ctx, clusterContext)
	}

	cluster, err := clusterutilv1.GetClusterFromMetadata(ctx, r.Client, vsphereCluster.ObjectMeta)
	if err != nil {
		log.Error(err, "unable to get Cluster from VSphereCluster")
		return reconcile.Result{}, nil
	}
	log = log.WithValues("Cluster", klog.KObj(cluster))
	ctx = ctrl.LoggerInto(ctx, log)

	// Pause reconciliation if entire vSphereCluster or Cluster is paused
	if annotations.IsPaused(cluster, vsphereCluster) {
		log.V(4).Info("VSphereCluster %s/%s linked to a cluster that is paused",
			vsphereCluster.Namespace, vsphereCluster.Name)
		return reconcile.Result{}, nil
	}

	// We cannot proceed until we are able to access the target cluster. Until
	// then just return a no-op and wait for the next sync. This will occur when
	// the Cluster's status is updated with a reference to the secret that has
	// the Kubeconfig data used to access the target cluster.
	guestClient, err := r.remoteClusterCacheTracker.GetClient(ctx, client.ObjectKeyFromObject(cluster))
	if err != nil {
		if errors.Is(err, remote.ErrClusterLocked) {
			log.V(5).Info("Requeuing because another worker has the lock on the ClusterCacheTracker")
			return ctrl.Result{Requeue: true}, nil
		}
		log.Error(err, "The control plane is not ready yet")
		return reconcile.Result{RequeueAfter: clusterNotReadyRequeueTime}, nil
	}

	// Defer to the Reconciler for reconciling a non-delete event.
	return r.ReconcileNormal(ctx, &vmwarecontext.GuestClusterContext{
		ClusterContext: clusterContext,
		GuestClient:    guestClient,
	})
}

// ReconcileDelete handles delete events for ProviderServiceAccounts.
func (r *ServiceAccountReconciler) ReconcileDelete(ctx context.Context, clusterCtx *vmwarecontext.ClusterContext) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.V(4).Info("Reconciling deleting Provider ServiceAccounts")

	pSvcAccounts, err := r.getProviderServiceAccounts(ctx, clusterCtx)
	if err != nil {
		log.Error(err, "Error fetching ProviderServiceAccounts")
		return reconcile.Result{}, err
	}

	for _, pSvcAccount := range pSvcAccounts {
		// Delete entries for configmap with serviceaccount
		if err := r.deleteServiceAccountConfigMap(ctx, pSvcAccount); err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "unable to delete configmap entry for provider serviceaccount %s", pSvcAccount.Name)
		}
	}

	return reconcile.Result{}, nil
}

// ReconcileNormal handles create and update events for ProviderServiceAccounts.
func (r *ServiceAccountReconciler) ReconcileNormal(ctx context.Context, guestClusterCtx *vmwarecontext.GuestClusterContext) (_ reconcile.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)
	log.V(4).Info("Reconciling ProviderServiceAccount")

	defer func() {
		if reterr != nil {
			conditions.MarkFalse(guestClusterCtx.VSphereCluster, vmwarev1.ProviderServiceAccountsReadyCondition, vmwarev1.ProviderServiceAccountsReconciliationFailedReason,
				clusterv1.ConditionSeverityWarning, reterr.Error())
		} else {
			conditions.MarkTrue(guestClusterCtx.VSphereCluster, vmwarev1.ProviderServiceAccountsReadyCondition)
		}
	}()

	pSvcAccounts, err := r.getProviderServiceAccounts(ctx, guestClusterCtx.ClusterContext)
	if err != nil {
		log.Error(err, "Error fetching ProviderServiceAccounts")
		return reconcile.Result{}, err
	}
	err = r.ensureProviderServiceAccounts(ctx, guestClusterCtx, pSvcAccounts)
	if err != nil {
		log.Error(err, "Error ensuring ProviderServiceAccounts")
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// Ensure service accounts from provider spec is created.
func (r *ServiceAccountReconciler) ensureProviderServiceAccounts(ctx context.Context, guestClusterCtx *vmwarecontext.GuestClusterContext, pSvcAccounts []vmwarev1.ProviderServiceAccount) error {
	log := ctrl.LoggerFrom(ctx)

	for i, pSvcAccount := range pSvcAccounts {
		// Note: We have to use := here to not overwrite log & ctx outside the for loop.
		log := log.WithValues("ProviderServiceAccount", klog.KRef(pSvcAccount.Namespace, pSvcAccount.Name))
		ctx := ctrl.LoggerInto(ctx, log)

		if guestClusterCtx.Cluster != nil && annotations.IsPaused(guestClusterCtx.Cluster, &(pSvcAccounts[i])) {
			log.V(4).Info("ProviderServiceAccount linked to a cluster that is paused or has pause annotation")
			continue
		}

		// 1. Create service accounts by the name specified in Provider Spec
		if err := r.ensureServiceAccount(ctx, pSvcAccount); err != nil {
			return errors.Wrapf(err, "unable to create provider serviceaccount %s", pSvcAccount.Name)
		}
		// 2. Update configmap with serviceaccount
		if err := r.ensureServiceAccountConfigMap(ctx, pSvcAccount); err != nil {
			return errors.Wrapf(err, "unable to sync configmap for provider serviceaccount %s", pSvcAccount.Name)
		}
		// 3. Create secret of Service account token type for the service account
		if err := r.ensureServiceAccountSecret(ctx, pSvcAccount); err != nil {
			return errors.Wrapf(err, "unable to create provider serviceaccount secret %s", getServiceAccountSecretName(pSvcAccount))
		}

		// 4. Create the associated role for the service account
		if err := r.ensureRole(ctx, pSvcAccount); err != nil {
			return errors.Wrapf(err, "unable to create role for provider serviceaccount %s", pSvcAccount.Name)
		}

		// 5. Create the associated roleBinding for the service account
		if err := r.ensureRoleBinding(ctx, pSvcAccount); err != nil {
			return errors.Wrapf(err, "unable to create rolebinding for provider serviceaccount %s", pSvcAccount.Name)
		}

		// 6. Sync the service account with the target
		if err := r.syncServiceAccountSecret(ctx, guestClusterCtx, pSvcAccount); err != nil {
			return errors.Wrapf(err, "unable to sync secret for provider serviceaccount %s", pSvcAccount.Name)
		}
	}
	return nil
}

func (r *ServiceAccountReconciler) ensureServiceAccount(ctx context.Context, pSvcAccount vmwarev1.ProviderServiceAccount) error {
	log := ctrl.LoggerFrom(ctx)

	svcAccount := corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      getServiceAccountName(pSvcAccount),
			Namespace: pSvcAccount.Namespace,
		},
	}

	err := util.SetControllerReferenceWithOverride(&pSvcAccount, &svcAccount, r.Client.Scheme())
	if err != nil {
		return err
	}
	log.V(4).Info("Creating service account")
	err = r.Client.Create(ctx, &svcAccount)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		// Note: We skip updating the service account because the token controller updates the service account with a
		// secret and we don't want to overwrite it with an empty secret.
		return err
	}
	return nil
}

func (r *ServiceAccountReconciler) ensureServiceAccountSecret(ctx context.Context, pSvcAccount vmwarev1.ProviderServiceAccount) error {
	log := ctrl.LoggerFrom(ctx)

	secret := &corev1.Secret{
		Type: corev1.SecretTypeServiceAccountToken,
		ObjectMeta: metav1.ObjectMeta{
			Name:      getServiceAccountSecretName(pSvcAccount),
			Namespace: pSvcAccount.Namespace,
			Annotations: map[string]string{
				// denotes that this secret holds the token for the service account
				corev1.ServiceAccountNameKey: getServiceAccountName(pSvcAccount),
			},
		},
	}
	log = log.WithValues("Secret", klog.KObj(secret))
	ctx = ctrl.LoggerInto(ctx, log)

	err := util.SetControllerReferenceWithOverride(&pSvcAccount, secret, r.Client.Scheme())
	if err != nil {
		return err
	}
	log.V(4).Info("Creating ServiceAccount Secret")
	err = r.Client.Create(ctx, secret)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		// Note: We skip updating the service account because the token controller updates the service account with a
		// secret and we don't want to overwrite it with an empty secret.
		return err
	}
	return nil
}

func (r *ServiceAccountReconciler) ensureRole(ctx context.Context, pSvcAccount vmwarev1.ProviderServiceAccount) error {
	log := ctrl.LoggerFrom(ctx)

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      getRoleName(pSvcAccount),
			Namespace: pSvcAccount.Namespace,
		},
	}
	log = log.WithValues("Role", klog.KObj(role))
	ctx = ctrl.LoggerInto(ctx, log)

	log.V(4).Info("Creating or updating Role")
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, role, func() error {
		if err := util.SetControllerReferenceWithOverride(&pSvcAccount, role, r.Client.Scheme()); err != nil {
			return err
		}
		role.Rules = pSvcAccount.Spec.Rules
		return nil
	})
	return err
}

func (r *ServiceAccountReconciler) ensureRoleBinding(ctx context.Context, pSvcAccount vmwarev1.ProviderServiceAccount) error {
	log := ctrl.LoggerFrom(ctx)

	roleName := getRoleName(pSvcAccount)
	svcAccountName := getServiceAccountName(pSvcAccount)
	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      getRoleBindingName(pSvcAccount),
			Namespace: pSvcAccount.Namespace,
		},
	}
	log = log.WithValues("RoleBinding", klog.KObj(roleBinding))
	ctx = ctrl.LoggerInto(ctx, log)
	log.V(4).Info("Creating or updating RoleBinding")

	err := r.Client.Get(ctx, types.NamespacedName{Name: getRoleBindingName(pSvcAccount), Namespace: pSvcAccount.Namespace}, roleBinding)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if err == nil {
		// If the roleRef needs changing, we have to delete the rolebinding and recreate it.
		if roleBinding.RoleRef.Name != roleName || roleBinding.RoleRef.Kind != "Role" || roleBinding.RoleRef.APIGroup != rbacv1.GroupName {
			if err := r.Client.Delete(ctx, roleBinding); err != nil {
				return err
			}
		}
	}

	_, err = controllerutil.CreateOrPatch(ctx, r.Client, roleBinding, func() error {
		if err := util.SetControllerReferenceWithOverride(&pSvcAccount, roleBinding, r.Client.Scheme()); err != nil {
			return err
		}
		roleBinding.RoleRef = rbacv1.RoleRef{
			Name:     roleName,
			Kind:     "Role",
			APIGroup: rbacv1.GroupName,
		}
		roleBinding.Subjects = []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				APIGroup:  "",
				Name:      svcAccountName,
				Namespace: pSvcAccount.Namespace,
			},
		}
		return nil
	})
	return err
}

func (r *ServiceAccountReconciler) syncServiceAccountSecret(ctx context.Context, guestClusterCtx *vmwarecontext.GuestClusterContext, pSvcAccount vmwarev1.ProviderServiceAccount) error {
	log := ctrl.LoggerFrom(ctx)
	log.V(4).Info("Attempting to sync token secret for ProviderServiceAccount")

	secretName := getServiceAccountSecretName(pSvcAccount)
	log.V(4).Info("Fetching secret for service account token details", "Secret", klog.KRef(pSvcAccount.Namespace, secretName))
	var svcAccountTokenSecret corev1.Secret
	err := r.Client.Get(ctx, types.NamespacedName{Name: secretName, Namespace: pSvcAccount.Namespace}, &svcAccountTokenSecret)
	if err != nil {
		return err
	}
	// Check if token data exists
	if len(svcAccountTokenSecret.Data) == 0 {
		// Note: We don't have to requeue here because we have a watch on the secret and the cluster should be reconciled
		// when a secret has token data populated.
		log.Info("Skipping sync secret for provider service account: secret has no data", "Secret", klog.KRef(pSvcAccount.Namespace, secretName))
		return nil
	}

	// Create the target namespace if it is not existing
	targetNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: pSvcAccount.Spec.TargetNamespace,
		},
	}

	if err = guestClusterCtx.GuestClient.Get(ctx, client.ObjectKey{Name: pSvcAccount.Spec.TargetNamespace}, targetNamespace); err != nil {
		if apierrors.IsNotFound(err) {
			err = guestClusterCtx.GuestClient.Create(ctx, targetNamespace)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	targetSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pSvcAccount.Spec.TargetSecretName,
			Namespace: pSvcAccount.Spec.TargetNamespace,
		},
	}
	log.V(4).Info("Creating or updating Secret in cluster", "Secret", klog.KObj(targetSecret))
	_, err = controllerutil.CreateOrPatch(ctx, guestClusterCtx.GuestClient, targetSecret, func() error {
		targetSecret.Data = svcAccountTokenSecret.Data
		return nil
	})
	return err
}

func (r *ServiceAccountReconciler) getConfigMapAndBuffer(ctx context.Context) (*corev1.ConfigMap, *corev1.ConfigMap, error) {
	configMap := &corev1.ConfigMap{}

	if err := r.Client.Get(ctx, GetCMNamespaceName(), configMap); err != nil {
		return nil, nil, err
	}

	configMapBuffer := &corev1.ConfigMap{}
	configMapBuffer.Name = configMap.Name
	configMapBuffer.Namespace = configMap.Namespace
	return configMapBuffer, configMap, nil
}

func (r *ServiceAccountReconciler) deleteServiceAccountConfigMap(ctx context.Context, svcAccount vmwarev1.ProviderServiceAccount) error {
	log := ctrl.LoggerFrom(ctx)

	svcAccountName := getSystemServiceAccountFullName(svcAccount)
	configMapBuffer, configMap, err := r.getConfigMapAndBuffer(ctx)
	if err != nil {
		return err
	}
	if valid, exist := configMap.Data[svcAccountName]; !exist || valid != strconv.FormatBool(true) {
		// Service account name is not in the config map
		return nil
	}
	log.Info("Deleting config map entry for provider service account")
	_, err = controllerutil.CreateOrPatch(ctx, r.Client, configMapBuffer, func() error {
		configMapBuffer.Data = configMap.Data
		delete(configMapBuffer.Data, svcAccountName)
		return nil
	})
	return err
}

func (r *ServiceAccountReconciler) ensureServiceAccountConfigMap(ctx context.Context, svcAccount vmwarev1.ProviderServiceAccount) error {
	log := ctrl.LoggerFrom(ctx)

	svcAccountName := getSystemServiceAccountFullName(svcAccount)
	configMapBuffer, configMap, err := r.getConfigMapAndBuffer(ctx)
	if err != nil {
		return err
	}
	if valid, exist := configMap.Data[svcAccountName]; exist && valid == strconv.FormatBool(true) {
		// Service account name is already in the config map
		return nil
	}
	log.Info("Updating config map for provider service account")
	_, err = controllerutil.CreateOrPatch(ctx, r.Client, configMapBuffer, func() error {
		configMapBuffer.Data = configMap.Data
		configMapBuffer.Data[svcAccountName] = "true"
		return nil
	})
	return err
}

func (r *ServiceAccountReconciler) getProviderServiceAccounts(ctx context.Context, clusterCtx *vmwarecontext.ClusterContext) ([]vmwarev1.ProviderServiceAccount, error) {
	var pSvcAccounts []vmwarev1.ProviderServiceAccount

	pSvcAccountList := vmwarev1.ProviderServiceAccountList{}
	if err := r.Client.List(ctx, &pSvcAccountList, client.InNamespace(clusterCtx.VSphereCluster.Namespace)); err != nil {
		return nil, err
	}

	for _, pSvcAccount := range pSvcAccountList.Items {
		// step to clean up the target secret in the guest cluster. Note: when the provider service account is deleted
		// all the associated serviceaccounts are deleted. Hence, the bearer token in the target
		// secret will be rendered invalid. Still, it's a good practice to delete the secret that we created.
		if pSvcAccount.DeletionTimestamp != nil {
			continue
		}
		ref := pSvcAccount.Spec.Ref
		if ref != nil && ref.Name == clusterCtx.VSphereCluster.Name {
			pSvcAccounts = append(pSvcAccounts, pSvcAccount)
		}
	}
	return pSvcAccounts, nil
}

func getRoleName(pSvcAccount vmwarev1.ProviderServiceAccount) string {
	return pSvcAccount.Name
}

func getRoleBindingName(pSvcAccount vmwarev1.ProviderServiceAccount) string {
	return pSvcAccount.Name
}

func getServiceAccountName(pSvcAccount vmwarev1.ProviderServiceAccount) string {
	return pSvcAccount.Name
}

func getServiceAccountSecretName(pSvcAccount vmwarev1.ProviderServiceAccount) string {
	return fmt.Sprintf("%s-secret", pSvcAccount.Name)
}

func getSystemServiceAccountFullName(pSvcAccount vmwarev1.ProviderServiceAccount) string {
	return fmt.Sprintf("%s.%s.%s", systemServiceAccountPrefix, getServiceAccountNamespace(pSvcAccount), getServiceAccountName(pSvcAccount))
}

func getServiceAccountNamespace(pSvcAccount vmwarev1.ProviderServiceAccount) string {
	return pSvcAccount.Namespace
}

// GetCMNamespaceName gets capi valid modifier configmap metadata.
func GetCMNamespaceName() types.NamespacedName {
	return types.NamespacedName{
		Namespace: os.Getenv("SERVICE_ACCOUNTS_CM_NAMESPACE"),
		Name:      os.Getenv("SERVICE_ACCOUNTS_CM_NAME"),
	}
}

// secretToVSphereCluster is a mapper function used to enqueue reconcile.Request objects.
// It accepts a Secret object owned by the controller and fetches the service account
// that contains the token and creates a reconcile.Request for the vmwarev1.VSphereCluster object.
func (r *ServiceAccountReconciler) secretToVSphereCluster(ctx context.Context, o client.Object) []reconcile.Request {
	secret, ok := o.(*corev1.Secret)
	if !ok {
		return nil
	}

	ownerRef := metav1.GetControllerOf(secret)
	if ownerRef != nil && ownerRef.Kind == kindProviderServiceAccount {
		if !metav1.HasAnnotation(secret.ObjectMeta, corev1.ServiceAccountNameKey) {
			return nil
		}
		svcAccountName := secret.GetAnnotations()[corev1.ServiceAccountNameKey]
		svcAccount := &corev1.ServiceAccount{}
		if err := r.Client.Get(ctx, client.ObjectKey{
			Namespace: secret.Namespace,
			Name:      svcAccountName,
		}, svcAccount); err != nil {
			return nil
		}
		return r.serviceAccountToVSphereCluster(ctx, svcAccount)
	}
	return nil
}

// serviceAccountToVSphereCluster is a mapper function used to enqueue reconcile.Request objects.
// From the watched object owned by this controller, it creates reconcile.Request object
// for the vmwarev1.VSphereCluster object that owns the watched object.
func (r *ServiceAccountReconciler) serviceAccountToVSphereCluster(ctx context.Context, o client.Object) []reconcile.Request {
	// We do this because this controller is effectively a vSphereCluster controller that reconciles its
	// dependent ProviderServiceAccount objects.
	ownerRef := metav1.GetControllerOf(o)
	if ownerRef != nil && ownerRef.Kind == kindProviderServiceAccount {
		key := types.NamespacedName{Namespace: o.GetNamespace(), Name: ownerRef.Name}
		pSvcAccount := &vmwarev1.ProviderServiceAccount{}
		if err := r.Client.Get(ctx, key, pSvcAccount); err != nil {
			return nil
		}
		return toVSphereClusterRequest(pSvcAccount)
	}
	return nil
}

// providerServiceAccountToVSphereCluster is a mapper function used to enqueue reconcile.Request objects.
func (r *ServiceAccountReconciler) providerServiceAccountToVSphereCluster(_ context.Context, o client.Object) []reconcile.Request {
	providerServiceAccount, ok := o.(*vmwarev1.ProviderServiceAccount)
	if !ok {
		return nil
	}

	return toVSphereClusterRequest(providerServiceAccount)
}

func toVSphereClusterRequest(providerServiceAccount *vmwarev1.ProviderServiceAccount) []reconcile.Request {
	vsphereClusterRef := providerServiceAccount.Spec.Ref
	if vsphereClusterRef == nil || vsphereClusterRef.Name == "" {
		return nil
	}
	return []reconcile.Request{
		{NamespacedName: client.ObjectKey{Namespace: providerServiceAccount.Namespace, Name: vsphereClusterRef.Name}},
	}
}
