package node

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"

	api "github.com/dell/csi-baremetal/api/generated/v1"
	apiV1 "github.com/dell/csi-baremetal/api/v1"
	accrd "github.com/dell/csi-baremetal/api/v1/availablecapacitycrd"
	"github.com/dell/csi-baremetal/api/v1/drivecrd"
	"github.com/dell/csi-baremetal/api/v1/lvgcrd"
	"github.com/dell/csi-baremetal/pkg/base"
	"github.com/dell/csi-baremetal/pkg/base/command"
	"github.com/dell/csi-baremetal/pkg/base/k8s"
	"github.com/dell/csi-baremetal/pkg/base/linuxutils/lsblk"
	"github.com/dell/csi-baremetal/pkg/base/linuxutils/lvm"
	"github.com/dell/csi-baremetal/pkg/base/util"
	"github.com/dell/csi-baremetal/pkg/common"
	"github.com/dell/csi-baremetal/pkg/eventing"
	"github.com/dell/csi-baremetal/pkg/node/provisioners/utilwrappers"
)

const (
	// DiscoverDrivesTimeout is the timeout for Discover method
	DiscoverDrivesTimeout = 300 * time.Second
)

type VolumeDiscoverer struct {
	// for interacting with kubernetes objects
	k8sClient *k8s.KubeClient
	// help to read/update particular CR
	crHelper *k8s.CRHelper
	// uses for communicating with hardware manager
	driveMgrClient api.DriveServiceClient
	// sink where we write events
	recorder eventRecorder
	// kubernetes node ID
	nodeID string
	// used for discoverLVGOnSystemDisk method to determine if we need to discover LVG in Discover method, default true
	// set false when there is no LVG on system disk or system disk is not SSD
	discoverLvgSSD bool
	// general logger
	log *logrus.Entry

	// whether VolumeController was initialized or no, uses for health probes
	initialized bool // TODO: it is shouldn't be here
	// uses for running lsblk util
	listBlk lsblk.WrapLsblk
	// uses for FS operations such as Mount/Unmount, MkFS and so on
	fsOps utilwrappers.FSOperations
	// uses for LVM operations
	lvmOps lvm.WrapLVM
}

// eventRecorder interface for sending events
type eventRecorder interface {
	Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{})
}

// driveStates internal struct, holds info about drive updates
// not thread safe
type driveUpdates struct {
	Created    []*drivecrd.Drive
	NotChanged []*drivecrd.Drive
	Updated    []updatedDrive
}

func (du *driveUpdates) AddCreated(drive *drivecrd.Drive) {
	du.Created = append(du.Created, drive)
}

func (du *driveUpdates) AddNotChanged(drive *drivecrd.Drive) {
	du.NotChanged = append(du.NotChanged, drive)
}

func (du *driveUpdates) AddUpdated(previousState, currentState *drivecrd.Drive) {
	du.Updated = append(du.Updated, updatedDrive{
		PreviousState: previousState, CurrentState: currentState})
}

// updatedDrive holds previous and current state for updated drive
type updatedDrive struct {
	PreviousState *drivecrd.Drive
	CurrentState  *drivecrd.Drive
}

func NewVolumeDiscoverer(client api.DriveServiceClient,
	executor command.CmdExecutor,
	logger *logrus.Logger,
	k8sclient *k8s.KubeClient,
	recorder eventRecorder, nodeID string) *VolumeDiscoverer {
	vm := &VolumeDiscoverer{
		k8sClient:      k8sclient,
		crHelper:       k8s.NewCRHelper(k8sclient, logger),
		driveMgrClient: client,
		fsOps:          utilwrappers.NewFSOperationsImpl(executor, logger),
		lvmOps:         lvm.NewLVM(executor, logger),
		listBlk:        lsblk.NewLSBLK(logger),
		nodeID:         nodeID,
		log:            logger.WithField("component", "VolumeController"),
		recorder:       recorder,
		discoverLvgSSD: true,
	}
	return vm
}

// Discover inspects actual drives structs from DriveManager and create volume object if partition exist on some of them
// (in case of VolumeController restart). Updates Drives CRs based on gathered from DriveManager information.
// Also this method creates AC CRs. Performs at some intervals in a goroutine
// Returns error if something went wrong during discovering
func (vd *VolumeDiscoverer) Discover() error {
	ctx, cancelFn := context.WithTimeout(context.Background(), DiscoverDrivesTimeout)
	defer cancelFn()
	drivesResponse, err := vd.driveMgrClient.GetDrivesList(ctx, &api.DrivesRequest{NodeId: vd.nodeID})
	if err != nil {
		return err
	}

	updates := vd.updateDrivesCRs(ctx, drivesResponse.Disks)
	vd.handleDriveUpdates(ctx, updates)

	freeDrives := vd.drivesAreNotUsed()
	if err = vd.discoverVolumeCRs(freeDrives); err != nil {
		return err
	}

	if err = vd.discoverAvailableCapacity(ctx, vd.nodeID); err != nil {
		return err
	}

	if vd.discoverLvgSSD {
		if err = vd.discoverLVGOnSystemDrive(); err != nil {
			vd.log.WithField("method", "Discover").
				Errorf("unable to inspect system LVG: %v", err)
		}
	}
	vd.initialized = true
	return nil
}

// updateDrivesCRs updates Drives CRs based on provided list of Drives.
// Receives golang context and slice of discovered api.Drive structs usually got from DriveManager
// returns struct with information about drives updates
func (vd *VolumeDiscoverer) updateDrivesCRs(ctx context.Context, discoveredDrives []*api.Drive) *driveUpdates {
	ll := vd.log.WithFields(logrus.Fields{
		"component": "VolumeController",
		"method":    "updateDrivesCRs",
	})
	ll.Debugf("Processing")

	updates := &driveUpdates{}

	driveCRs := vd.crHelper.GetDriveCRs(vd.nodeID)

	// Try to find not existing CR for discovered drives
	for _, drivePtr := range discoveredDrives {
		exist := false
		for _, driveCR := range driveCRs {
			driveCR := driveCR
			// If drive CR already exist, try to update, if drive was changed
			if vd.drivesAreTheSame(drivePtr, &driveCR.Spec) {
				exist = true
				if driveCR.Equals(drivePtr) {
					updates.AddNotChanged(&driveCR)
				} else {
					previousState := driveCR.DeepCopy()
					drivePtr.UUID = driveCR.Spec.UUID
					toUpdate := driveCR
					toUpdate.Spec = *drivePtr
					if err := vd.k8sClient.UpdateCR(ctx, &toUpdate); err != nil {
						ll.Errorf("Failed to update drive CR (health/status) %v, error %v", toUpdate, err)
						updates.AddNotChanged(previousState)
					} else {
						updates.AddUpdated(previousState, &toUpdate)
					}
				}
				break
			}
		}
		if !exist {
			// Drive CR is not exist, try to create it
			toCreateSpec := *drivePtr
			toCreateSpec.NodeId = vd.nodeID
			toCreateSpec.UUID = uuid.New().String()
			driveCR := vd.k8sClient.ConstructDriveCR(toCreateSpec.UUID, toCreateSpec)
			if err := vd.k8sClient.CreateCR(ctx, driveCR.Name, driveCR); err != nil {
				ll.Errorf("Failed to create drive CR %v, error: %v", driveCR, err)
			}
			updates.AddCreated(driveCR)
		}
	}

	// that means that it is a first round and drives are discovered first time
	if len(driveCRs) == 0 {
		return updates
	}

	// Try to find missing drive in drive CRs and update according CR
	for _, d := range vd.crHelper.GetDriveCRs(vd.nodeID) {
		wasDiscovered := false
		for _, drive := range discoveredDrives {
			if vd.drivesAreTheSame(&d.Spec, drive) {
				wasDiscovered = true
				break
			}
		}
		isInLVG := false
		if !wasDiscovered {
			ll.Debugf("Check whether drive %v in LVG or no", d)
			isInLVG = vd.isDriveIsInLVG(d.Spec)
		}
		if !wasDiscovered && !isInLVG {
			// TODO: remove AC and aware Volumes here
			ll.Warnf("Set status OFFLINE for drive %v", d.Spec)
			previousState := d.DeepCopy()
			toUpdate := d
			toUpdate.Spec.Status = apiV1.DriveStatusOffline
			toUpdate.Spec.Health = apiV1.HealthUnknown
			err := vd.k8sClient.UpdateCR(ctx, &toUpdate)
			if err != nil {
				ll.Errorf("Failed to update drive CR %v, error %v", toUpdate, err)
				updates.AddNotChanged(previousState)
			} else {
				updates.AddUpdated(previousState, &toUpdate)
			}
		}
	}
	return updates
}

func (vd *VolumeDiscoverer) handleDriveUpdates(ctx context.Context, updates *driveUpdates) {
	for _, updDrive := range updates.Updated {
		vd.handleDriveStatusChange(ctx, &updDrive.CurrentState.Spec)
	}
	vd.createEventsForDriveUpdates(updates)
}

// isDriveIsInLVG check whether drive is a part of some LVG or no
func (vd *VolumeDiscoverer) isDriveIsInLVG(d api.Drive) bool {
	lvgs := vd.crHelper.GetLVGCRs(vd.nodeID)
	for _, lvg := range lvgs {
		if util.ContainsString(lvg.Spec.Locations, d.UUID) {
			return true
		}
	}
	return false
}

// discoverVolumeCRs updates volumes cache based on provided freeDrives.
// searches drives in freeDrives that are not have volume and if there are some partitions on them - try to read
// partition uuid and create volume object
func (vd *VolumeDiscoverer) discoverVolumeCRs(freeDrives []*drivecrd.Drive) error {
	ll := vd.log.WithFields(logrus.Fields{
		"method": "discoverVolumeCRs",
	})

	// explore each drive from freeDrives
	lsblk, err := vd.listBlk.GetBlockDevices("")
	if err != nil {
		return fmt.Errorf("unable to inspect system block devices via lsblk, error: %v", err)
	}

	for _, d := range freeDrives {
		for _, ld := range lsblk {
			if strings.EqualFold(ld.Serial, d.Spec.SerialNumber) && len(ld.Children) > 0 {
				if vd.isDriveIsInLVG(d.Spec) {
					ll.Debugf("Drive %v is in LVG and not a FREE", d.Spec)
					break
				}
				var (
					partUUID string
					size     int64
				)
				partUUID = ld.PartUUID
				if partUUID == "" {
					partUUID = uuid.New().String() // just generate random and exclude drive
					ll.Warnf("UUID generated %s", partUUID)
				}
				if ld.Size != "" {
					size, err = strconv.ParseInt(ld.Size, 10, 64)
					if err != nil {
						ll.Warnf("Unable parse string %s to int, for device %s, error: %v", ld.Size, ld.Name, err)
					}
				}

				volumeCR := vd.k8sClient.ConstructVolumeCR(partUUID, api.Volume{
					NodeId:       vd.nodeID,
					Id:           partUUID,
					Size:         size,
					Location:     d.Spec.UUID,
					LocationType: apiV1.LocationTypeDrive,
					Mode:         apiV1.ModeFS,
					Type:         ld.FSType,
					Health:       d.Spec.Health,
					CSIStatus:    "",
				})
				ll.Infof("Creating volume CR: %v", volumeCR)
				if err = vd.k8sClient.CreateCR(context.Background(), partUUID, volumeCR); err != nil {
					ll.Errorf("Unable to create volume CR %s: %v", partUUID, err)
				}
			}
		}
	}
	return nil
}

// DiscoverAvailableCapacity inspect current available capacity on nodes and fill AC CRs. This method manages only
// hardware available capacity such as HDD or SSD. If drive is healthy and online and also it is not used in LVGs
// and it doesn't contain volume then this drive is in AvailableCapacity CRs.
// Returns error if at least one drive from cache was handled badly
func (vd *VolumeDiscoverer) discoverAvailableCapacity(ctx context.Context, nodeID string) error {
	ll := vd.log.WithFields(logrus.Fields{
		"method": "discoverAvailableCapacity",
	})

	var (
		err      error
		wasError = false
		lvgList  = &lvgcrd.LVGList{}
		acList   = &accrd.AvailableCapacityList{}
	)

	if err = vd.k8sClient.ReadList(ctx, lvgList); err != nil {
		return fmt.Errorf("failed to get LVG CRs list: %v", err)
	}
	if err = vd.k8sClient.ReadList(ctx, acList); err != nil {
		return fmt.Errorf("unable to read AC list: %v", err)
	}

	for _, drive := range vd.crHelper.GetDriveCRs(vd.nodeID) {
		if drive.Spec.Health != apiV1.HealthGood || drive.Spec.Status != apiV1.DriveStatusOnline {
			continue
		}

		capacity := &api.AvailableCapacity{
			Size:         drive.Spec.Size,
			Location:     drive.Spec.UUID,
			StorageClass: util.ConvertDriveTypeToStorageClass(drive.Spec.Type),
			NodeId:       nodeID,
		}

		name := uuid.New().String()

		// check whether appropriate AC exists or not
		acExist := false
		for _, ac := range acList.Items {
			if ac.Spec.Location == drive.Spec.UUID {
				acExist = true
				break
			}
		}
		if !acExist {
			newAC := vd.k8sClient.ConstructACCR(name, *capacity)
			ll.Infof("Creating Available Capacity %v", newAC)
			if err := vd.k8sClient.CreateCR(context.WithValue(ctx, k8s.RequestUUID, name),
				name, newAC); err != nil {
				ll.Errorf("Error during CreateAvailableCapacity request to k8s: %v, error: %v",
					capacity, err)
				wasError = true
			}
		}
	}

	if wasError {
		return errors.New("not all available capacity were created")
	}

	return nil
}

// drivesAreNotUsed search drives in drives CRs that isn't have any volumes
// Returns slice of pointers on drivecrd.Drive structs
func (vd *VolumeDiscoverer) drivesAreNotUsed() []*drivecrd.Drive {
	// search drives that don't have parent volume
	drives := make([]*drivecrd.Drive, 0)
	for _, d := range vd.crHelper.GetDriveCRs(vd.nodeID) {
		isUsed := false
		for _, v := range vd.crHelper.GetVolumeCRs(vd.nodeID) {
			// expect only Drive LocationType, for Drive LocationType Location will be a UUID of the drive
			if strings.EqualFold(d.Spec.UUID, v.Spec.Location) {
				isUsed = true
				break
			}
		}
		if !isUsed {
			dInst := d
			drives = append(drives, &dInst)
		}
	}
	return drives
}

// discoverLVGOnSystemDrive discovers LVG configuration on system SSD drive and creates LVG CR and AC CR,
// return nil in case of success. If system drive is not SSD or LVG CR that points in system VG is exists - return nil.
// If system VG free space is less then threshold - AC CR will not be created but LVG will.
// Returns error in case of error on any step
func (vd *VolumeDiscoverer) discoverLVGOnSystemDrive() error {
	ll := vd.log.WithField("method", "discoverLVGOnSystemDrive")

	var (
		lvgList = lvgcrd.LVGList{}
		errTmpl = "unable to inspect system LVM, error: %v"
		err     error
	)

	// at first check whether LVG on system drive exists or no
	if err = vd.k8sClient.ReadList(context.Background(), &lvgList); err != nil {
		return fmt.Errorf(errTmpl, err)
	}

	for _, lvg := range lvgList.Items {
		if lvg.Spec.Node == vd.nodeID && len(lvg.Spec.Locations) > 0 && lvg.Spec.Locations[0] == base.SystemDriveAsLocation {
			var vgFreeSpace int64
			if vgFreeSpace, err = vd.lvmOps.GetVgFreeSpace(lvg.Spec.Name); err != nil {
				return err
			}
			ll.Infof("LVG CR that points on system VG is exists: %v", lvg)
			return vd.createACIfFreeSpace(lvg.Name, apiV1.StorageClassSystemLVG, vgFreeSpace)
		}
	}

	var (
		rootMountPoint, vgName string
		vgFreeSpace            int64
	)

	if rootMountPoint, err = vd.fsOps.FindMountPoint(base.KubeletRootPath); err != nil {
		return fmt.Errorf(errTmpl, err)
	}

	// from container we expect here name like "VG_NAME[/var/lib/kubelet/pods]"
	rootMountPoint = strings.Split(rootMountPoint, "[")[0]

	devices, err := vd.listBlk.GetBlockDevices(rootMountPoint)
	if err != nil {
		return fmt.Errorf(errTmpl, err)
	}

	if devices[0].Rota != base.NonRotationalNum {
		vd.discoverLvgSSD = false
		ll.Infof("System disk is not SSD. LVG will not be created base on it.")
		return nil
	}

	lvgExists, err := vd.lvmOps.IsLVGExists(rootMountPoint)

	if err != nil {
		return fmt.Errorf(errTmpl, err)
	}

	if !lvgExists {
		vd.discoverLvgSSD = false
		ll.Infof("System disk is SSD. but it doesn't have LVG.")
		return nil
	}

	if vgName, err = vd.lvmOps.FindVgNameByLvName(rootMountPoint); err != nil {
		return fmt.Errorf(errTmpl, err)
	}
	if vgFreeSpace, err = vd.lvmOps.GetVgFreeSpace(vgName); err != nil {
		return fmt.Errorf(errTmpl, err)
	}
	lvs := vd.lvmOps.GetLVsInVG(vgName)
	var (
		vgCRName = uuid.New().String()
		vg       = api.LogicalVolumeGroup{
			Name:       vgName,
			Node:       vd.nodeID,
			Locations:  []string{base.SystemDriveAsLocation},
			Size:       vgFreeSpace,
			Status:     apiV1.Created,
			VolumeRefs: lvs,
		}
		vgCR = vd.k8sClient.ConstructLVGCR(vgCRName, vg)
		ctx  = context.WithValue(context.Background(), k8s.RequestUUID, vg.Name)
	)
	if err = vd.k8sClient.CreateCR(ctx, vg.Name, vgCR); err != nil {
		return fmt.Errorf("unable to create LVG CR %v, error: %v", vgCR, err)
	}
	return vd.createACIfFreeSpace(vgCRName, apiV1.StorageClassSystemLVG, vgFreeSpace)
}

// handleDriveStatusChange removes AC that is based on unhealthy drive, returns AC if drive returned to healthy state,
// mark volumes of the unhealthy drive as unhealthy.
// Receives golang context and api.Drive that should be handled
func (vd *VolumeDiscoverer) handleDriveStatusChange(ctx context.Context, drive *api.Drive) {
	ll := vd.log.WithFields(logrus.Fields{
		"method":  "handleDriveStatusChange",
		"driveID": drive.UUID,
	})

	ll.Infof("The new drive status from DriveMgr is %s", drive.Health)

	// Handle resources without LVG
	// Remove AC based on disk with health BAD, SUSPECT, UNKNOWN
	if drive.Health != apiV1.HealthGood || drive.Status == apiV1.DriveStatusOffline {
		ac := vd.crHelper.GetACByLocation(drive.UUID)
		if ac != nil {
			ll.Infof("Removing AC %s based on unhealthy location %s", ac.Name, ac.Spec.Location)
			if err := vd.k8sClient.DeleteCR(ctx, ac); err != nil {
				ll.Errorf("Failed to delete unhealthy available capacity CR: %v", err)
			}
		}
	}

	// Set disk's health status to volume CR
	vol := vd.crHelper.GetVolumeByLocation(drive.UUID)
	if vol != nil {
		ll.Infof("Setting updated status %s to volume %s", drive.Health, vol.Name)
		// save previous health state
		prevHealthState := vol.Spec.Health
		vol.Spec.Health = drive.Health
		if err := vd.k8sClient.UpdateCR(ctx, vol); err != nil {
			ll.Errorf("Failed to update volume CR's %s health status: %v", vol.Name, err)
		}
		if vol.Spec.Health == apiV1.HealthBad {
			vd.recorder.Eventf(vol, eventing.WarningType, eventing.VolumeBadHealth,
				"Volume health transitioned from %s to %s. Inherited from %s drive on %s)",
				prevHealthState, vol.Spec.Health, drive.Health, drive.NodeId)
		}
	}

	// Handle resources with LVG
	// This is not work for the current moment because HAL doesn't monitor disks with LVM
	// TODO AK8S-472 Handle disk health which are used by LVGs
}

// drivesAreTheSame check whether two drive represent same node drive or no
// method is rely on that each drive could be uniquely identified by it VID/PID/Serial Number
func (vd *VolumeDiscoverer) drivesAreTheSame(drive1, drive2 *api.Drive) bool {
	return drive1.SerialNumber == drive2.SerialNumber &&
		drive1.VID == drive2.VID &&
		drive1.PID == drive2.PID
}

// createACIfFreeSpace create AC CR if there are free spcae on drive
// Receive context, drive location, storage class, size of available capacity
// Return error
func (vd *VolumeDiscoverer) createACIfFreeSpace(location string, sc string, size int64) error {
	ll := vd.log.WithFields(logrus.Fields{
		"method": "createACIfFreeSpace",
	})
	if size == 0 {
		size++ // if size is 0 it field will not display for CR
	}
	acCR := vd.crHelper.GetACByLocation(location)
	if acCR != nil {
		return nil
	}
	if size > common.AcSizeMinThresholdBytes {
		acName := uuid.New().String()
		acCR = vd.k8sClient.ConstructACCR(acName, api.AvailableCapacity{
			Location:     location,
			NodeId:       vd.nodeID,
			StorageClass: sc,
			Size:         size,
		})
		if err := vd.k8sClient.CreateCR(context.Background(), acName, acCR); err != nil {
			return fmt.Errorf("unable to create AC based on system LVG, error: %v", err)
		}
		ll.Infof("Created AC %v for lvg %s", acCR, location)
		return nil
	}
	ll.Infof("There is no available space on %s", location)
	return nil
}

// createEventsForDriveUpdates create required events for drive state change
func (vd *VolumeDiscoverer) createEventsForDriveUpdates(updates *driveUpdates) {
	for _, createdDrive := range updates.Created {
		vd.sendEventForDrive(createdDrive, eventing.InfoType, eventing.DriveDiscovered,
			"New drive discovered SN: %s, Node: %s.",
			createdDrive.Spec.SerialNumber, createdDrive.Spec.NodeId)
		vd.createEventForDriveHealthChange(
			createdDrive, apiV1.HealthUnknown, createdDrive.Spec.Health)
	}
	for _, updDrive := range updates.Updated {
		if updDrive.CurrentState.Spec.Health != updDrive.PreviousState.Spec.Health {
			vd.createEventForDriveHealthChange(
				updDrive.CurrentState, updDrive.PreviousState.Spec.Health, updDrive.CurrentState.Spec.Health)
		}
		if updDrive.CurrentState.Spec.Status != updDrive.PreviousState.Spec.Status {
			vd.createEventForDriveStatusChange(
				updDrive.CurrentState, updDrive.PreviousState.Spec.Status, updDrive.CurrentState.Spec.Status)
		}
	}
}

func (vd *VolumeDiscoverer) createEventForDriveHealthChange(
	drive *drivecrd.Drive, prevHealth, currentHealth string) {
	healthMsgTemplate := "Drive health is: %s, previous state: %s."
	eventType := eventing.WarningType
	var reason string
	switch currentHealth {
	case apiV1.HealthGood:
		eventType = eventing.InfoType
		reason = eventing.DriveHealthGood
	case apiV1.HealthBad:
		eventType = eventing.ErrorType
		reason = eventing.DriveHealthFailure
	case apiV1.HealthSuspect:
		reason = eventing.DriveHealthSuspect
	case apiV1.HealthUnknown:
		reason = eventing.DriveHealthUnknown
	default:
		return
	}
	vd.sendEventForDrive(drive, eventType, reason,
		healthMsgTemplate, currentHealth, prevHealth)
}

func (vd *VolumeDiscoverer) createEventForDriveStatusChange(
	drive *drivecrd.Drive, prevStatus, currentStatus string) {
	statusMsgTemplate := "Drive status is: %s, previous status: %s."
	eventType := eventing.InfoType
	var reason string
	switch currentStatus {
	case apiV1.DriveStatusOnline:
		reason = eventing.DriveStatusOnline
	case apiV1.DriveStatusOffline:
		eventType = eventing.ErrorType
		reason = eventing.DriveStatusOffline
	default:
		return
	}
	vd.sendEventForDrive(drive, eventType, reason,
		statusMsgTemplate, currentStatus, prevStatus)
}

func (vd *VolumeDiscoverer) sendEventForDrive(drive *drivecrd.Drive, eventtype, reason, messageFmt string,
	args ...interface{}) {
	messageFmt += prepareDriveDescription(drive)
	vd.recorder.Eventf(drive, eventtype, reason, messageFmt, args...)
}

func prepareDriveDescription(drive *drivecrd.Drive) string {
	return fmt.Sprintf(" Drive Details: SN='%s', Node='%s',"+
		" Type='%s', Model='%s %s',"+
		" Size='%d', Firmware='%s'",
		drive.Spec.SerialNumber, drive.Spec.NodeId, drive.Spec.Type,
		drive.Spec.VID, drive.Spec.PID, drive.Spec.Size, drive.Spec.Firmware)
}
