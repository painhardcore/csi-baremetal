package scenarios

import (
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/kubernetes/test/e2e/framework"
	e2elog "k8s.io/kubernetes/test/e2e/framework/log"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	"k8s.io/kubernetes/test/e2e/storage/testsuites"

	"github.com/dell/csi-baremetal/pkg/base/command"
	"github.com/dell/csi-baremetal/test/e2e/common"
)

func DefineNodeReplacementTestSuite(driver testsuites.TestDriver) {
	ginkgo.Context("CSI-Baremetal Node Replacement test suite", func() {
		nrTest(driver)
	})
}

func nrTest(driver testsuites.TestDriver) {
	var (
		pod           *corev1.Pod
		pvc           *corev1.PersistentVolumeClaim
		k8sSC         *storagev1.StorageClass
		executor      = &command.Executor{}
		logger        = logrus.New()
		driverCleanup func()
		ns            string
		nodeToReplace string // represents kind node
		f             = framework.NewDefaultFramework("node-replacement")
	)
	logger.SetLevel(logrus.DebugLevel)
	executor.SetLogger(logger)

	init := func() {
		var (
			perTestConf *testsuites.PerTestConfig
			err         error
		)

		ns = f.Namespace.Name
		perTestConf, driverCleanup = driver.PrepareTest(f)
		k8sSC = driver.(*baremetalDriver).GetStorageClassWithStorageType(perTestConf, storageClassHDD)
		k8sSC, err = f.ClientSet.StorageV1().StorageClasses().Create(k8sSC)
		framework.ExpectNoError(err)
	}

	cleanup := func() {
		e2elog.Logf("Starting cleanup for test NodeReplacement")

		// TODO: handle case when node wasn't added

		common.CleanupAfterCustomTest(f, driverCleanup, []*corev1.Pod{pod}, []*corev1.PersistentVolumeClaim{pvc})
	}

	ginkgo.It("Pod should consume same PV after node had being replaced", func() {
		init()
		defer cleanup()

		var err error
		// create pvc
		pvc, err = f.ClientSet.CoreV1().PersistentVolumeClaims(ns).
			Create(constructPVC(ns, driver.(testsuites.DynamicPVTestDriver).GetClaimSize(), k8sSC.Name, pvcName))
		framework.ExpectNoError(err)

		// create pod with pvc
		pod, err = e2epod.CreatePod(f.ClientSet, ns, nil, []*corev1.PersistentVolumeClaim{pvc},
			false, "sleep 3600")
		framework.ExpectNoError(err)

		e2elog.Logf("Pod %s with PVC %s created.", pod.Name, pvc.Name)

		// delete pod
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(pod.Name, nil)
		if err != nil {
			if !apierrs.IsNotFound(err) {
				framework.Failf("unable to delete pod %s: %v", pod.Name, err)
			}
		} else {
			err = e2epod.WaitForPodNotFoundInNamespace(f.ClientSet, pod.Name, f.Namespace.Name, time.Minute*2)
			framework.ExpectNoError(err)
		}

		// since test is run in Kind k8s cluster, each node is represented by docker container
		// node' name is the same as a docker container name by which this node is represented.
		nodeToReplace = pod.Spec.NodeName

		// find node UID that is used as a part of Drive.Spec.NodeID
		nodes, err := e2enode.GetReadySchedulableNodesOrDie(f.ClientSet)
		framework.ExpectNoError(err)
		var nodeID string
		var masterNodeName string
		for _, node := range nodes.Items {
			if node.Name == pod.Spec.NodeName {
				nodeID = string(node.UID)
				break
			}
			if _, ok := node.GetLabels()["node-role.kubernetes.io/master"]; ok {
				masterNodeName = node.Name
			}
		}
		if nodeID == "" {
			framework.Failf("Unable to find UID for node %s", pod.Spec.NodeName)
		}

		e2elog.Logf("Master host is %s", masterNodeName)

		// save config of Drives on that node
		allDrivesUnstr := getUObjList(f, common.DriveGVR)
		allVolumesUnstr := getUObjList(f, common.VolumeGVR)
		if len(allVolumesUnstr.Items) != 1 {
			framework.Failf("Unable to found Volume CR that corresponds to the PVC %s. VolumeCR amount - %d. %v",
				pvc.Name, len(allVolumesUnstr.Items), allVolumesUnstr.Items)
		}
		volumeLocation, _, err := unstructured.NestedString(allVolumesUnstr.Items[0].Object, "spec", "Location")
		framework.ExpectNoError(err)

		// found drive that corresponds to volumeCR
		driveOnNode := make([]common.LoopBackManagerConfigDevice, 0)
		bdev := ""
		provisionedDriveSN := ""
		for _, drive := range allDrivesUnstr.Items {
			n, _, err := unstructured.NestedString(drive.Object, "spec", "NodeId")
			framework.ExpectNoError(err)
			if n == nodeID {
				sn, _, err := unstructured.NestedString(drive.Object, "spec", "SerialNumber")
				framework.ExpectNoError(err)
				driveOnNode = append(driveOnNode, common.LoopBackManagerConfigDevice{SerialNumber: &sn})
				path, _, err := unstructured.NestedString(drive.Object, "spec", "Path")
				framework.ExpectNoError(err)
				e2elog.Logf("Append drive with SN %s and path %s", sn, path)
				if sn == volumeLocation {
					e2elog.Logf("PVC %s location is drive with SN %s and path %s", sn, path)
					bdev = path
					provisionedDriveSN = sn
				}
			}
		}
		if len(driveOnNode) == 0 {
			framework.Failf("Unable to detect which DriveCRs correspond to node %s (%s)", pod.Spec.NodeName, nodeID)
		}

		// change Loopback mgr config
		lmConfig := &common.LoopBackManagerConfig{
			Nodes: []common.LoopBackManagerConfigNode{
				{
					NodeID: &pod.Spec.NodeName,
					Drives: driveOnNode,
				},
			},
		}
		applyLMConfig(f, lmConfig)

		// delete node and add it again
		e := command.Executor{}
		logger := logrus.New()
		e.SetLogger(logger)
		e.SetLevel(logrus.DebugLevel)

		_, _, err = e.RunCmd(fmt.Sprintf("kubectl drain %s --ignore-daemonsets", nodeToReplace))
		framework.ExpectNoError(err)
		_, _, err = e.RunCmd(fmt.Sprintf("kubectl delete node %s", nodeToReplace))
		framework.ExpectNoError(err)
		_, _, err = e.RunCmd(fmt.Sprintf("docker exec -i %s kubeadm reset --force", nodeToReplace))
		framework.ExpectNoError(err)
		_, _, err = e.RunCmd(fmt.Sprintf("docker exec -i %s rm -rf /etc/kubernetes", nodeToReplace))
		framework.ExpectNoError(err)
		_, _, err = e.RunCmd(fmt.Sprintf("docker exec -i %s systemctl restart kubelet", nodeToReplace))
		framework.ExpectNoError(err)

		time.Sleep(time.Second * 5)

		var joinCommand string
		joinCommand, _, err = e.RunCmd(fmt.Sprintf("docker exec -i %s kubeadm token create --print-join-command", masterNodeName))
		framework.ExpectNoError(err)
		_, _, err = e.RunCmd(fmt.Sprintf("docker exec -i %s %s --ignore-preflight-errors=all", nodeToReplace, joinCommand))
		framework.ExpectNoError(err)

		// by drive.spec.serialNumber find drive.spec.path -> newBDev
		// read config from bdev and apply it for newBDev
		allDrivesUnstr = getUObjList(f, common.DriveGVR)
		for _, drive := range allDrivesUnstr.Items {
			sn, _, err := unstructured.NestedString(drive.Object, "spec", "SerialNumber")
			framework.ExpectNoError(err)
			if sn == provisionedDriveSN {
				newBDev, _, err := unstructured.NestedString(drive.Object, "spec", "Path")
				framework.ExpectNoError(err)
				framework.Logf("Found drive with SN %s. bdev - %s, newBDev - %s", bdev, newBDev)
				if bdev == newBDev {
					break
				}
				err = common.CopyPartitionConfig(
					bdev,
					strings.Replace(pvc.Name, "pvc-", "", 1),
					newBDev,
					logger)
				if err != nil {
					e2elog.Failf("CopyPartitionConfig finished with error: %v", err)
				}
				e2elog.Logf("Partition was restored successfully from %s to %s", bdev, newBDev)
				break
			}
		}

		// create pod again
		pod, err = e2epod.CreatePod(f.ClientSet, ns, nil, []*corev1.PersistentVolumeClaim{pvc},
			false, "sleep 3600")
		framework.ExpectNoError(err)

		// check that pod consume same pvc
		var boundAgain = false
		pods, err := e2epod.GetPodsInNamespace(f.ClientSet, f.Namespace.Name, map[string]string{})
		framework.ExpectNoError(err)

		// search pod again
		for _, p := range pods {
			if p.Name == pod.Name {
				// search volumes
				volumes := p.Spec.Volumes
				for _, v := range volumes {
					if v.PersistentVolumeClaim.ClaimName == pvc.Name {
						boundAgain = true
						break
					}
				}
				break
			}
		}
		e2elog.Logf("Pod has same PVC: %v", boundAgain)
		framework.ExpectEqual(boundAgain, true)
	})
}