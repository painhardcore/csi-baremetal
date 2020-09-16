package node

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"
	k8sError "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	api "github.com/dell/csi-baremetal/api/generated/v1"
	apiV1 "github.com/dell/csi-baremetal/api/v1"
	"github.com/dell/csi-baremetal/api/v1/lvgcrd"
	"github.com/dell/csi-baremetal/api/v1/volumecrd"
	"github.com/dell/csi-baremetal/pkg/base"
	"github.com/dell/csi-baremetal/pkg/base/command"
	"github.com/dell/csi-baremetal/pkg/base/k8s"
	"github.com/dell/csi-baremetal/pkg/base/util"
	p "github.com/dell/csi-baremetal/pkg/node/provisioners"
)

// VolumeController is the struct to perform volume operations on node side with real storage devices
type VolumeController struct {
	// for interacting with kubernetes objects
	k8sClient *k8s.KubeClient

	// holds implementations of Provisioner interface
	provisioners map[p.VolumeType]p.Provisioner

	// kubernetes node ID
	nodeID string
	// general logger
	log *logrus.Entry
}

const (
	// VolumeOperationsTimeout is the timeout for local Volume creation/deletion
	VolumeOperationsTimeout = 900 * time.Second
	// amount of reconcile requests that could be processed simultaneously
	maxConcurrentReconciles = 15
)

// NewVolumeController is the constructor for VolumeController struct
// Receives an instance of DriveServiceClient to interact with DriveManager, CmdExecutor to execute linux commands,
// logrus logger, base.KubeClient and ID of a node where VolumeController works
// Returns an instance of VolumeController
func NewVolumeController(e command.CmdExecutor, l *logrus.Logger, k8sclient *k8s.KubeClient, nodeID string) *VolumeController {
	vm := &VolumeController{
		k8sClient: k8sclient,
		provisioners: map[p.VolumeType]p.Provisioner{
			p.DriveBasedVolumeType: p.NewDriveProvisioner(e, k8sclient, l),
			p.LVMBasedVolumeType:   p.NewLVMProvisioner(e, k8sclient, l),
		},
		nodeID: nodeID,
		log:    l.WithField("component", "VolumeController"),
	}
	return vm
}

// SetProvisioners sets provisioners for current VolumeController instance
// uses for UTs and Sanity tests purposes
func (vc *VolumeController) SetProvisioners(provs map[p.VolumeType]p.Provisioner) {
	vc.provisioners = provs
}

// Reconcile is the main Reconcile loop of VolumeController. This loop handles creation of volumes matched to Volume CR on
// VolumeManagers's node if Volume.Spec.CSIStatus is Creating. Also this loop handles volume deletion on the node if
// Volume.Spec.CSIStatus is Removing.
// Returns reconcile result as ctrl.Result or error if something went wrong
func (vc *VolumeController) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx, cancelFn := context.WithTimeout(
		context.WithValue(context.Background(), k8s.RequestUUID, req.Name),
		VolumeOperationsTimeout)
	defer cancelFn()

	ll := vc.log.WithFields(logrus.Fields{
		"method":   "Reconcile",
		"volumeID": req.Name,
	})

	volume := &volumecrd.Volume{}

	err := vc.k8sClient.ReadCR(ctx, req.Name, volume)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ll.Infof("Processing for status %s", volume.Spec.CSIStatus)
	switch volume.Spec.CSIStatus {
	case apiV1.Creating:
		if util.IsStorageClassLVG(volume.Spec.StorageClass) {
			return vc.handleCreatingVolumeInLVG(ctx, volume)
		}
		return vc.prepareVolume(ctx, volume)
	case apiV1.Removing:
		return vc.handleRemovingStatus(ctx, volume)
	default:
		return ctrl.Result{}, nil
	}
}

// handleCreatingVolumeInLVG handles volume CR that has storage class related to LVG and CSIStatus creating
// check whether underlying LVG ready or not, add volume to LVG volumeRefs (if needed) and create real storage based on volume
// uses as a step for Reconcile for Volume CR
func (vc *VolumeController) handleCreatingVolumeInLVG(ctx context.Context, volume *volumecrd.Volume) (ctrl.Result, error) {
	ll := vc.log.WithFields(logrus.Fields{
		"method":   "handleCreatingVolumeInLVG",
		"volumeID": volume.Spec.Id,
	})

	var (
		lvg = &lvgcrd.LVG{}
		err error
	)

	if err = vc.k8sClient.ReadCR(ctx, volume.Spec.Location, lvg); err != nil {
		ll.Errorf("Unable to read underlying LVG %s: %v", volume.Spec.Location, err)
		if k8sError.IsNotFound(err) {
			volume.Spec.CSIStatus = apiV1.Failed
			err = vc.k8sClient.UpdateCR(ctx, volume)
			if err == nil {
				return ctrl.Result{}, nil // no need to retry
			}
			ll.Errorf("Unable to update volume CR and set status to failed: %v", err)
		}
		// retry because of LVG wasn't read or Volume status wasn't updated
		return ctrl.Result{Requeue: true, RequeueAfter: base.DefaultRequeueForVolume}, err
	}

	switch lvg.Spec.Status {
	case apiV1.Creating:
		ll.Debugf("Underlying LVG %s is still being created", lvg.Name)
		return ctrl.Result{Requeue: true, RequeueAfter: base.DefaultRequeueForVolume}, nil
	case apiV1.Failed:
		ll.Errorf("Underlying LVG %s has reached failed status. Unable to create volume on failed lvg.", lvg.Name)
		volume.Spec.CSIStatus = apiV1.Failed
		if err = vc.k8sClient.UpdateCR(ctx, volume); err != nil {
			ll.Errorf("Unable to update volume CR and set status to failed: %v", err)
			// retry because of volume status wasn't updated
			return ctrl.Result{Requeue: true, RequeueAfter: base.DefaultRequeueForVolume}, err
		}
		return ctrl.Result{}, nil // no need to retry
	case apiV1.Created:
		// add volume ID to LVG.Spec.VolumeRefs
		if !util.ContainsString(lvg.Spec.VolumeRefs, volume.Spec.Id) {
			lvg.Spec.VolumeRefs = append(lvg.Spec.VolumeRefs, volume.Spec.Id)
			if err = vc.k8sClient.UpdateCR(ctx, lvg); err != nil {
				ll.Errorf("Unable to add Volume ID to LVG %s volume refs: %v", lvg.Name, err)
				return ctrl.Result{Requeue: true}, err
			}
		}
		return vc.prepareVolume(ctx, volume)
	default:
		ll.Warnf("Unable to recognize LVG status. LVG - %v", lvg)
		return ctrl.Result{Requeue: true, RequeueAfter: base.DefaultRequeueForVolume}, nil
	}
}

// prepareVolume prepares real storage based on provided volume and update corresponding volume CR's CSIStatus
// uses as a step for Reconcile for Volume CR
func (vc *VolumeController) prepareVolume(ctx context.Context, volume *volumecrd.Volume) (ctrl.Result, error) {
	ll := vc.log.WithFields(logrus.Fields{
		"method":   "prepareVolume",
		"volumeID": volume.Spec.Id,
	})

	newStatus := apiV1.Created

	err := vc.getProvisionerForVolume(&volume.Spec).PrepareVolume(volume.Spec)
	if err != nil {
		ll.Errorf("Unable to create volume size of %d bytes: %v. Set volume status to Failed", volume.Spec.Size, err)
		newStatus = apiV1.Failed
	}

	volume.Spec.CSIStatus = newStatus
	if updateErr := vc.k8sClient.UpdateCRWithAttempts(ctx, volume, 5); updateErr != nil {
		ll.Errorf("Unable to update volume status to %s: %v", newStatus, updateErr)
		return ctrl.Result{Requeue: true}, updateErr
	}

	return ctrl.Result{}, err
}

// handleRemovingStatus handles volume CR with removing CSIStatus - removed real storage (partition/lv) and
// update corresponding volume CR's CSIStatus
// uses as a step for Reconcile for Volume CR
func (vc *VolumeController) handleRemovingStatus(ctx context.Context, volume *volumecrd.Volume) (ctrl.Result, error) {
	ll := vc.log.WithFields(logrus.Fields{
		"method":   "handleRemovingStatus",
		"volumeID": volume.Name,
	})

	var (
		err       error
		newStatus string
	)

	if err = vc.getProvisionerForVolume(&volume.Spec).ReleaseVolume(volume.Spec); err != nil {
		ll.Errorf("Failed to remove volume - %s. Error: %v. Set status to Failed", volume.Spec.Id, err)
		newStatus = apiV1.Failed
	} else {
		ll.Infof("Volume - %s was successfully removed. Set status to Removed", volume.Spec.Id)
		newStatus = apiV1.Removed
	}

	volume.Spec.CSIStatus = newStatus
	if updateErr := vc.k8sClient.UpdateCRWithAttempts(ctx, volume, 10); updateErr != nil {
		ll.Error("Unable to set new status for volume")
		return ctrl.Result{Requeue: true}, updateErr
	}
	return ctrl.Result{}, err
}

// SetupWithManager registers VolumeController to ControllerManager
func (vc *VolumeController) SetupWithManager(mgr ctrl.Manager) error {
	vc.log.WithField("method", "SetupWithManager").
		Infof("MaxConcurrentReconciles - %d", maxConcurrentReconciles)
	return ctrl.NewControllerManagedBy(mgr).
		For(&volumecrd.Volume{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: maxConcurrentReconciles,
		}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return vc.isCorrespondedToNodePredicate(e.Object)
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return vc.isCorrespondedToNodePredicate(e.Object)
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				return vc.isCorrespondedToNodePredicate(e.ObjectOld)
			},
			GenericFunc: func(e event.GenericEvent) bool {
				return vc.isCorrespondedToNodePredicate(e.Object)
			},
		}).
		Complete(vc)
}

// isCorrespondedToNodePredicate checks is a provided obj is aVolume CR object
// and that volume's node is and current manager node
func (vc *VolumeController) isCorrespondedToNodePredicate(obj runtime.Object) bool {
	if vol, ok := obj.(*volumecrd.Volume); ok {
		if vol.Spec.NodeId == vc.nodeID {
			return true
		}
	}

	return false
}

// getProvisionerForVolume returns appropriate Provisioner implementation for volume
func (vc *VolumeController) getProvisionerForVolume(vol *api.Volume) p.Provisioner {
	if util.IsStorageClassLVG(vol.StorageClass) {
		return vc.provisioners[p.LVMBasedVolumeType]
	}

	return vc.provisioners[p.DriveBasedVolumeType]
}
