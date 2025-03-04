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

package controllers

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apitypes "k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/remote"
	ipamv1 "sigs.k8s.io/cluster-api/exp/ipam/api/v1alpha1"
	clusterutilv1 "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbldr "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/feature"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/clustermodule"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/identity"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/record"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services/govmomi"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
)

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=vspherevms,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=vspherevms/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinedeployments;machinesets,verbs=get;list;watch
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=kubeadmcontrolplanes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;delete

// AddVMControllerToManager adds the VM controller to the provided manager.
func AddVMControllerToManager(ctx context.Context, controllerManagerCtx *capvcontext.ControllerManagerContext, mgr manager.Manager, tracker *remote.ClusterCacheTracker, options controller.Options) error {
	var (
		controlledType     = &infrav1.VSphereVM{}
		controlledTypeName = reflect.TypeOf(controlledType).Elem().Name()
		controlledTypeGVK  = infrav1.GroupVersion.WithKind(controlledTypeName)

		controllerNameShort = fmt.Sprintf("%s-controller", strings.ToLower(controlledTypeName))
		controllerNameLong  = fmt.Sprintf("%s/%s/%s", controllerManagerCtx.Namespace, controllerManagerCtx.Name, controllerNameShort)
	)

	// Build the controller context.
	controllerContext := &capvcontext.ControllerContext{
		ControllerManagerContext: controllerManagerCtx,
		Name:                     controllerNameShort,
		Recorder:                 record.New(mgr.GetEventRecorderFor(controllerNameLong)),
		Logger:                   controllerManagerCtx.Logger.WithName(controllerNameShort),
	}
	r := vmReconciler{
		ControllerContext:         controllerContext,
		VMService:                 &govmomi.VMService{},
		remoteClusterCacheTracker: tracker,
	}

	return ctrl.NewControllerManagedBy(mgr).
		// Watch the controlled, infrastructure resource.
		For(controlledType).
		WithOptions(options).
		// Watch a GenericEvent channel for the controlled resource.
		//
		// This is useful when there are events outside of Kubernetes that
		// should cause a resource to be synchronized, such as a goroutine
		// waiting on some asynchronous, external task to complete.
		WatchesRawSource(
			&source.Channel{Source: controllerManagerCtx.GetGenericEventChannelFor(controlledTypeGVK)},
			&handler.EnqueueRequestForObject{},
		).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), controllerManagerCtx.WatchFilterValue)).
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.clusterToVSphereVMs),
			ctrlbldr.WithPredicates(
				predicate.Funcs{
					UpdateFunc: func(e event.UpdateEvent) bool {
						newCluster := e.ObjectNew.(*clusterv1.Cluster)
						// check whether cluster has either spec.paused or pasued annotation
						return !annotations.IsPaused(newCluster, newCluster)
					},
					CreateFunc: func(e event.CreateEvent) bool {
						cluster := e.Object.(*clusterv1.Cluster)
						// check whether cluster has either spec.paused or pasued annotation
						return annotations.IsPaused(cluster, cluster)
					},
				}),
		).
		Watches(
			&infrav1.VSphereCluster{},
			handler.EnqueueRequestsFromMapFunc(r.vsphereClusterToVSphereVMs),
			ctrlbldr.WithPredicates(
				predicate.Funcs{
					UpdateFunc: func(e event.UpdateEvent) bool {
						oldCluster := e.ObjectOld.(*infrav1.VSphereCluster)
						newCluster := e.ObjectNew.(*infrav1.VSphereCluster)
						return !clustermodule.Compare(oldCluster.Spec.ClusterModules, newCluster.Spec.ClusterModules)
					},
					CreateFunc:  func(e event.CreateEvent) bool { return false },
					DeleteFunc:  func(e event.DeleteEvent) bool { return false },
					GenericFunc: func(e event.GenericEvent) bool { return false },
				}),
		).
		Watches(
			&ipamv1.IPAddressClaim{},
			handler.EnqueueRequestsFromMapFunc(r.ipAddressClaimToVSphereVM),
		).
		Complete(r)
}

type vmReconciler struct {
	*capvcontext.ControllerContext

	VMService                 services.VirtualMachineService
	remoteClusterCacheTracker *remote.ClusterCacheTracker
}

// Reconcile ensures the back-end state reflects the Kubernetes resource state intent.
func (r vmReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	r.Logger.V(4).Info("Starting Reconcile", "key", req.NamespacedName)
	// Get the VSphereVM resource for this request.
	vsphereVM := &infrav1.VSphereVM{}
	if err := r.Client.Get(ctx, req.NamespacedName, vsphereVM); err != nil {
		if apierrors.IsNotFound(err) {
			r.Logger.Info("VSphereVM not found, won't reconcile", "key", req.NamespacedName)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// Create the patch helper.
	patchHelper, err := patch.NewHelper(vsphereVM, r.Client)
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(
			err,
			"failed to init patch helper for %s %s/%s",
			vsphereVM.GroupVersionKind(),
			vsphereVM.Namespace,
			vsphereVM.Name)
	}

	authSession, err := r.retrieveVcenterSession(ctx, vsphereVM)
	if err != nil {
		conditions.MarkFalse(vsphereVM, infrav1.VCenterAvailableCondition, infrav1.VCenterUnreachableReason, clusterv1.ConditionSeverityError, err.Error())
		return reconcile.Result{}, err
	}
	conditions.MarkTrue(vsphereVM, infrav1.VCenterAvailableCondition)

	// Fetch the owner VSphereMachine.
	vsphereMachine, err := util.GetOwnerVSphereMachine(ctx, r.Client, vsphereVM.ObjectMeta)
	// vsphereMachine can be nil in cases where custom mover other than clusterctl
	// moves the resources without ownerreferences set
	// in that case nil vsphereMachine can cause panic and CrashLoopBackOff the pod
	// preventing vspheremachine_controller from setting the ownerref
	if err != nil || vsphereMachine == nil {
		r.Logger.Info("Owner VSphereMachine not found, won't reconcile", "key", req.NamespacedName)
		return reconcile.Result{}, nil
	}

	vsphereCluster, err := util.GetVSphereClusterFromVSphereMachine(ctx, r.Client, vsphereMachine)
	if err != nil || vsphereCluster == nil {
		r.Logger.Info("VSphereCluster not found, won't reconcile", "key", ctrlclient.ObjectKeyFromObject(vsphereMachine))
		return reconcile.Result{}, nil
	}

	// Fetch the CAPI Machine.
	machine, err := clusterutilv1.GetOwnerMachine(ctx, r.Client, vsphereMachine.ObjectMeta)
	if err != nil {
		return reconcile.Result{}, err
	}
	if machine == nil {
		r.Logger.Info("Waiting for OwnerRef to be set on VSphereMachine", "key", vsphereMachine.Name)
		return reconcile.Result{}, nil
	}

	var vsphereFailureDomain *infrav1.VSphereFailureDomain
	if failureDomain := machine.Spec.FailureDomain; failureDomain != nil {
		vsphereDeploymentZone := &infrav1.VSphereDeploymentZone{}
		if err := r.Client.Get(ctx, apitypes.NamespacedName{Name: *failureDomain}, vsphereDeploymentZone); err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to find vsphere deployment zone %s", *failureDomain)
		}

		vsphereFailureDomain = &infrav1.VSphereFailureDomain{}
		if err := r.Client.Get(ctx, apitypes.NamespacedName{Name: vsphereDeploymentZone.Spec.FailureDomain}, vsphereFailureDomain); err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to find vsphere failure domain %s", vsphereDeploymentZone.Spec.FailureDomain)
		}
	}

	// Create the VM context for this request.
	vmContext := &capvcontext.VMContext{
		ControllerContext:    r.ControllerContext,
		VSphereVM:            vsphereVM,
		VSphereFailureDomain: vsphereFailureDomain,
		Session:              authSession,
		Logger:               r.Logger.WithName(req.Namespace).WithName(req.Name),
		PatchHelper:          patchHelper,
	}

	// Print the task-ref upon entry and upon exit.
	vmContext.Logger.V(4).Info(
		"VSphereVM.Status.TaskRef OnEntry",
		"task-ref", vmContext.VSphereVM.Status.TaskRef)
	defer func() {
		vmContext.Logger.V(4).Info(
			"VSphereVM.Status.TaskRef OnExit",
			"task-ref", vmContext.VSphereVM.Status.TaskRef)
	}()

	// Always issue a patch when exiting this function so changes to the
	// resource are patched back to the API server.
	defer func() {
		// always update the readyCondition.
		conditions.SetSummary(vmContext.VSphereVM,
			conditions.WithConditions(
				infrav1.VCenterAvailableCondition,
				infrav1.IPAddressClaimedCondition,
				infrav1.VMProvisionedCondition,
			),
		)

		// Patch the VSphereVM resource.
		if err := vmContext.Patch(ctx); err != nil {
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	cluster, err := clusterutilv1.GetClusterFromMetadata(ctx, r.Client, vsphereVM.ObjectMeta)
	if err == nil {
		if annotations.IsPaused(cluster, vsphereVM) {
			r.Logger.V(4).Info("VSphereVM %s/%s linked to a cluster that is paused",
				vsphereVM.Namespace, vsphereVM.Name)
			return reconcile.Result{}, nil
		}
	}

	if vsphereVM.ObjectMeta.DeletionTimestamp.IsZero() {
		// If the VSphereVM doesn't have our finalizer, add it.
		// Requeue immediately to avoid the race condition between init and delete
		if !ctrlutil.ContainsFinalizer(vsphereVM, infrav1.VMFinalizer) {
			ctrlutil.AddFinalizer(vsphereVM, infrav1.VMFinalizer)
			return reconcile.Result{}, nil
		}
	}

	return r.reconcile(ctx, vmContext, fetchClusterModuleInput{
		VSphereCluster: vsphereCluster,
		Machine:        machine,
	})
}

// reconcile encases the behavior of the controller around cluster module information
// retrieval depending upon inputs passed.
//
// This logic was moved to a smaller function outside of the main Reconcile() loop
// for the ease of testing.
func (r vmReconciler) reconcile(ctx context.Context, vmCtx *capvcontext.VMContext, input fetchClusterModuleInput) (reconcile.Result, error) {
	if feature.Gates.Enabled(feature.NodeAntiAffinity) {
		clusterModuleInfo, err := r.fetchClusterModuleInfo(ctx, input)
		// If cluster module information cannot be fetched for a VM being deleted,
		// we should not block VM deletion since the cluster module is updated
		// once the VM gets removed.
		if err != nil && vmCtx.VSphereVM.ObjectMeta.DeletionTimestamp.IsZero() {
			return reconcile.Result{}, err
		}
		vmCtx.ClusterModuleInfo = clusterModuleInfo
	}

	// Handle deleted machines
	if !vmCtx.VSphereVM.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, vmCtx)
	}

	// Handle non-deleted machines
	return r.reconcileNormal(ctx, vmCtx)
}

func (r vmReconciler) reconcileDelete(ctx context.Context, vmCtx *capvcontext.VMContext) (reconcile.Result, error) {
	vmCtx.Logger.Info("Handling deleted VSphereVM")

	conditions.MarkFalse(vmCtx.VSphereVM, infrav1.VMProvisionedCondition, clusterv1.DeletingReason, clusterv1.ConditionSeverityInfo, "")
	result, vm, err := r.VMService.DestroyVM(ctx, vmCtx)
	if err != nil {
		conditions.MarkFalse(vmCtx.VSphereVM, infrav1.VMProvisionedCondition, "DeletionFailed", clusterv1.ConditionSeverityWarning, err.Error())
		return reconcile.Result{}, errors.Wrapf(err, "failed to destroy VM")
	}

	if !result.IsZero() {
		// a non-zero value means we need to requeue the request before proceed.
		return result, nil
	}

	// Requeue the operation until the VM is "notfound".
	if vm.State != infrav1.VirtualMachineStateNotFound {
		vmCtx.Logger.Info("vm state is not reconciled", "expected-vm-state", infrav1.VirtualMachineStateNotFound, "actual-vm-state", vm.State)
		return reconcile.Result{}, nil
	}

	// Attempt to delete the node corresponding to the vsphere VM
	result, err = r.deleteNode(ctx, vmCtx, vm.Name)
	if err != nil {
		r.Logger.V(6).Info("unable to delete node", "err", err)
	}
	if !result.IsZero() {
		// a non-zero value means we need to requeue the request before proceed.
		return result, nil
	}

	if err := r.deleteIPAddressClaims(ctx, vmCtx); err != nil {
		return reconcile.Result{}, err
	}

	// The VM is deleted so remove the finalizer.
	ctrlutil.RemoveFinalizer(vmCtx.VSphereVM, infrav1.VMFinalizer)

	return reconcile.Result{}, nil
}

// deleteNode attempts to find and best effort delete the node corresponding to the VM
// This is necessary since CAPI does not the nodeRef field on the owner Machine object
// until the node moves to Ready state. Hence, on Machine deletion it is unable to delete
// the kubernetes node corresponding to the VM.
func (r vmReconciler) deleteNode(ctx context.Context, vmCtx *capvcontext.VMContext, name string) (reconcile.Result, error) {
	// Fetching the cluster object from the VSphereVM object to create a remote client to the cluster
	cluster, err := clusterutilv1.GetClusterFromMetadata(ctx, r.Client, vmCtx.VSphereVM.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	clusterClient, err := r.remoteClusterCacheTracker.GetClient(ctx, ctrlclient.ObjectKeyFromObject(cluster))
	if err != nil {
		if errors.Is(err, remote.ErrClusterLocked) {
			r.Logger.V(5).Info("Requeuing because another worker has the lock on the ClusterCacheTracker")
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	// Attempt to delete the corresponding node
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	return ctrl.Result{}, clusterClient.Delete(ctx, node)
}

func (r vmReconciler) reconcileNormal(ctx context.Context, vmCtx *capvcontext.VMContext) (reconcile.Result, error) {
	if vmCtx.VSphereVM.Status.FailureReason != nil || vmCtx.VSphereVM.Status.FailureMessage != nil {
		r.Logger.Info("VM is failed, won't reconcile", "namespace", vmCtx.VSphereVM.Namespace, "name", vmCtx.VSphereVM.Name)
		return reconcile.Result{}, nil
	}

	if r.isWaitingForStaticIPAllocation(vmCtx) {
		conditions.MarkFalse(vmCtx.VSphereVM, infrav1.VMProvisionedCondition, infrav1.WaitingForStaticIPAllocationReason, clusterv1.ConditionSeverityInfo, "")
		vmCtx.Logger.Info("vm is waiting for static ip to be available")
		return reconcile.Result{}, nil
	}

	if err := r.reconcileIPAddressClaims(ctx, vmCtx); err != nil {
		return reconcile.Result{}, err
	}

	// Get or create the VM.
	vm, err := r.VMService.ReconcileVM(ctx, vmCtx)
	if err != nil {
		vmCtx.Logger.Error(err, "error reconciling VM")
		return reconcile.Result{}, errors.Wrapf(err, "failed to reconcile VM")
	}

	// Do not proceed until the backend VM is marked ready.
	if vm.State != infrav1.VirtualMachineStateReady {
		vmCtx.Logger.Info(
			"VM state is not reconciled",
			"expected-vm-state", infrav1.VirtualMachineStateReady,
			"actual-vm-state", vm.State)
		return reconcile.Result{}, nil
	}

	// Update the VSphereVM's BIOS UUID.
	vmCtx.Logger.Info("vm bios-uuid", "biosuuid", vm.BiosUUID)

	// defensive check to ensure we are not removing the biosUUID
	if vm.BiosUUID != "" {
		vmCtx.VSphereVM.Spec.BiosUUID = vm.BiosUUID
	} else {
		return reconcile.Result{}, errors.Errorf("bios uuid is empty while VM is ready")
	}

	// VMRef should be set just once. It is not supposed to change!
	if vm.VMRef != "" && vmCtx.VSphereVM.Status.VMRef == "" {
		vmCtx.VSphereVM.Status.VMRef = vm.VMRef
	}

	// Update the VSphereVM's network status.
	r.reconcileNetwork(vmCtx, vm)

	// we didn't get any addresses, requeue
	if len(vmCtx.VSphereVM.Status.Addresses) == 0 {
		conditions.MarkFalse(vmCtx.VSphereVM, infrav1.VMProvisionedCondition, infrav1.WaitingForIPAllocationReason, clusterv1.ConditionSeverityInfo, "")
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Once the network is online the VM is considered ready.
	vmCtx.VSphereVM.Status.Ready = true
	conditions.MarkTrue(vmCtx.VSphereVM, infrav1.VMProvisionedCondition)
	vmCtx.Logger.Info("VSphereVM is ready")
	return reconcile.Result{}, nil
}

// isWaitingForStaticIPAllocation checks whether the VM should wait for a static IP
// to be allocated.
// It checks the state of both DHCP4 and DHCP6 for all the network devices and if
// any static IP addresses or IPAM Pools are specified.
func (r vmReconciler) isWaitingForStaticIPAllocation(vmCtx *capvcontext.VMContext) bool {
	devices := vmCtx.VSphereVM.Spec.Network.Devices
	for _, dev := range devices {
		if !dev.DHCP4 && !dev.DHCP6 && len(dev.IPAddrs) == 0 && len(dev.AddressesFromPools) == 0 {
			// Static IP is not available yet
			return true
		}
	}

	return false
}

func (r vmReconciler) reconcileNetwork(vmCtx *capvcontext.VMContext, vm infrav1.VirtualMachine) {
	vmCtx.VSphereVM.Status.Network = vm.Network
	ipAddrs := make([]string, 0, len(vm.Network))
	for _, netStatus := range vmCtx.VSphereVM.Status.Network {
		ipAddrs = append(ipAddrs, netStatus.IPAddrs...)
	}
	vmCtx.VSphereVM.Status.Addresses = ipAddrs
}

func (r vmReconciler) clusterToVSphereVMs(ctx context.Context, a ctrlclient.Object) []reconcile.Request {
	requests := []reconcile.Request{}
	vms := &infrav1.VSphereVMList{}
	err := r.Client.List(ctx, vms, ctrlclient.MatchingLabels(
		map[string]string{
			clusterv1.ClusterNameLabel: a.GetName(),
		},
	))
	if err != nil {
		return requests
	}
	for _, vm := range vms.Items {
		r := reconcile.Request{
			NamespacedName: apitypes.NamespacedName{
				Name:      vm.Name,
				Namespace: vm.Namespace,
			},
		}
		requests = append(requests, r)
	}
	return requests
}

func (r vmReconciler) vsphereClusterToVSphereVMs(ctx context.Context, a ctrlclient.Object) []reconcile.Request {
	vsphereCluster, ok := a.(*infrav1.VSphereCluster)
	if !ok {
		return nil
	}
	clusterName, ok := vsphereCluster.Labels[clusterv1.ClusterNameLabel]
	if !ok {
		return nil
	}

	requests := []reconcile.Request{}
	vms := &infrav1.VSphereVMList{}
	err := r.Client.List(ctx, vms, ctrlclient.MatchingLabels(
		map[string]string{
			clusterv1.ClusterNameLabel: clusterName,
		},
	))
	if err != nil {
		return requests
	}
	for _, vm := range vms.Items {
		r := reconcile.Request{
			NamespacedName: apitypes.NamespacedName{
				Name:      vm.Name,
				Namespace: vm.Namespace,
			},
		}
		requests = append(requests, r)
	}
	return requests
}

func (r vmReconciler) ipAddressClaimToVSphereVM(_ context.Context, a ctrlclient.Object) []reconcile.Request {
	ipAddressClaim, ok := a.(*ipamv1.IPAddressClaim)
	if !ok {
		return nil
	}

	requests := []reconcile.Request{}
	if clusterutilv1.HasOwner(ipAddressClaim.OwnerReferences, infrav1.GroupVersion.String(), []string{"VSphereVM"}) {
		for _, ref := range ipAddressClaim.OwnerReferences {
			if ref.Kind == "VSphereVM" {
				requests = append(requests, reconcile.Request{
					NamespacedName: apitypes.NamespacedName{
						Name:      ref.Name,
						Namespace: ipAddressClaim.Namespace,
					},
				})
				break
			}
		}
	}
	return requests
}

func (r vmReconciler) retrieveVcenterSession(ctx context.Context, vsphereVM *infrav1.VSphereVM) (*session.Session, error) {
	// Get cluster object and then get VSphereCluster object

	params := session.NewParams().
		WithServer(vsphereVM.Spec.Server).
		WithDatacenter(vsphereVM.Spec.Datacenter).
		WithUserInfo(r.ControllerContext.Username, r.ControllerContext.Password).
		WithThumbprint(vsphereVM.Spec.Thumbprint).
		WithFeatures(session.Feature{
			EnableKeepAlive:   r.EnableKeepAlive,
			KeepAliveDuration: r.KeepAliveDuration,
		})
	cluster, err := clusterutilv1.GetClusterFromMetadata(ctx, r.Client, vsphereVM.ObjectMeta)
	if err != nil {
		r.Logger.Info("VsphereVM is missing cluster label or cluster does not exist")
		return session.GetOrCreate(ctx,
			params)
	}

	if cluster.Spec.InfrastructureRef == nil {
		return nil, errors.Errorf("cannot retrieve vCenter session for cluster %s: .spec.infrastructureRef is nil", klog.KObj(cluster))
	}
	key := ctrlclient.ObjectKey{
		Namespace: cluster.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	vsphereCluster := &infrav1.VSphereCluster{}
	err = r.Client.Get(ctx, key, vsphereCluster)
	if err != nil {
		r.Logger.Info("VSphereCluster couldn't be retrieved")
		return session.GetOrCreate(ctx,
			params)
	}

	if vsphereCluster.Spec.IdentityRef != nil {
		creds, err := identity.GetCredentials(ctx, r.Client, vsphereCluster, r.Namespace)
		if err != nil {
			return nil, errors.Wrap(err, "failed to retrieve credentials from IdentityRef")
		}
		params = params.WithUserInfo(creds.Username, creds.Password)
		return session.GetOrCreate(ctx,
			params)
	}

	// Fallback to using credentials provided to the manager
	return session.GetOrCreate(ctx,
		params)
}

func (r vmReconciler) fetchClusterModuleInfo(ctx context.Context, clusterModInput fetchClusterModuleInput) (*string, error) {
	var (
		owner ctrlclient.Object
		err   error
	)
	machine := clusterModInput.Machine
	logger := r.Logger.WithName(machine.Namespace).WithName(machine.Name)

	input := util.FetchObjectInput{
		Client: r.Client,
		Object: machine,
	}
	// TODO (srm09): Figure out a way to find the latest version of the CRD
	if util.IsControlPlaneMachine(machine) {
		owner, err = util.FetchControlPlaneOwnerObject(ctx, input)
	} else {
		owner, err = util.FetchMachineDeploymentOwnerObject(ctx, input)
	}
	if err != nil {
		// If the owner objects cannot be traced, we can assume that the objects
		// have been deleted in which case we do not want cluster module info populated
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	for _, mod := range clusterModInput.VSphereCluster.Spec.ClusterModules {
		if mod.TargetObjectName == owner.GetName() {
			logger.Info("cluster module with UUID found", "moduleUUID", mod.ModuleUUID)
			return pointer.String(mod.ModuleUUID), nil
		}
	}
	logger.V(4).Info("no cluster module found")
	return nil, nil
}

type fetchClusterModuleInput struct {
	VSphereCluster *infrav1.VSphereCluster
	Machine        *clusterv1.Machine
}
