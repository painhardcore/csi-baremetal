package losetup

import (
	"encoding/json"

	"github.com/sirupsen/logrus"

	"eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/pkg/base/command"
)

const (
	losetupCmd                    = "losetup"
	findAllLoopBackDevicesCmdTmpl = losetupCmd + " -J"
)

//{
//"loopdevices": [
//{"name": "/dev/loop29", "sizelimit": "0", "offset": "0", "autoclear": "0", "ro": "0", "back-file": "/host/home/baremetal-csi-node-7cj9p-2160067800.img", "dio": "0"},
//{"name": "/dev/loop57", "sizelimit": "0", "offset": "0", "autoclear": "0", "ro": "0", "back-file": "/host/home/baremetal-csi-node-qznk9-4138873595.img", "dio": "0"},
//{"name": "/dev/loop19", "sizelimit": "0", "offset": "0", "autoclear": "0", "ro": "0", "back-file": "/host/home/baremetal-csi-node-xwjh9-708954500.img", "dio": "0"},
//{"name": "/dev/loop39", "sizelimit": "0", "offset": "0", "autoclear": "0", "ro": "0", "back-file": "/host/home/baremetal-csi-node-7cj9p-1923947331.img", "dio": "0"},
//{"name": "/dev/loop67", "sizelimit": "0", "offset": "0", "autoclear": "0", "ro": "0", "back-file": "/host/home/baremetal-csi-node-7cj9p-623686980.img", "dio": "0"}
//]
//}

// ListDevices is for unmarshalling output
type ListDevices struct {
	LoopBackDevices []LoopBackDevice `json:"loopdevices"`
}

// LoopBackDevice is a device with small amount of fields
type LoopBackDevice struct {
	Name     string `json:"name"`
	BackFile string `json:"back-file"`
}

// WrapLOSETUP is an interface that encapsulates operation with system losetup util
type WrapLOSETUP interface {
	GetLoopBackDevices() ([]LoopBackDevice, error)
}

// LOSETUP is a wrap for system losetup util
type LOSETUP struct {
	e   command.CmdExecutor
	log *logrus.Entry
}

// GetLoopBackDevices gets all devices from losetup
func (lo *LOSETUP) GetLoopBackDevices() ([]LoopBackDevice, error) {
	stdout, stderr, err := lo.e.RunCmd(findAllLoopBackDevicesCmdTmpl)
	if err != nil {
		lo.log.Errorf("Unable to get loopback configurations f: %s", stderr)
		return nil, err
	}
	var list ListDevices
	err = json.Unmarshal([]byte(stdout), &list)
	if err != nil {
		lo.log.Errorf("Unable to parse output: %s", stderr)
		return nil, err
	}
	return list.LoopBackDevices, nil
}

// NewLOSETUP is just a constructor for LOSETUP
func NewLOSETUP(e command.CmdExecutor, log *logrus.Entry) *LOSETUP {
	return &LOSETUP{e: e, log: log.WithField("component", "LOSETUP")}
}
