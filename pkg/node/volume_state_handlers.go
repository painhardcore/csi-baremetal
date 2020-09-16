package node

import (
	"context"

	"github.com/sirupsen/logrus"
	k8sError "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"

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

type volumeStateHandler interface {
	handle(ctx context.Context, currState volumecrd.Volume) (newState volumecrd.Volume, err error)
}

type commonStateHandler struct {// holds implementations of Provisioner interface
	provisioners map[p.VolumeType]p.Provisioner

	k8sClient *k8s.KubeClient
	log       *logrus.Logger
}

func NewCommonStateHandler(e command.CmdExecutor, k8sClient *k8s.KubeClient, log *logrus.Logger) commonStateHandler {
	return commonStateHandler{
		k8sClient: k8sClient,
		log: 		log,
		provisioners: map[p.VolumeType]p.Provisioner{
			p.DriveBasedVolumeType: p.NewDriveProvisioner(e, k8sClient, log),
			p.LVMBasedVolumeType:   p.NewLVMProvisioner(e, k8sClient, log),
		},
	}
}

// getProvisioner returns appropriate Provisioner implementation for volume
func (csh *commonStateHandler) getProvisioner(vol *api.Volume) p.Provisioner {
	if util.IsStorageClassLVG(vol.StorageClass) {
		return csh.provisioners[p.LVMBasedVolumeType]
	}

	return csh.provisioners[p.DriveBasedVolumeType]
}

type creatingHandler struct {
	commonStateHandler
}

func (ch *creatingHandler) handle(ctx context.Context, currState volumecrd.Volume) (newState volumecrd.Volume, err error) {
	if util.IsStorageClassLVG(currState.Spec.StorageClass) {
		return ch.handleCreatingVolumeInLVG(ctx, volume)
	}
	return ch.prepareVolume(ctx, volume)
}

// handleCreatingVolumeInLVG handles volume CR that has storage class related to LVG and CSIStatus creating
// check whether underlying LVG ready or not, add volume to LVG volumeRefs (if needed) and create real storage based on volume
// uses as a step for Reconcile for Volume CR
func (ch *creatingHandler) handleCreatingVolumeInLVG(ctx context.Context, volume volumecrd.Volume) (volumecrd.Volume, error) {
	ll := ch.log.WithFields(logrus.Fields{
		"method":   "handleCreatingVolumeInLVG",
		"volumeID": volume.Spec.Id,
	})

	var (
		lvg = &lvgcrd.LVG{}
		err error
	)

	if err = ch.k8sClient.ReadCR(ctx, volume.Spec.Location, lvg); err != nil {
		ll.Errorf("Unable to read underlying LVG %s: %v", volume.Spec.Location, err)
		if k8sError.IsNotFound(err) {
			volume.Spec.CSIStatus = apiV1.Failed
			return volume, nil
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
		if err = ch.k8sClient.UpdateCR(ctx, volume); err != nil {
			ll.Errorf("Unable to update volume CR and set status to failed: %v", err)
			// retry because of volume status wasn't updated
			return ctrl.Result{Requeue: true, RequeueAfter: base.DefaultRequeueForVolume}, err
		}
		return ctrl.Result{}, nil // no need to retry
	case apiV1.Created:
		// add volume ID to LVG.Spec.VolumeRefs
		if !util.ContainsString(lvg.Spec.VolumeRefs, volume.Spec.Id) {
			lvg.Spec.VolumeRefs = append(lvg.Spec.VolumeRefs, volume.Spec.Id)
			if err = ch.k8sClient.UpdateCR(ctx, lvg); err != nil {
				ll.Errorf("Unable to add Volume ID to LVG %s volume refs: %v", lvg.Name, err)
				return ctrl.Result{Requeue: true}, err
			}
		}
		return ch.prepareVolume(ctx, volume)
	default:
		ll.Warnf("Unable to recognize LVG status. LVG - %v", lvg)
		return ctrl.Result{Requeue: true, RequeueAfter: base.DefaultRequeueForVolume}, nil
	}
}

// prepareVolume prepares real storage based on provided volume and update corresponding volume CR's CSIStatus
// uses as a step for Reconcile for Volume CR
func (ch *creatingHandler) prepareVolume(ctx context.Context, volume volumecrd.Volume) (ctrl.Result, error) {
	ll := ch.log.WithFields(logrus.Fields{
		"method":   "prepareVolume",
		"volumeID": volume.Spec.Id,
	})

	newStatus := apiV1.Created

	err := ch.getProvisioner(&volume.Spec).PrepareVolume(volume.Spec)
	if err != nil {
		ll.Errorf("Unable to create volume size of %d bytes: %v. Set volume status to Failed", volume.Spec.Size, err)
		newStatus = apiV1.Failed
	}

	volume.Spec.CSIStatus = newStatus
	if updateErr := ch.k8sClient.UpdateCRWithAttempts(ctx, volume, 5); updateErr != nil {
		ll.Errorf("Unable to update volume status to %s: %v", newStatus, updateErr)
		return ctrl.Result{Requeue: true}, updateErr
	}

	return ctrl.Result{}, err
}

type removingHandler struct {
	commonStateHandler
}

func (rh *removingHandler) handle(ctx context.Context, currState volumecrd.Volume) (newState volumecrd.Volume, err error) {
	return newState, nil
}