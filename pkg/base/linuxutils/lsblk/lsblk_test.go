package lsblk

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"

	api "eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/api/generated/v1"
	"eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/api/v1/drivecrd"
	"eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/pkg/mocks"
)

var (
	testLogger    = logrus.New()
	allDevicesCmd = fmt.Sprintf(CmdTmpl, "")

	sn        = "sn-1111"
	testDrive = api.Drive{
		SerialNumber: sn,
	}

	testDriveCR = drivecrd.Drive{
		Spec: testDrive,
	}
)

func TestLSBLK_GetBlockDevices_Success(t *testing.T) {
	e := &mocks.GoMockExecutor{}
	l := NewLSBLK(e)

	e.On("RunCmd", allDevicesCmd).Return(mocks.LsblkTwoDevicesStr, "", nil)

	out, err := l.GetBlockDevices("")
	assert.Nil(t, err)
	assert.NotNil(t, out)
	assert.Equal(t, 2, len(out))

}

func TestLSBLK_GetBlockDevices_Fail(t *testing.T) {
	e := &mocks.GoMockExecutor{}
	l := NewLSBLK(e)

	e.On(mocks.RunCmd, allDevicesCmd).Return("not a json", "", nil).Times(1)
	out, err := l.GetBlockDevices("")
	assert.Nil(t, out)
	assert.NotNil(t, err)
	assert.Contains(t, err.Error(), "unable to unmarshal output to BlockDevice instance")

	expectedError := errors.New("lsblk failed")
	e.On(mocks.RunCmd, allDevicesCmd).Return("", "", expectedError).Times(1)
	out, err = l.GetBlockDevices("")
	assert.Nil(t, out)
	assert.NotNil(t, err)
	assert.Equal(t, expectedError, err)

	e.On(mocks.RunCmd, allDevicesCmd).Return(mocks.NoLsblkKeyStr, "", nil).Times(1)
	out, err = l.GetBlockDevices("")
	assert.Nil(t, out)
	assert.NotNil(t, err)
	assert.Contains(t, err.Error(), "unexpected lsblk output format")
}

func TestLSBLK_SearchDrivePath_Success(t *testing.T) {
	e := &mocks.GoMockExecutor{}
	l := NewLSBLK(e)

	// path is in drive spec
	dCR := testDriveCR
	path := "/dev/sda"
	dCR.Spec.Path = path

	res, err := l.SearchDrivePath(&dCR)
	assert.Nil(t, err)
	assert.Equal(t, path, res)

	// got from lsblk output
	e.On("RunCmd", allDevicesCmd).Return(mocks.LsblkTwoDevicesStr, "", nil)
	sn = "hdd1"                  // from mocks.LsblkTwoDevicesStr
	expectedDevice := "/dev/sda" // from mocks.LsblkTwoDevicesStr
	d2CR := testDriveCR
	d2CR.Spec.SerialNumber = sn

	res, err = l.SearchDrivePath(&d2CR)
	assert.Nil(t, err)
	assert.Equal(t, expectedDevice, res)
}

func TestLSBLK_SearchDrivePath(t *testing.T) {
	e := &mocks.GoMockExecutor{}
	l := NewLSBLK(e)

	// lsblk fail
	expectedErr := errors.New("lsblk error")
	e.On("RunCmd", allDevicesCmd).Return("", "", expectedErr)
	res, err := l.SearchDrivePath(&testDriveCR)
	assert.Equal(t, "", res)
	assert.Equal(t, expectedErr, err)

	// sn isn't presented in lsblk output
	e.On("RunCmd", allDevicesCmd).Return(mocks.LsblkTwoDevicesStr, "", nil)
	sn := "sn-that-isnt-existed"
	dCR := testDriveCR
	dCR.Spec.SerialNumber = sn

	res, err = l.SearchDrivePath(&dCR)
	assert.Equal(t, "", res)
	assert.NotNil(t, err)

}