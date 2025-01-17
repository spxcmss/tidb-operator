// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package member

import (
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/tidb-operator/pkg/apis/label"
	"github.com/pingcap/tidb-operator/pkg/apis/pingcap/v1alpha1"
	"github.com/pingcap/tidb-operator/pkg/controller"
	"github.com/pingcap/tidb-operator/pkg/pdapi"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/pointer"
)

func TestPDScalerScaleOut(t *testing.T) {
	g := NewGomegaWithT(t)
	type testcase struct {
		name             string
		update           func(cluster *v1alpha1.TidbCluster)
		pdUpgrading      bool
		hasPVC           bool
		hasDeferAnn      bool
		annoIsNil        bool
		pvcDeleteErr     bool
		statusSyncFailed bool
		err              bool
		changed          bool
	}

	testFn := func(test testcase, t *testing.T) {
		tc := newTidbClusterForPD()
		test.update(tc)

		if test.pdUpgrading {
			tc.Status.PD.Phase = v1alpha1.UpgradePhase
		}

		oldSet := newStatefulSetForPDScale()
		newSet := oldSet.DeepCopy()
		newSet.Spec.Replicas = pointer.Int32Ptr(7)

		scaler, _, pvcIndexer, _, pvcControl := newFakePDScaler()

		pvc := newPVCForStatefulSet(oldSet, v1alpha1.PDMemberType, tc.Name)
		if !test.annoIsNil {
			pvc.Annotations = map[string]string{}
		}

		if test.hasDeferAnn {
			pvc.Annotations = map[string]string{}
			pvc.Annotations[label.AnnPVCDeferDeleting] = time.Now().Format(time.RFC3339)
		}
		if test.hasPVC {
			pvcIndexer.Add(pvc)
		}

		if test.pvcDeleteErr {
			pvcControl.SetDeletePVCError(errors.NewInternalError(fmt.Errorf("API server failed")), 0)
		}

		tc.Status.PD.Synced = !test.statusSyncFailed

		err := scaler.ScaleOut(tc, oldSet, newSet)
		if test.err {
			g.Expect(err).To(HaveOccurred())
		} else {
			g.Expect(err).NotTo(HaveOccurred())
		}
		if test.changed {
			g.Expect(int(*newSet.Spec.Replicas)).To(Equal(6))
		} else {
			g.Expect(int(*newSet.Spec.Replicas)).To(Equal(5))
		}
	}

	tests := []testcase{
		{
			name:             "normal",
			update:           normalPDMember,
			pdUpgrading:      false,
			hasPVC:           true,
			hasDeferAnn:      false,
			annoIsNil:        true,
			pvcDeleteErr:     false,
			statusSyncFailed: false,
			err:              false,
			changed:          true,
		},
		{
			name:             "pd is upgrading",
			update:           normalPDMember,
			pdUpgrading:      true,
			hasPVC:           true,
			hasDeferAnn:      false,
			annoIsNil:        true,
			pvcDeleteErr:     false,
			statusSyncFailed: false,
			err:              false,
			changed:          true,
		},
		{
			name:             "cache don't have pvc",
			update:           normalPDMember,
			pdUpgrading:      false,
			hasPVC:           false,
			hasDeferAnn:      false,
			annoIsNil:        true,
			pvcDeleteErr:     false,
			statusSyncFailed: false,
			err:              false,
			changed:          true,
		},
		{
			name:             "pvc annotation is not nil but doesn't contain defer deletion annotation",
			update:           normalPDMember,
			pdUpgrading:      false,
			hasPVC:           true,
			hasDeferAnn:      false,
			annoIsNil:        false,
			pvcDeleteErr:     false,
			statusSyncFailed: false,
			err:              false,
			changed:          true,
		},
		{
			name:             "pvc annotations defer deletion is not nil, pvc delete failed",
			update:           normalPDMember,
			pdUpgrading:      false,
			hasPVC:           true,
			hasDeferAnn:      true,
			annoIsNil:        false,
			pvcDeleteErr:     true,
			statusSyncFailed: false,
			err:              true,
			changed:          false,
		},
		{
			name: "failover now",
			update: func(tc *v1alpha1.TidbCluster) {
				normalPDMember(tc)
				podName := ordinalPodName(v1alpha1.PDMemberType, tc.GetName(), 0)
				tc.Status.PD.FailureMembers = map[string]v1alpha1.PDFailureMember{
					podName: {PodName: podName},
				}
				pd := tc.Status.PD.Members[podName]
				pd.Health = false
				tc.Status.PD.Members[podName] = pd
			},
			pdUpgrading:      false,
			hasPVC:           true,
			hasDeferAnn:      true,
			annoIsNil:        false,
			pvcDeleteErr:     false,
			statusSyncFailed: false,
			err:              false,
			changed:          true,
		},
		{
			name:             "pd status sync failed",
			update:           normalPDMember,
			pdUpgrading:      false,
			hasPVC:           true,
			hasDeferAnn:      false,
			annoIsNil:        true,
			pvcDeleteErr:     false,
			statusSyncFailed: true,
			err:              true,
			changed:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testFn(tt, t)
		})
	}
}

func TestPDScalerScaleIn(t *testing.T) {
	g := NewGomegaWithT(t)
	type testcase struct {
		name             string
		pdUpgrading      bool
		hasPVC           bool
		pvcUpdateErr     bool
		deleteMemberErr  bool
		statusSyncFailed bool
		err              bool
		changed          bool
		isLeader         bool
	}

	testFn := func(test testcase, t *testing.T) {
		tc := newTidbClusterForPD()

		if test.pdUpgrading {
			tc.Status.PD.Phase = v1alpha1.UpgradePhase
		}

		oldSet := newStatefulSetForPDScale()
		newSet := oldSet.DeepCopy()
		newSet.Spec.Replicas = pointer.Int32Ptr(3)

		pod := &corev1.Pod{
			TypeMeta: metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
			ObjectMeta: metav1.ObjectMeta{
				Name:              PdPodName(tc.GetName(), 4),
				Namespace:         corev1.NamespaceDefault,
				CreationTimestamp: metav1.Time{Time: time.Now().Add(-1 * time.Hour)},
			},
		}

		scaler, pdControl, pvcIndexer, podIndexer, pvcControl := newFakePDScaler()

		podIndexer.Add(pod)

		if test.hasPVC {
			pvc1 := newScaleInPVCForStatefulSet(oldSet, v1alpha1.PDMemberType, tc.Name)
			pvc2 := pvc1.DeepCopy()
			pvc1.Name = pvc1.Name + "-1"
			pvc1.UID = pvc1.UID + "-1"
			pvc2.Name = pvc2.Name + "-2"
			pvc2.UID = pvc2.UID + "-2"
			pvcIndexer.Add(pvc1)
			pvcIndexer.Add(pvc2)
			pod.Spec.Volumes = append(pod.Spec.Volumes,
				corev1.Volume{
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvc1.Name,
						},
					},
				},
				corev1.Volume{
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvc2.Name,
						},
					},
				})
		}

		pdClient := controller.NewFakePDClient(pdControl, tc)

		pdClient.AddReaction(pdapi.GetPDLeaderActionType, func(action *pdapi.Action) (interface{}, error) {
			leader := pdpb.Member{
				Name: fmt.Sprintf("%s-pd-%d", tc.GetName(), 0),
			}
			return &leader, nil
		})

		if test.deleteMemberErr {
			pdClient.AddReaction(pdapi.DeleteMemberActionType, func(action *pdapi.Action) (interface{}, error) {
				return nil, fmt.Errorf("error")
			})
		}
		if test.pvcUpdateErr {
			pvcControl.SetUpdatePVCError(errors.NewInternalError(fmt.Errorf("API server failed")), 0)
		}

		if test.isLeader {
			pdClient.AddReaction(pdapi.GetPDLeaderActionType, func(action *pdapi.Action) (interface{}, error) {
				leader := pdpb.Member{
					Name: fmt.Sprintf("%s-pd-%d", tc.GetName(), 4),
				}
				return &leader, nil
			})
			pdClient.AddReaction(pdapi.TransferPDLeaderActionType, func(action *pdapi.Action) (i interface{}, e error) {
				return nil, nil
			})
		}

		tc.Status.PD.Synced = !test.statusSyncFailed

		err := scaler.ScaleIn(tc, oldSet, newSet)
		if test.err {
			g.Expect(err).To(HaveOccurred())
		} else {
			g.Expect(err).NotTo(HaveOccurred())
		}
		if test.changed {
			g.Expect(int(*newSet.Spec.Replicas)).To(Equal(4))
		} else {
			g.Expect(int(*newSet.Spec.Replicas)).To(Equal(5))
		}
	}

	tests := []testcase{
		{
			name:             "normal",
			pdUpgrading:      false,
			hasPVC:           true,
			pvcUpdateErr:     false,
			deleteMemberErr:  false,
			statusSyncFailed: false,
			err:              false,
			changed:          true,
			isLeader:         false,
		},
		{
			name:             "able to scale in while pd is upgrading",
			pdUpgrading:      true,
			hasPVC:           true,
			pvcUpdateErr:     false,
			deleteMemberErr:  false,
			statusSyncFailed: false,
			err:              false,
			changed:          true,
			isLeader:         false,
		},
		{
			name:             "error when delete member",
			hasPVC:           true,
			pvcUpdateErr:     false,
			pdUpgrading:      false,
			deleteMemberErr:  true,
			statusSyncFailed: false,
			err:              true,
			changed:          false,
			isLeader:         false,
		},
		{
			name:             "cache don't have pvc",
			pdUpgrading:      false,
			hasPVC:           false,
			pvcUpdateErr:     false,
			deleteMemberErr:  false,
			statusSyncFailed: false,
			err:              true,
			changed:          false,
			isLeader:         false,
		},
		{
			name:             "error when update pvc",
			pdUpgrading:      false,
			hasPVC:           true,
			pvcUpdateErr:     true,
			deleteMemberErr:  false,
			statusSyncFailed: false,
			err:              true,
			changed:          false,
			isLeader:         false,
		},
		{
			name:             "pd status sync failed",
			pdUpgrading:      false,
			hasPVC:           true,
			pvcUpdateErr:     false,
			deleteMemberErr:  false,
			statusSyncFailed: true,
			err:              true,
			changed:          false,
			isLeader:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testFn(tt, t)
		})
	}
}

func TestPDScalerScaleInBlockByOtherComponents(t *testing.T) {
	// check if PD scale in is blocked when other components are using PD
	g := NewGomegaWithT(t)
	type testcase struct {
		name    string
		tikv    bool
		tidb    bool
		tiflash bool
		ticdc   bool
		pump    bool
	}

	testFn := func(test testcase, t *testing.T) {
		tc := newTidbClusterForPD()

		oldSet := newStatefulSetForPDScale()
		newSet := oldSet.DeepCopy()
		newSet.Spec.Replicas = pointer.Int32Ptr(3)

		scaler, _, _, _, _ := newFakePDScaler()

		tc.Spec.PD.Replicas = 0

		if test.tikv {
			tc.Status.TiKV.Stores = map[string]v1alpha1.TiKVStore{
				"1": {
					ID:      "1",
					PodName: ordinalPodName(v1alpha1.TiKVMemberType, tc.GetName(), 4),
					State:   v1alpha1.TiKVStateUp,
				},
			}
		} else {
			tc.Status.TiKV.Stores = nil
		}

		if test.tidb {
			tc.Status.TiDB.Members = map[string]v1alpha1.TiDBMember{
				"failover-tidb-0": {
					Name:   "failover-tidb-0",
					Health: true,
				},
			}
		} else {
			tc.Status.TiDB.Members = nil
		}

		if test.tiflash {
			tc.Status.TiFlash.Stores = map[string]v1alpha1.TiKVStore{
				"1": {
					ID:      "1",
					PodName: ordinalPodName(v1alpha1.TiFlashMemberType, tc.GetName(), 4),
					State:   v1alpha1.TiKVStateUp,
				},
			}
		} else {
			tc.Status.TiFlash.Stores = nil
		}

		if test.ticdc {
			tc.Status.TiCDC.StatefulSet = &apps.StatefulSetStatus{Replicas: 1}
		} else {
			tc.Status.TiCDC.StatefulSet = &apps.StatefulSetStatus{Replicas: 0}
		}

		if test.pump {
			tc.Status.Pump.StatefulSet = &apps.StatefulSetStatus{Replicas: 1}
		} else {
			tc.Status.Pump.StatefulSet = &apps.StatefulSetStatus{Replicas: 0}
		}

		result := scaler.preCheckUpMembers(tc, "pd-1")
		if test.tikv || test.tidb || test.tiflash || test.ticdc || test.pump {
			g.Expect(result).To(BeFalse())
		} else {
			g.Expect(result).To(BeTrue())
		}
	}

	tests := []testcase{
		{
			name:    "tikv on",
			tikv:    true,
			tidb:    false,
			tiflash: false,
			ticdc:   false,
			pump:    false,
		},
		{
			name:    "tidb on",
			tikv:    false,
			tidb:    true,
			tiflash: false,
			ticdc:   false,
			pump:    false,
		},
		{
			name:    "tiflash on",
			tikv:    false,
			tidb:    false,
			tiflash: true,
			ticdc:   false,
			pump:    false,
		},
		{
			name:    "ticdc on",
			tikv:    false,
			tidb:    false,
			tiflash: false,
			ticdc:   true,
			pump:    false,
		},
		{
			name:    "pump on",
			tikv:    false,
			tidb:    false,
			tiflash: false,
			ticdc:   false,
			pump:    true,
		},
		{
			name:    "all zero",
			tikv:    false,
			tidb:    false,
			tiflash: false,
			ticdc:   false,
			pump:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testFn(tt, t)
		})
	}
}

func newFakePDScaler() (*pdScaler, *pdapi.FakePDControl, cache.Indexer, cache.Indexer, *controller.FakePVCControl) {
	fakeDeps := controller.NewFakeDependencies()
	pdScaler := &pdScaler{generalScaler: generalScaler{deps: fakeDeps}}
	pdControl := fakeDeps.PDControl.(*pdapi.FakePDControl)
	pvcIndexer := fakeDeps.KubeInformerFactory.Core().V1().PersistentVolumeClaims().Informer().GetIndexer()
	podIndexer := fakeDeps.KubeInformerFactory.Core().V1().Pods().Informer().GetIndexer()
	pvcControl := fakeDeps.PVCControl.(*controller.FakePVCControl)
	return pdScaler, pdControl, pvcIndexer, podIndexer, pvcControl
}

func newStatefulSetForPDScale() *apps.StatefulSet {
	set := &apps.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scaler",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: apps.StatefulSetSpec{
			Replicas: pointer.Int32Ptr(5),
		},
	}
	return set
}

func _newPVCForStatefulSet(set *apps.StatefulSet, memberType v1alpha1.MemberType, name string, ordinal int32) *corev1.PersistentVolumeClaim {
	podName := ordinalPodName(memberType, name, ordinal)
	var l label.Label
	switch memberType {
	case v1alpha1.DMMasterMemberType, v1alpha1.DMWorkerMemberType:
		l = label.NewDM().Instance(name)
	default:
		l = label.New().Instance(name)
	}
	l[label.AnnPodNameKey] = podName
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ordinalPVCName(memberType, set.GetName(), ordinal),
			Namespace: metav1.NamespaceDefault,
			Labels:    l,
		},
	}
}

func newPVCForStatefulSet(set *apps.StatefulSet, memberType v1alpha1.MemberType, name string) *corev1.PersistentVolumeClaim {
	return _newPVCForStatefulSet(set, memberType, name, *set.Spec.Replicas)
}

func newScaleInPVCForStatefulSet(set *apps.StatefulSet, memberType v1alpha1.MemberType, name string) *corev1.PersistentVolumeClaim {
	return _newPVCForStatefulSet(set, memberType, name, *set.Spec.Replicas-1)
}

func normalPDMember(tc *v1alpha1.TidbCluster) {
	tcName := tc.GetName()
	tc.Status.PD.Members = map[string]v1alpha1.PDMember{
		ordinalPodName(v1alpha1.PDMemberType, tcName, 0): {Health: true},
		ordinalPodName(v1alpha1.PDMemberType, tcName, 1): {Health: true},
		ordinalPodName(v1alpha1.PDMemberType, tcName, 2): {Health: true},
		ordinalPodName(v1alpha1.PDMemberType, tcName, 3): {Health: true},
		ordinalPodName(v1alpha1.PDMemberType, tcName, 4): {Health: true},
	}
}
