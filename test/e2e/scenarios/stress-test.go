package scenarios

import (
	"fmt"
	"time"

	"github.com/onsi/ginkgo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kubernetes/test/e2e/framework"
	e2elog "k8s.io/kubernetes/test/e2e/framework/log"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	"k8s.io/kubernetes/test/e2e/storage/testsuites"

	"eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/test/e2e/common"
)

var ssName = "stress-test-ss"

// DefineStressTestSuite defines custom baremetal-csi stress test
func DefineStressTestSuite(driver testsuites.TestDriver) {
	ginkgo.Context("Baremetal-csi drive health change tests", func() {
		// Test scenario:
		// Create StatefulSet with replicas count which is equal to kind node count
		// Each replica should consume all loop devices from the node in HDD SC
		driveStressTest(driver)
	})
}

// driveStressTest test checks behavior of driver under horizontal scale load (increase amount of nodes)
func driveStressTest(driver testsuites.TestDriver) {
	var (
		k8sSC             *storagev1.StorageClass
		driverCleanup     func()
		ns                string
		f                 = framework.NewDefaultFramework("stress")
		statefulSetSchema schema.GroupKind
		amountOfCSINodes  int
	)

	init := func() {
		var (
			perTestConf *testsuites.PerTestConfig
			err         error
		)
		ns = f.Namespace.Name

		perTestConf, driverCleanup = driver.PrepareTest(f)

		nodeList, err := f.ClientSet.CoreV1().Nodes().List(metav1.ListOptions{})
		framework.ExpectNoError(err)
		// -1 because of control plane which is unscheduled for pods
		amountOfCSINodes = len(nodeList.Items)-1

		k8sSC = driver.(*baremetalDriver).GetDynamicProvisionStorageClass(perTestConf, "xfs")
		k8sSC, err = f.ClientSet.StorageV1().StorageClasses().Create(k8sSC)
		framework.ExpectNoError(err)

		// wait for csi pods to be running and ready
		err = e2epod.WaitForPodsRunningReady(f.ClientSet, ns, int32(amountOfCSINodes), 0, 90*time.Second, nil)
		framework.ExpectNoError(err)
	}

	cleanup := func() {
		e2elog.Logf("Starting cleanup for test StressTest")

		err := framework.DeleteResourceAndWaitForGC(f.ClientSet, statefulSetSchema, ns, ssName)
		framework.ExpectNoError(err)

		pvcList, err := f.ClientSet.CoreV1().PersistentVolumeClaims(ns).List(metav1.ListOptions{})
		framework.ExpectNoError(err)
		pvcPointersList := make([]*corev1.PersistentVolumeClaim, len(pvcList.Items))
		for i, _ := range pvcList.Items {
			pvcPointersList[i] = &pvcList.Items[i]
		}

		common.CleanupAfterCustomTest(f, driverCleanup, nil, pvcPointersList)
	}

	ginkgo.It("should serve DaemonSet on multi node cluster", func() {
		init()
		defer cleanup()

		ss := CreateStressTestStatefulSet(ns, int32(amountOfCSINodes), 3, k8sSC.Name,
			driver.(testsuites.DynamicPVTestDriver).GetClaimSize())
		ss, err := f.ClientSet.AppsV1().StatefulSets(ns).Create(ss)
		framework.ExpectNoError(err)
		statefulSetSchema = ss.GroupVersionKind().GroupKind()

		err = framework.WaitForStatefulSetReplicasReady(ss.Name, ns, f.ClientSet, 20*time.Second, 10*time.Minute)
		framework.ExpectNoError(err)
	})
}

func CreateStressTestStatefulSet(ns string, amountOfReplicas int32, volumesPerReplica int, scName string,
	claimSize string) *appsv1.StatefulSet {
	pvcs := make([]corev1.PersistentVolumeClaim, volumesPerReplica)
	vms := make([]corev1.VolumeMount, volumesPerReplica)
	pvcPrefix := "volume%d"
	dataPath := "/data/%s"

	for i, _ := range pvcs {
		pvcs[i] = corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf(pvcPrefix, i),
				Namespace: ns,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteOnce,
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse(claimSize),
					},
				},
				StorageClassName: &scName,
			},
		}
	}

	for i := 0; i < volumesPerReplica; i++ {
		volumeName := pvcs[i].Name
		vms[i] = corev1.VolumeMount{
			Name:      volumeName,
			MountPath: fmt.Sprintf(dataPath, volumeName),
		}
	}

	podTemplate := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"app": ssName},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:            "busybox",
					Image:           "busybox:1.29",
					Command:         []string{"/bin/sh", "-c", "sleep 36000"},
					VolumeMounts:    vms,
					ImagePullPolicy: "Never",
				},
			},
			Affinity: &corev1.Affinity{
				PodAntiAffinity: &corev1.PodAntiAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "app",
										Operator: "In",
										Values:   []string{ssName},
									},
								},
							},
							TopologyKey: "kubernetes.io/hostname",
						},
					},
				},
			},
		},
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ssName,
			Namespace: ns,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &amountOfReplicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": ssName},
			},
			Template:             podTemplate,
			VolumeClaimTemplates: pvcs,
			ServiceName:          ssName,
			PodManagementPolicy:  "Parallel",
		},
	}
}