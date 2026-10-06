/*
Copyright The Kubernetes Authors.

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

package scheduler

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2/ktesting"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/features"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	internalcache "k8s.io/kubernetes/pkg/scheduler/backend/cache"
	internalqueue "k8s.io/kubernetes/pkg/scheduler/backend/queue"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/gangscheduling"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/interpodaffinity"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodeaffinity"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodeports"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/tainttoleration"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/profile"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
	"k8s.io/utils/ptr"
)

func newSignTestFramework(t *testing.T, ignorePreferredTermsOfExistingPods bool) (context.Context, framework.Framework) {
	t.Helper()
	featuregatetesting.SetFeatureGatesDuringTest(t, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
		features.TopologyAwareWorkloadScheduling:            true,
		features.GenericWorkload:                            true,
		features.CompositePodGroup:                          true,
		features.TopologyAwareCompositePodGroupOptimization: true,
	})

	_, ctx := ktesting.NewTestContext(t)
	informerFactory := informers.NewSharedInformerFactory(clientsetfake.NewClientset(), 0)
	snapshot := internalcache.NewEmptySnapshot()
	fts := feature.NewSchedulerFeaturesFromGates(utilfeature.DefaultFeatureGate)

	interPodAffinityFactory := func(ctx context.Context, _ runtime.Object, h fwk.Handle) (fwk.Plugin, error) {
		return interpodaffinity.New(ctx, &schedulerapi.InterPodAffinityArgs{
			HardPodAffinityWeight:              1,
			IgnorePreferredTermsOfExistingPods: ignorePreferredTermsOfExistingPods,
		}, h, fts)
	}

	registry := []tf.RegisterPluginFunc{
		tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		tf.RegisterPluginAsExtensions(noderesources.Name, frameworkruntime.FactoryAdapter(fts, noderesources.NewFit), "PreFilter", "Filter"),
		tf.RegisterPluginAsExtensions(nodeaffinity.Name, frameworkruntime.FactoryAdapter(fts, nodeaffinity.New), "PreFilter", "Filter"),
		tf.RegisterPluginAsExtensions(tainttoleration.Name, frameworkruntime.FactoryAdapter(fts, tainttoleration.New), "PreFilter", "Filter"),
		tf.RegisterPluginAsExtensions(nodeports.Name, frameworkruntime.FactoryAdapter(fts, nodeports.New), "PreFilter", "Filter"),
		tf.RegisterPluginAsExtensions(interpodaffinity.Name, interPodAffinityFactory, "PreFilter", "Filter"),
		tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
	}

	schedFwk, err := tf.NewFramework(ctx, registry, "test-scheduler",
		frameworkruntime.WithInformerFactory(informerFactory),
		frameworkruntime.WithSnapshotSharedLister(snapshot),
		frameworkruntime.WithPodNominator(internalqueue.NewSchedulingQueue(nil, informerFactory)),
	)
	if err != nil {
		t.Fatalf("Failed to create framework: %v", err)
	}
	return ctx, schedFwk
}

func TestAreChildPodGroupsIdentical(t *testing.T) {
	basePodSpec := func() v1.PodSpec {
		return v1.PodSpec{
			NodeSelector: map[string]string{"zone": "us-east-1"},
			Tolerations: []v1.Toleration{
				{Key: "dedicated", Operator: v1.TolerationOpEqual, Value: "gpu", Effect: v1.TaintEffectNoSchedule},
			},
			Priority: ptr.To(int32(100)),
			Containers: []v1.Container{
				{
					Name: "worker",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
					Ports: []v1.ContainerPort{{ContainerPort: 8080, HostPort: 8080}},
				},
			},
		}
	}

	makePGInfo := func(name string, spec v1.PodSpec, numPods int, minCount int32, topologyKey string) *framework.PodGroupInfo {
		pg := st.MakePodGroup().Name(name).MinCount(minCount).Obj()
		if topologyKey != "" {
			pg.Spec.SchedulingConstraints = &schedulingv1beta1.PodGroupSchedulingConstraints{
				Topology: []schedulingv1beta1.TopologyConstraint{
					{Key: topologyKey},
				},
			}
		}
		var pods []*v1.Pod
		for i := 0; i < numPods; i++ {
			p := st.MakePod().Name(fmt.Sprintf("%s-pod-%d", name, i)).PodGroupName(name).Obj()
			p.Spec = *spec.DeepCopy()
			pods = append(pods, p)
		}
		return &framework.PodGroupInfo{
			GenericPodGroup: fwk.NewGenericPodGroup(pg),
			UnscheduledPods: pods,
		}
	}

	makeCPGInfo := func(name string, topologyKey string, children []*framework.PodGroupInfo) *framework.PodGroupInfo {
		wrapper := st.MakeCompositePodGroup().Name(name)
		if topologyKey != "" {
			wrapper = wrapper.TopologyKey(topologyKey)
		}
		return &framework.PodGroupInfo{
			GenericPodGroup: fwk.NewGenericCompositePodGroup(wrapper.Obj()),
			Children:        children,
		}
	}

	tests := []struct {
		name                               string
		ignorePreferredTermsOfExistingPods *bool
		children                           func() []*framework.PodGroupInfo
		want                               bool
	}{
		{
			name: "identical child pod groups",
			children: func() []*framework.PodGroupInfo {
				return []*framework.PodGroupInfo{
					makePGInfo("child-0", basePodSpec(), 2, 2, "rack"),
					makePGInfo("child-1", basePodSpec(), 2, 2, "rack"),
				}
			},
			want: true,
		},
		{
			name: "single child",
			children: func() []*framework.PodGroupInfo {
				return []*framework.PodGroupInfo{
					makePGInfo("child-0", basePodSpec(), 2, 2, "rack"),
				}
			},
			want: false,
		},
		{
			name: "mixed composite and leaf children",
			children: func() []*framework.PodGroupInfo {
				cpg := st.MakeCompositePodGroup().Name("cpg-child").Obj()
				return []*framework.PodGroupInfo{
					{GenericPodGroup: fwk.NewGenericCompositePodGroup(cpg)},
					makePGInfo("child-1", basePodSpec(), 2, 2, "rack"),
				}
			},
			want: false,
		},
		{
			name: "zero unscheduled pods",
			children: func() []*framework.PodGroupInfo {
				return []*framework.PodGroupInfo{
					makePGInfo("child-0", basePodSpec(), 0, 2, "rack"),
					makePGInfo("child-1", basePodSpec(), 0, 2, "rack"),
				}
			},
			want: false,
		},
		{
			name: "different unscheduled pod counts",
			children: func() []*framework.PodGroupInfo {
				return []*framework.PodGroupInfo{
					makePGInfo("child-0", basePodSpec(), 2, 2, "rack"),
					makePGInfo("child-1", basePodSpec(), 3, 2, "rack"),
				}
			},
			want: false,
		},
		{
			name: "different gang minCount",
			children: func() []*framework.PodGroupInfo {
				return []*framework.PodGroupInfo{
					makePGInfo("child-0", basePodSpec(), 2, 1, "rack"),
					makePGInfo("child-1", basePodSpec(), 2, 2, "rack"),
				}
			},
			want: false,
		},
		{
			name: "different topology constraints",
			children: func() []*framework.PodGroupInfo {
				return []*framework.PodGroupInfo{
					makePGInfo("child-0", basePodSpec(), 2, 2, "rack"),
					makePGInfo("child-1", basePodSpec(), 2, 2, "zone"),
				}
			},
			want: false,
		},
		{
			name: "different node selector",
			children: func() []*framework.PodGroupInfo {
				specB := basePodSpec()
				specB.NodeSelector = map[string]string{"zone": "us-west-1"}
				return []*framework.PodGroupInfo{
					makePGInfo("child-0", basePodSpec(), 2, 2, "rack"),
					makePGInfo("child-1", specB, 2, 2, "rack"),
				}
			},
			want: false,
		},
		{
			name: "different tolerations",
			children: func() []*framework.PodGroupInfo {
				specB := basePodSpec()
				specB.Tolerations = nil
				return []*framework.PodGroupInfo{
					makePGInfo("child-0", basePodSpec(), 2, 2, "rack"),
					makePGInfo("child-1", specB, 2, 2, "rack"),
				}
			},
			want: false,
		},
		{
			name: "different CPU requests",
			children: func() []*framework.PodGroupInfo {
				specB := basePodSpec()
				specB.Containers[0].Resources.Requests[v1.ResourceCPU] = resource.MustParse("4")
				return []*framework.PodGroupInfo{
					makePGInfo("child-0", basePodSpec(), 2, 2, "rack"),
					makePGInfo("child-1", specB, 2, 2, "rack"),
				}
			},
			want: false,
		},
		{
			name: "different host ports",
			children: func() []*framework.PodGroupInfo {
				specB := basePodSpec()
				specB.Containers[0].Ports = []v1.ContainerPort{{ContainerPort: 8080, HostPort: 9090}}
				return []*framework.PodGroupInfo{
					makePGInfo("child-0", basePodSpec(), 2, 2, "rack"),
					makePGInfo("child-1", specB, 2, 2, "rack"),
				}
			},
			want: false,
		},
		{
			name: "incompatible pods within the same pod group",
			children: func() []*framework.PodGroupInfo {
				pg0 := makePGInfo("child-0", basePodSpec(), 2, 2, "rack")
				pg0.UnscheduledPods[1].Spec.Containers[0].Resources.Requests[v1.ResourceCPU] = resource.MustParse("4")
				pg1 := makePGInfo("child-1", basePodSpec(), 2, 2, "rack")
				pg1.UnscheduledPods[1].Spec.Containers[0].Resources.Requests[v1.ResourceCPU] = resource.MustParse("4")
				return []*framework.PodGroupInfo{pg0, pg1}
			},
			want: false,
		},
		{
			name: "un-signable pods (with PodAffinity)",
			children: func() []*framework.PodGroupInfo {
				specWithAffinity := basePodSpec()
				specWithAffinity.Affinity = &v1.Affinity{
					PodAffinity: &v1.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
							{TopologyKey: "zone"},
						},
					},
				}
				return []*framework.PodGroupInfo{
					makePGInfo("child-0", specWithAffinity, 2, 2, "rack"),
					makePGInfo("child-1", specWithAffinity, 2, 2, "rack"),
				}
			},
			want: false,
		},
		{
			name:                               "different pod labels across pod groups with IgnorePreferredTermsOfExistingPods enabled",
			ignorePreferredTermsOfExistingPods: ptr.To(true),
			children: func() []*framework.PodGroupInfo {
				pg0 := makePGInfo("child-0", basePodSpec(), 2, 2, "rack")
				for _, p := range pg0.UnscheduledPods {
					p.Labels = map[string]string{"podgroup": "child-0"}
				}
				pg1 := makePGInfo("child-1", basePodSpec(), 2, 2, "rack")
				for _, p := range pg1.UnscheduledPods {
					p.Labels = map[string]string{"podgroup": "child-1"}
				}
				return []*framework.PodGroupInfo{pg0, pg1}
			},
			want: true,
		},
		{
			name:                               "different pod labels across pod groups with IgnorePreferredTermsOfExistingPods disabled",
			ignorePreferredTermsOfExistingPods: ptr.To(false),
			children: func() []*framework.PodGroupInfo {
				pg0 := makePGInfo("child-0", basePodSpec(), 2, 2, "rack")
				for _, p := range pg0.UnscheduledPods {
					p.Labels = map[string]string{"podgroup": "child-0"}
				}
				pg1 := makePGInfo("child-1", basePodSpec(), 2, 2, "rack")
				for _, p := range pg1.UnscheduledPods {
					p.Labels = map[string]string{"podgroup": "child-1"}
				}
				return []*framework.PodGroupInfo{pg0, pg1}
			},
			want: false,
		},
		{
			name: "identical composite child pod groups (3-level hierarchy)",
			children: func() []*framework.PodGroupInfo {
				cpg0 := makeCPGInfo("cpg-0", "zone", []*framework.PodGroupInfo{
					makePGInfo("cpg-0-leaf-0", basePodSpec(), 2, 2, "rack"),
					makePGInfo("cpg-0-leaf-1", basePodSpec(), 2, 2, "rack"),
				})
				cpg1 := makeCPGInfo("cpg-1", "zone", []*framework.PodGroupInfo{
					makePGInfo("cpg-1-leaf-0", basePodSpec(), 2, 2, "rack"),
					makePGInfo("cpg-1-leaf-1", basePodSpec(), 2, 2, "rack"),
				})
				return []*framework.PodGroupInfo{cpg0, cpg1}
			},
			want: true,
		},
		{
			name: "composite child pod groups with different topology constraints",
			children: func() []*framework.PodGroupInfo {
				cpg0 := makeCPGInfo("cpg-0", "zone", []*framework.PodGroupInfo{
					makePGInfo("cpg-0-leaf-0", basePodSpec(), 2, 2, "rack"),
				})
				cpg1 := makeCPGInfo("cpg-1", "region", []*framework.PodGroupInfo{
					makePGInfo("cpg-1-leaf-0", basePodSpec(), 2, 2, "rack"),
				})
				return []*framework.PodGroupInfo{cpg0, cpg1}
			},
			want: false,
		},
		{
			name: "composite child pod groups with different child counts",
			children: func() []*framework.PodGroupInfo {
				cpg0 := makeCPGInfo("cpg-0", "zone", []*framework.PodGroupInfo{
					makePGInfo("cpg-0-leaf-0", basePodSpec(), 2, 2, "rack"),
				})
				cpg1 := makeCPGInfo("cpg-1", "zone", []*framework.PodGroupInfo{
					makePGInfo("cpg-1-leaf-0", basePodSpec(), 2, 2, "rack"),
					makePGInfo("cpg-1-leaf-1", basePodSpec(), 2, 2, "rack"),
				})
				return []*framework.PodGroupInfo{cpg0, cpg1}
			},
			want: false,
		},
		{
			name: "composite child pod groups with non-identical grandchild pod groups",
			children: func() []*framework.PodGroupInfo {
				specB := basePodSpec()
				specB.Containers[0].Resources.Requests[v1.ResourceCPU] = resource.MustParse("4")
				cpg0 := makeCPGInfo("cpg-0", "zone", []*framework.PodGroupInfo{
					makePGInfo("cpg-0-leaf-0", basePodSpec(), 2, 2, "rack"),
					makePGInfo("cpg-0-leaf-1", basePodSpec(), 2, 2, "rack"),
				})
				cpg1 := makeCPGInfo("cpg-1", "zone", []*framework.PodGroupInfo{
					makePGInfo("cpg-1-leaf-0", basePodSpec(), 2, 2, "rack"),
					makePGInfo("cpg-1-leaf-1", specB, 2, 2, "rack"),
				})
				return []*framework.PodGroupInfo{cpg0, cpg1}
			},
			want: false,
		},
		{
			name: "composite child pod groups with zero children",
			children: func() []*framework.PodGroupInfo {
				cpg0 := makeCPGInfo("cpg-0", "zone", nil)
				cpg1 := makeCPGInfo("cpg-1", "zone", nil)
				return []*framework.PodGroupInfo{cpg0, cpg1}
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ignorePreferred := true
			if tt.ignorePreferredTermsOfExistingPods != nil {
				ignorePreferred = *tt.ignorePreferredTermsOfExistingPods
			}
			ctx, schedFwk := newSignTestFramework(t, ignorePreferred)
			got := areChildPodGroupsIdentical(ctx, schedFwk, tt.children())
			if got != tt.want {
				t.Errorf("areChildPodGroupsIdentical() = %v, want %v", got, tt.want)
			}
		})
	}
}

// evalTrackingPlacementPlugin records which placements were evaluated, scored, and normalized.
type evalTrackingPlacementPlugin struct {
	fakePlacementPlugin
	mu             sync.Mutex
	evaluations    map[string][]string
	scored         map[string][]string
	normalizeCalls map[string][][]string
	normalizeFn    func(scores []fwk.PlacementScore) *fwk.Status
}

func (p *evalTrackingPlacementPlugin) Filter(ctx context.Context, state fwk.CycleState, pod *v1.Pod, nodeInfo fwk.NodeInfo) *fwk.Status {
	p.mu.Lock()
	p.evaluations[pod.Name] = append(p.evaluations[pod.Name], nodeInfo.Node().Name)
	p.mu.Unlock()
	return p.fakePlacementPlugin.Filter(ctx, state, pod, nodeInfo)
}

func (p *evalTrackingPlacementPlugin) ScorePlacement(ctx context.Context, state fwk.PlacementCycleState, podGroup fwk.PodGroupInfo, placement *fwk.PodGroupAssignments) (int64, *fwk.Status) {
	p.mu.Lock()
	if p.scored != nil {
		p.scored[podGroup.GetName()] = append(p.scored[podGroup.GetName()], placement.Placement.Name)
	}
	p.mu.Unlock()
	return p.fakePlacementPlugin.ScorePlacement(ctx, state, podGroup, placement)
}

func (p *evalTrackingPlacementPlugin) PlacementScoreExtensions() fwk.PlacementScoreExtensions {
	return p
}

func (p *evalTrackingPlacementPlugin) NormalizePlacementScore(ctx context.Context, state fwk.PodGroupCycleState, podGroup fwk.PodGroupInfo, scores []fwk.PlacementScore) *fwk.Status {
	p.mu.Lock()
	if p.normalizeCalls != nil {
		names := make([]string, len(scores))
		for i, s := range scores {
			names[i] = s.Placement.Name
		}
		p.normalizeCalls[podGroup.GetName()] = append(p.normalizeCalls[podGroup.GetName()], names)
	}
	p.mu.Unlock()
	if p.normalizeFn != nil {
		return p.normalizeFn(scores)
	}
	return nil
}

func TestCPGPlacementOptimization_CacheAndEvaluationCount(t *testing.T) {
	nodes := []*v1.Node{
		st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "16", v1.ResourcePods: "10"}).Obj(),
		st.MakeNode().Name("node2").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "16", v1.ResourcePods: "10"}).Obj(),
		st.MakeNode().Name("node3").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "16", v1.ResourcePods: "10"}).Obj(),
		st.MakeNode().Name("node4").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "16", v1.ResourcePods: "10"}).Obj(),
	}

	makeQueuedPodInfo := func(name, pgName string, cpuReq string) (*v1.Pod, *framework.QueuedPodInfo) {
		p := st.MakePod().Name(name).UID(name).PodGroupName(pgName).
			Labels(map[string]string{"podgroup": pgName}).
			Req(map[v1.ResourceName]string{v1.ResourceCPU: cpuReq}).Obj()
		pInfo, err := framework.NewPodInfo(p)
		if err != nil {
			t.Fatalf("Failed to create pod info: %v", err)
		}
		return p, &framework.QueuedPodInfo{PodInfo: pInfo}
	}

	type testCase struct {
		name                   string
		enableFeatureGate      bool
		numChildren            int
		nonIdenticalChildren   bool
		incompatibleIntraPG    bool
		podPerNode             bool
		injectedFilterStatus   map[string]*fwk.Status
		customChildPlacements  map[string][]string
		customChildScores      map[string]map[string]int64
		normalizeFn            func(scores []fwk.PlacementScore) *fwk.Status
		expectedHosts          map[string]string
		expectedMaxEvaluations map[string]int
		expectedScoredCount    map[string]int
		expectedNormalizeSizes map[string][]int
	}

	tests := []testCase{
		{
			name:              "2 identical children - spread to second placement (P_last full)",
			enableFeatureGate: true,
			numChildren:       2,
			podPerNode:        true,
			expectedHosts: map[string]string{
				"p1": "node1",
				"p2": "node2",
			},
			expectedMaxEvaluations: map[string]int{
				"p1": 4, // child 1 evaluates all 4 placements
				"p2": 2, // child 2 re-evaluates P_last (node1) and validates top candidate (node2)
			},
			expectedScoredCount: map[string]int{
				"pg1": 4, // child 1 scores all 4 placements
				"pg2": 0, // P_last (node1) is infeasible so no raw scoring is needed for pg2
			},
			expectedNormalizeSizes: map[string][]int{
				"pg1": {4},
				"pg2": {3}, // remaining 3 cached placements are re-normalized
			},
		},
		{
			name:              "2 identical children - colocation on P_last",
			enableFeatureGate: true,
			numChildren:       2,
			podPerNode:        false,
			expectedHosts: map[string]string{
				"p1": "node1",
				"p2": "node1",
			},
			expectedMaxEvaluations: map[string]int{
				"p1": 4, // child 1 evaluates all 4 placements
				"p2": 1, // child 2 re-evaluates P_last (node1) and accepts directly
			},
			expectedScoredCount: map[string]int{
				"pg1": 4, // child 1 scores all 4 placements
				"pg2": 1, // child 2 only recalculates raw score for P_last (placement1)
			},
			expectedNormalizeSizes: map[string][]int{
				"pg1": {4},
				"pg2": {4}, // all 4 placements are re-normalized using cached raw scores for placement2..4
			},
		},
		{
			name:              "2 identical children - P_last remains feasible but raw score drops below cached placement after normalization",
			enableFeatureGate: true,
			numChildren:       2,
			podPerNode:        false,
			customChildScores: map[string]map[string]int64{
				"pg1": {
					"placement1": 200,
					"placement2": 150,
					"placement3": 100,
					"placement4": 50,
				},
				"pg2": {
					"placement1": 120, // P_last raw score drops below placement2's cached raw score (150)
					"placement2": 150,
					"placement3": 100,
					"placement4": 50,
				},
			},
			normalizeFn: func(scores []fwk.PlacementScore) *fwk.Status {
				var maxScore int64
				for _, s := range scores {
					if s.Score > maxScore {
						maxScore = s.Score
					}
				}
				if maxScore > 0 {
					for i := range scores {
						scores[i].Score = scores[i].Score * fwk.MaxScore / maxScore
					}
				}
				return nil
			},
			expectedHosts: map[string]string{
				"p1": "node1",
				"p2": "node2",
			},
			expectedMaxEvaluations: map[string]int{
				"p1": 4,
				"p2": 2, // evaluates P_last (node1), re-scores P_last, re-normalizes all 4, then validates node2
			},
			expectedScoredCount: map[string]int{
				"pg1": 4,
				"pg2": 1, // only P_last (placement1) has ScorePlacement called
			},
			expectedNormalizeSizes: map[string][]int{
				"pg1": {4},
				"pg2": {4},
			},
		},
		{
			name:              "2 identical children - overlapping placements re-evaluated and re-scored without duplicate evaluation",
			enableFeatureGate: true,
			numChildren:       2,
			podPerNode:        true,
			customChildPlacements: map[string][]string{
				"placement1": {"node1", "node2"},
				"placement2": {"node2", "node3"}, // overlaps with placement1 on node2
				"placement3": {"node3", "node4"}, // disjoint from placement1
				"placement4": {"node4"},          // disjoint from placement1
			},
			customChildScores: map[string]map[string]int64{
				"pg1": {
					"placement1": 100,
					"placement2": 80,
					"placement3": 60,
					"placement4": 40,
				},
				"pg2": {
					"placement1": 50, // drops after node1 is used
					"placement2": 90, // overlapping placement2 is re-scored and becomes best
					"placement3": 60,
					"placement4": 40,
				},
			},
			expectedHosts: map[string]string{
				"p1": "node1",
				"p2": "node2",
			},
			expectedMaxEvaluations: map[string]int{
				"p1": 8,
				"p2": 4, // evaluates placement1 (2 nodes) + placement2 (2 nodes), then reuses placement2 result without re-evaluating!
			},
			expectedScoredCount: map[string]int{
				"pg1": 4,
				"pg2": 2, // ScorePlacement called only for placement1 and overlapping placement2 (not placement3 or placement4)
			},
			expectedNormalizeSizes: map[string][]int{
				"pg1": {4},
				"pg2": {4}, // all 4 feasible placements are re-normalized
			},
		},
		{
			name:              "2 identical children - overlapping placement becomes infeasible and is invalidated before selecting non-overlapping candidate",
			enableFeatureGate: true,
			numChildren:       2,
			podPerNode:        true,
			customChildPlacements: map[string][]string{
				"placement1": {"node1"},
				"placement2": {"node1"}, // overlaps with placement1 on node1, so also becomes infeasible after p1 uses node1
				"placement3": {"node3"}, // disjoint from placement1
				"placement4": {"node4"}, // disjoint from placement1
			},
			expectedHosts: map[string]string{
				"p1": "node1",
				"p2": "node3",
			},
			expectedMaxEvaluations: map[string]int{
				"p1": 4,
				"p2": 3, // evaluates placement1 (infeasible) + overlapping placement2 (infeasible), then validates non-overlapping placement3 (feasible)
			},
			expectedScoredCount: map[string]int{
				"pg1": 4,
				"pg2": 0, // both overlapping placements (placement1, placement2) are infeasible
			},
			expectedNormalizeSizes: map[string][]int{
				"pg1": {4},
				"pg2": {2}, // remaining 2 feasible placements (placement3, placement4) are re-normalized
			},
		},
		{
			name:              "2 identical children - validation failure triggers fallback",
			enableFeatureGate: true,
			numChildren:       2,
			podPerNode:        true,
			injectedFilterStatus: map[string]*fwk.Status{
				// node2 fails filter for p2, forcing validation failure and fallback to node3
				"node2": fwk.NewStatus(fwk.Unschedulable, "node2 temporarily unavailable"),
			},
			expectedHosts: map[string]string{
				"p1": "node1",
				"p2": "node3", // falls back to next best placement
			},
			expectedMaxEvaluations: map[string]int{
				"p1": 4,
				"p2": 6, // 2 prior checks (P_last + candidate) + 4 in fallback podGroupSchedulingPlacementAlgorithm
			},
		},
		{
			name:              "3 identical children - progressive cache update",
			enableFeatureGate: true,
			numChildren:       3,
			podPerNode:        true,
			expectedHosts: map[string]string{
				"p1": "node1",
				"p2": "node2",
				"p3": "node3",
			},
			expectedMaxEvaluations: map[string]int{
				"p1": 4,
				"p2": 2, // checks node1, schedules on node2
				"p3": 2, // checks node2, schedules on node3
			},
		},
		{
			name:              "Feature gate disabled - all placements evaluated for every child",
			enableFeatureGate: false,
			numChildren:       2,
			podPerNode:        true,
			expectedHosts: map[string]string{
				"p1": "node1",
				"p2": "node2",
			},
			expectedMaxEvaluations: map[string]int{
				"p1": 4,
				"p2": 4, // without optimization, child 2 evaluates all placements
			},
		},
		{
			name:                 "Non-identical children - optimization disabled",
			enableFeatureGate:    true,
			numChildren:          2,
			nonIdenticalChildren: true,
			podPerNode:           true,
			expectedHosts: map[string]string{
				"p1": "node1",
				"p2": "node2",
			},
			expectedMaxEvaluations: map[string]int{
				"p1": 4,
				"p2": 4, // non-identical children bypass cache
			},
		},
		{
			name:                "Incompatible pods within child pod group - optimization disabled",
			enableFeatureGate:   true,
			numChildren:         2,
			incompatibleIntraPG: true,
			podPerNode:          false,
			expectedHosts: map[string]string{
				"p1":       "node1",
				"p1-extra": "node1",
				"p2":       "node1",
				"p2-extra": "node1",
			},
			expectedMaxEvaluations: map[string]int{
				"p1": 4,
				"p2": 4, // incompatible pods within a PodGroup bypass cache
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGatesDuringTest(t, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
				features.TopologyAwareWorkloadScheduling:            true,
				features.GenericWorkload:                            true,
				features.CompositePodGroup:                          true,
				features.TopologyAwareCompositePodGroupOptimization: tt.enableFeatureGate,
			})

			logger, ctx := ktesting.NewTestContext(t)
			informerFactory := informers.NewSharedInformerFactory(clientsetfake.NewClientset(), 0)
			queue := internalqueue.NewSchedulingQueue(nil, informerFactory)

			cpg := st.MakeCompositePodGroup().Name("cpg").Obj()
			var (
				childPGs       []*schedulingv1beta1.PodGroup
				childPGInfos   []*framework.PodGroupInfo
				queuedPodInfos []*framework.QueuedPodInfo
				pods           []*v1.Pod
			)

			for i := 1; i <= tt.numChildren; i++ {
				pgName := fmt.Sprintf("pg%d", i)
				podName := fmt.Sprintf("p%d", i)
				pg := st.MakePodGroup().Name(pgName).ParentCompositePodGroup("cpg").Obj()
				pg.CreationTimestamp = metav1.NewTime(time.UnixMilli(int64(i)))
				childPGs = append(childPGs, pg)

				p, qpInfo := makeQueuedPodInfo(podName, pgName, "1")
				pods = append(pods, p)
				queuedPodInfos = append(queuedPodInfos, qpInfo)

				pgInfo := &framework.PodGroupInfo{
					GenericPodGroup: fwk.NewGenericPodGroup(pg),
					UnscheduledPods: []*v1.Pod{p},
				}
				if tt.nonIdenticalChildren && i == 2 {
					// Add an extra pod to child 2 to break equivalence
					extraPod, extraQpInfo := makeQueuedPodInfo("p2-extra", pgName, "1")
					pods = append(pods, extraPod)
					queuedPodInfos = append(queuedPodInfos, extraQpInfo)
					pgInfo.UnscheduledPods = append(pgInfo.UnscheduledPods, extraPod)
				}
				if tt.incompatibleIntraPG {
					// Add a second pod with different CPU requests to each PodGroup
					extraPod, extraQpInfo := makeQueuedPodInfo(fmt.Sprintf("p%d-extra", i), pgName, "2")
					pods = append(pods, extraPod)
					queuedPodInfos = append(queuedPodInfos, extraQpInfo)
					pgInfo.UnscheduledPods = append(pgInfo.UnscheduledPods, extraPod)
				}
				childPGInfos = append(childPGInfos, pgInfo)
			}

			rootPGInfo := &framework.PodGroupInfo{
				GenericPodGroup: fwk.NewGenericCompositePodGroup(cpg),
				Children:        childPGInfos,
			}

			// Generate 4 candidate placements, one per node by default
			childPlacements := map[string][]string{
				"placement1": {nodes[0].Name},
				"placement2": {nodes[1].Name},
				"placement3": {nodes[2].Name},
				"placement4": {nodes[3].Name},
			}
			if tt.customChildPlacements != nil {
				childPlacements = tt.customChildPlacements
			}
			generatePlacementsResult := map[fwk.EntityKey]map[string][]string{
				rootPGInfo.GetKey(): {
					"placement1": {nodes[0].Name, nodes[1].Name, nodes[2].Name, nodes[3].Name},
				},
			}
			scorePlacementsResult := map[fwk.EntityKey]map[string]int64{
				rootPGInfo.GetKey(): {
					"placement1": 100,
				},
			}
			for _, cInfo := range childPGInfos {
				generatePlacementsResult[cInfo.GetKey()] = childPlacements
				if custom, ok := tt.customChildScores[cInfo.GetName()]; ok {
					scorePlacementsResult[cInfo.GetKey()] = custom
				} else {
					scorePlacementsResult[cInfo.GetKey()] = map[string]int64{
						"placement1": 100,
						"placement2": 80,
						"placement3": 60,
						"placement4": 40,
					}
				}
			}

			trackingPlugin := &evalTrackingPlacementPlugin{
				fakePlacementPlugin: fakePlacementPlugin{
					name:                     "TrackingPlacementPlugin",
					generatePlacementsResult: generatePlacementsResult,
					scorePlacementsResult:    scorePlacementsResult,
					podPerNode:               tt.podPerNode,
					reservedNodes:            sets.New[string](),
					filterStatus:             tt.injectedFilterStatus,
				},
				evaluations:    make(map[string][]string),
				scored:         make(map[string][]string),
				normalizeCalls: make(map[string][][]string),
				normalizeFn:    tt.normalizeFn,
			}

			orderedPlugin := &orderedPlacementPlugin{&trackingPlugin.fakePlacementPlugin}
			gangPluginFactory := func(ctx context.Context, obj runtime.Object, handle fwk.Handle) (fwk.Plugin, error) {
				return gangscheduling.New(ctx, obj, handle, feature.Features{EnableTopologyAwareWorkloadScheduling: true})
			}
			fts := feature.NewSchedulerFeaturesFromGates(utilfeature.DefaultFeatureGate)
			interPodAffinityFactory := func(ctx context.Context, _ runtime.Object, h fwk.Handle) (fwk.Plugin, error) {
				return interpodaffinity.New(ctx, &schedulerapi.InterPodAffinityArgs{
					HardPodAffinityWeight:              1,
					IgnorePreferredTermsOfExistingPods: true,
				}, h, fts)
			}

			registry := []tf.RegisterPluginFunc{
				tf.RegisterPlacementGeneratePlugin(orderedPlugin.Name(), func(_ context.Context, _ runtime.Object, _ fwk.Handle) (fwk.Plugin, error) {
					return orderedPlugin, nil
				}),
				tf.RegisterPlacementScorePlugin(trackingPlugin.Name(), func(_ context.Context, _ runtime.Object, _ fwk.Handle) (fwk.Plugin, error) {
					return trackingPlugin, nil
				}, 1),
				tf.RegisterFilterPlugin(trackingPlugin.Name(), func(_ context.Context, _ runtime.Object, _ fwk.Handle) (fwk.Plugin, error) {
					return trackingPlugin, nil
				}),
				tf.RegisterReservePlugin(trackingPlugin.Name(), func(_ context.Context, _ runtime.Object, _ fwk.Handle) (fwk.Plugin, error) {
					return trackingPlugin, nil
				}),
				tf.RegisterPluginAsExtensions(noderesources.Name, frameworkruntime.FactoryAdapter(fts, noderesources.NewFit), "PreFilter", "Filter"),
				tf.RegisterPluginAsExtensions(interpodaffinity.Name, interPodAffinityFactory, "PreFilter", "Filter"),
				tf.RegisterPlacementFeasiblePlugin(gangscheduling.Name, gangPluginFactory),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			}

			snapshot := internalcache.NewEmptySnapshot()
			schedFwk, err := tf.NewFramework(ctx, registry, "test-scheduler",
				frameworkruntime.WithInformerFactory(informerFactory),
				frameworkruntime.WithSnapshotSharedLister(snapshot),
				frameworkruntime.WithPodNominator(queue),
			)
			if err != nil {
				t.Fatalf("Failed to create framework: %v", err)
			}

			cache := internalcache.New(ctx, nil, true, true)
			for _, node := range nodes {
				cache.AddNode(logger, node)
			}
			cache.AddGenericPodGroup(fwk.NewGenericCompositePodGroup(cpg))
			for _, pg := range childPGs {
				cache.AddGenericPodGroup(fwk.NewGenericPodGroup(pg))
			}
			for _, p := range pods {
				cache.AddPodGroupMember(p)
			}

			sched := &Scheduler{
				Cache:            cache,
				nodeInfoSnapshot: snapshot,
				SchedulingQueue:  queue,
				Profiles:         profile.Map{"test-scheduler": schedFwk},
			}
			initTestAlgorithm(t, sched)

			if err := sched.Cache.UpdateSnapshot(logger, sched.nodeInfoSnapshot); err != nil {
				t.Fatalf("Failed to update snapshot: %v", err)
			}

			cpgInfo := newQueuedPodGroupInfo(rootPGInfo, queuedPodInfos...)
			results := sched.runRootSchedulingAlgorithm(ctx, schedFwk, framework.NewCycleState(), cpgInfo)

			// Verify pod host assignments
			gotHosts := make(map[string]string)
			for _, res := range results {
				for _, podRes := range res.podResults {
					if podRes.status.IsSuccess() {
						gotHosts[podRes.podInfo.Pod.Name] = podRes.scheduleResult.SuggestedHost
					}
				}
			}
			for podName, wantHost := range tt.expectedHosts {
				if gotHosts[podName] != wantHost {
					t.Errorf("Pod %s scheduled on host %q, want %q", podName, gotHosts[podName], wantHost)
				}
			}

			// Verify placement evaluation counts
			for podName, maxEval := range tt.expectedMaxEvaluations {
				actualEvals := len(trackingPlugin.evaluations[podName])
				if actualEvals > maxEval {
					t.Errorf("Pod %s had %d placement evaluations %v, wanted <= %d",
						podName, actualEvals, trackingPlugin.evaluations[podName], maxEval)
				}
			}

			// Verify ScorePlacement call counts
			for pgName, wantScored := range tt.expectedScoredCount {
				gotScored := len(trackingPlugin.scored[pgName])
				if gotScored != wantScored {
					t.Errorf("PodGroup %s had %d ScorePlacement calls %v, want %d",
						pgName, gotScored, trackingPlugin.scored[pgName], wantScored)
				}
			}

			// Verify NormalizePlacementScore call sizes
			for pgName, wantSizes := range tt.expectedNormalizeSizes {
				calls := trackingPlugin.normalizeCalls[pgName]
				if len(calls) != len(wantSizes) {
					t.Errorf("PodGroup %s had %d NormalizePlacementScore calls %v, want %d",
						pgName, len(calls), calls, len(wantSizes))
					continue
				}
				for i, wantSize := range wantSizes {
					if len(calls[i]) != wantSize {
						t.Errorf("PodGroup %s NormalizePlacementScore call %d had %d placements %v, want %d",
							pgName, i, len(calls[i]), calls[i], wantSize)
					}
				}
			}
		})
	}
}

func TestCPGPlacementOptimization_ThreeLevelCompositeChildrenCache(t *testing.T) {
	// 8 nodes across 4 zones (2 nodes per zone).
	nodes := []*v1.Node{
		st.MakeNode().Name("z1-n1").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "16", v1.ResourcePods: "10"}).Obj(),
		st.MakeNode().Name("z1-n2").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "16", v1.ResourcePods: "10"}).Obj(),
		st.MakeNode().Name("z2-n1").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "16", v1.ResourcePods: "10"}).Obj(),
		st.MakeNode().Name("z2-n2").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "16", v1.ResourcePods: "10"}).Obj(),
		st.MakeNode().Name("z3-n1").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "16", v1.ResourcePods: "10"}).Obj(),
		st.MakeNode().Name("z3-n2").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "16", v1.ResourcePods: "10"}).Obj(),
		st.MakeNode().Name("z4-n1").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "16", v1.ResourcePods: "10"}).Obj(),
		st.MakeNode().Name("z4-n2").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "16", v1.ResourcePods: "10"}).Obj(),
	}

	makeQueuedPodInfo := func(name, pgName, cpuReq string) (*v1.Pod, *framework.QueuedPodInfo) {
		p := st.MakePod().Name(name).UID(name).PodGroupName(pgName).
			Labels(map[string]string{"podgroup": pgName}).
			Req(map[v1.ResourceName]string{v1.ResourceCPU: cpuReq}).Obj()
		pInfo, err := framework.NewPodInfo(p)
		if err != nil {
			t.Fatalf("Failed to create pod info: %v", err)
		}
		return p, &framework.QueuedPodInfo{PodInfo: pInfo}
	}

	tests := []struct {
		name                   string
		enableFeatureGate      bool
		nonIdenticalChildCPGs  bool
		expectedHosts          map[string]string
		expectedMaxEvaluations map[string]int
	}{
		{
			name:              "3-level CPG with 2 identical child CPGs - caches both composite and leaf placements",
			enableFeatureGate: true,
			expectedHosts: map[string]string{
				"p-1-1": "z1-n1",
				"p-1-2": "z1-n2",
				"p-2-1": "z2-n1",
				"p-2-2": "z2-n2",
			},
			expectedMaxEvaluations: map[string]int{
				// child-cpg-1 evaluates all 4 zones (z1..z4):
				// p-1-1 evaluates 2 nodes in each of the 4 zones = 8 evaluations
				"p-1-1": 8,
				// p-1-2 uses leaf cache inside each zone: in z1..z4, checks n1 (full) + validates n2 = 2 per zone * 4 zones = 8 evaluations
				"p-1-2": 8,
				// child-cpg-2 uses composite cache: only evaluates z1 (fails on p-2-1) and validates z2 (succeeds)!
				// p-2-1 evaluates 2 nodes in z1 (fails) + 2 nodes in z2 = 4 evaluations (skips z3 and z4 completely!)
				"p-2-1": 4,
				// p-2-2 only runs in z2 (since z1 failed on p-2-1) and uses leaf cache (checks z2-n1 + validates z2-n2) = 2 evaluations!
				"p-2-2": 2,
			},
		},
		{
			name:              "3-level CPG with feature gate disabled - evaluates all zones for second child CPG",
			enableFeatureGate: false,
			expectedHosts: map[string]string{
				"p-1-1": "z1-n1",
				"p-1-2": "z1-n2",
				"p-2-1": "z2-n1",
				"p-2-2": "z2-n2",
			},
			expectedMaxEvaluations: map[string]int{
				"p-1-1": 8,
				"p-1-2": 8,
				// Without optimization, child-cpg-2 evaluates all 4 zones (z1 fails, z2..z4 evaluated) = 8 evaluations for p-2-1
				"p-2-1": 8,
				"p-2-2": 6,
			},
		},
		{
			name:                  "3-level CPG with non-identical child CPGs - composite cache bypassed",
			enableFeatureGate:     true,
			nonIdenticalChildCPGs: true,
			expectedHosts: map[string]string{
				"p-1-1": "z1-n1",
				"p-1-2": "z1-n2",
				"p-2-1": "z2-n1",
				"p-2-2": "z2-n2",
			},
			expectedMaxEvaluations: map[string]int{
				"p-1-1": 8,
				"p-1-2": 8,
				// Non-identical child CPGs bypass composite cache (evaluates all 4 zones = 8 evaluations for p-2-1)
				"p-2-1": 8,
				"p-2-2": 6,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGatesDuringTest(t, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
				features.TopologyAwareWorkloadScheduling:            true,
				features.GenericWorkload:                            true,
				features.CompositePodGroup:                          true,
				features.TopologyAwareCompositePodGroupOptimization: tt.enableFeatureGate,
			})

			logger, ctx := ktesting.NewTestContext(t)
			informerFactory := informers.NewSharedInformerFactory(clientsetfake.NewClientset(), 0)
			queue := internalqueue.NewSchedulingQueue(nil, informerFactory)

			rootCPG := st.MakeCompositePodGroup().Name("root-cpg").MinGroupCount(2).Obj()
			var (
				childCPGs      []*schedulingv1alpha3.CompositePodGroup
				childCPGInfos  []*framework.PodGroupInfo
				leafPGs        []*schedulingv1beta1.PodGroup
				queuedPodInfos []*framework.QueuedPodInfo
				pods           []*v1.Pod
			)

			for i := 1; i <= 2; i++ {
				cpgName := fmt.Sprintf("child-cpg-%d", i)
				cpg := st.MakeCompositePodGroup().Name(cpgName).ParentCompositePodGroup("root-cpg").MinGroupCount(2).Obj()
				cpg.CreationTimestamp = metav1.NewTime(time.UnixMilli(int64(i)))
				childCPGs = append(childCPGs, cpg)

				var leafPGInfos []*framework.PodGroupInfo
				for j := 1; j <= 2; j++ {
					pgName := fmt.Sprintf("pg-%d-%d", i, j)
					podName := fmt.Sprintf("p-%d-%d", i, j)
					pg := st.MakePodGroup().Name(pgName).ParentCompositePodGroup(cpgName).Obj()
					pg.CreationTimestamp = metav1.NewTime(time.UnixMilli(int64(i*10 + j)))
					leafPGs = append(leafPGs, pg)

					cpuReq := "1"
					if tt.nonIdenticalChildCPGs && i == 2 {
						cpuReq = "2"
					}
					p, qpInfo := makeQueuedPodInfo(podName, pgName, cpuReq)
					pods = append(pods, p)
					queuedPodInfos = append(queuedPodInfos, qpInfo)

					leafPGInfos = append(leafPGInfos, &framework.PodGroupInfo{
						GenericPodGroup: fwk.NewGenericPodGroup(pg),
						UnscheduledPods: []*v1.Pod{p},
					})
				}

				childCPGInfos = append(childCPGInfos, &framework.PodGroupInfo{
					GenericPodGroup: fwk.NewGenericCompositePodGroup(cpg),
					Children:        leafPGInfos,
				})
			}

			rootPGInfo := &framework.PodGroupInfo{
				GenericPodGroup: fwk.NewGenericCompositePodGroup(rootCPG),
				Children:        childCPGInfos,
			}

			allNodeNames := make([]string, len(nodes))
			for i, n := range nodes {
				allNodeNames[i] = n.Name
			}

			generatePlacementsResult := map[fwk.EntityKey]map[string][]string{
				rootPGInfo.GetKey(): {
					"root-placement": allNodeNames,
				},
			}
			scorePlacementsResult := map[fwk.EntityKey]map[string]int64{
				rootPGInfo.GetKey(): {
					"root-placement": 100,
				},
			}

			zonePlacements := map[string][]string{
				"placement1": {"z1-n1", "z1-n2"},
				"placement2": {"z2-n1", "z2-n2"},
				"placement3": {"z3-n1", "z3-n2"},
				"placement4": {"z4-n1", "z4-n2"},
			}
			zoneScores := map[string]int64{
				"placement1": 100,
				"placement2": 80,
				"placement3": 60,
				"placement4": 40,
			}

			for _, cpgInfo := range childCPGInfos {
				generatePlacementsResult[cpgInfo.GetKey()] = zonePlacements
				scorePlacementsResult[cpgInfo.GetKey()] = zoneScores
				for _, leafInfo := range cpgInfo.Children {
					generatePlacementsResult[leafInfo.GetKey()] = map[string][]string{
						"placement1": {"z1-n1"},
						"placement2": {"z1-n2"},
						"placement3": {"z2-n1"},
						"placement4": {"z2-n2"},
						"placement5": {"z3-n1"},
						"placement6": {"z3-n2"},
						"placement7": {"z4-n1"},
						"placement8": {"z4-n2"},
					}
					scorePlacementsResult[leafInfo.GetKey()] = map[string]int64{
						"placement1": 100,
						"placement2": 90,
						"placement3": 100,
						"placement4": 90,
						"placement5": 100,
						"placement6": 90,
						"placement7": 100,
						"placement8": 90,
					}
				}
			}

			trackingPlugin := &evalTrackingPlacementPlugin{
				fakePlacementPlugin: fakePlacementPlugin{
					name:                     "TrackingPlacementPlugin",
					generatePlacementsResult: generatePlacementsResult,
					scorePlacementsResult:    scorePlacementsResult,
					podPerNode:               true,
					reservedNodes:            sets.New[string](),
				},
				evaluations: make(map[string][]string),
			}

			orderedPlugin := &orderedPlacementPlugin{&trackingPlugin.fakePlacementPlugin}
			gangPluginFactory := func(ctx context.Context, obj runtime.Object, handle fwk.Handle) (fwk.Plugin, error) {
				return gangscheduling.New(ctx, obj, handle, feature.Features{EnableTopologyAwareWorkloadScheduling: true})
			}
			fts := feature.NewSchedulerFeaturesFromGates(utilfeature.DefaultFeatureGate)
			interPodAffinityFactory := func(ctx context.Context, _ runtime.Object, h fwk.Handle) (fwk.Plugin, error) {
				return interpodaffinity.New(ctx, &schedulerapi.InterPodAffinityArgs{
					HardPodAffinityWeight:              1,
					IgnorePreferredTermsOfExistingPods: true,
				}, h, fts)
			}

			registry := []tf.RegisterPluginFunc{
				tf.RegisterPlacementGeneratePlugin(orderedPlugin.Name(), func(_ context.Context, _ runtime.Object, _ fwk.Handle) (fwk.Plugin, error) {
					return orderedPlugin, nil
				}),
				tf.RegisterPlacementScorePlugin(trackingPlugin.Name(), func(_ context.Context, _ runtime.Object, _ fwk.Handle) (fwk.Plugin, error) {
					return trackingPlugin, nil
				}, 1),
				tf.RegisterFilterPlugin(trackingPlugin.Name(), func(_ context.Context, _ runtime.Object, _ fwk.Handle) (fwk.Plugin, error) {
					return trackingPlugin, nil
				}),
				tf.RegisterReservePlugin(trackingPlugin.Name(), func(_ context.Context, _ runtime.Object, _ fwk.Handle) (fwk.Plugin, error) {
					return trackingPlugin, nil
				}),
				tf.RegisterPluginAsExtensions(noderesources.Name, frameworkruntime.FactoryAdapter(fts, noderesources.NewFit), "PreFilter", "Filter"),
				tf.RegisterPluginAsExtensions(interpodaffinity.Name, interPodAffinityFactory, "PreFilter", "Filter"),
				tf.RegisterPlacementFeasiblePlugin(gangscheduling.Name, gangPluginFactory),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			}

			snapshot := internalcache.NewEmptySnapshot()
			schedFwk, err := tf.NewFramework(ctx, registry, "test-scheduler",
				frameworkruntime.WithInformerFactory(informerFactory),
				frameworkruntime.WithSnapshotSharedLister(snapshot),
				frameworkruntime.WithPodNominator(queue),
			)
			if err != nil {
				t.Fatalf("Failed to create framework: %v", err)
			}

			cache := internalcache.New(ctx, nil, true, true)
			for _, node := range nodes {
				cache.AddNode(logger, node)
			}
			cache.AddGenericPodGroup(fwk.NewGenericCompositePodGroup(rootCPG))
			for _, cpg := range childCPGs {
				cache.AddGenericPodGroup(fwk.NewGenericCompositePodGroup(cpg))
			}
			for _, pg := range leafPGs {
				cache.AddGenericPodGroup(fwk.NewGenericPodGroup(pg))
			}
			for _, p := range pods {
				cache.AddPodGroupMember(p)
			}

			sched := &Scheduler{
				Cache:            cache,
				nodeInfoSnapshot: snapshot,
				SchedulingQueue:  queue,
				Profiles:         profile.Map{"test-scheduler": schedFwk},
			}
			initTestAlgorithm(t, sched)

			if err := sched.Cache.UpdateSnapshot(logger, sched.nodeInfoSnapshot); err != nil {
				t.Fatalf("Failed to update snapshot: %v", err)
			}

			cpgInfo := newQueuedPodGroupInfo(rootPGInfo, queuedPodInfos...)
			results := sched.runRootSchedulingAlgorithm(ctx, schedFwk, framework.NewCycleState(), cpgInfo)

			gotHosts := make(map[string]string)
			for _, res := range results {
				for _, podRes := range res.podResults {
					if podRes.status.IsSuccess() {
						gotHosts[podRes.podInfo.Pod.Name] = podRes.scheduleResult.SuggestedHost
					}
				}
			}
			for podName, wantHost := range tt.expectedHosts {
				if gotHosts[podName] != wantHost {
					t.Errorf("Pod %s scheduled on host %q, want %q", podName, gotHosts[podName], wantHost)
				}
			}

			for podName, maxEval := range tt.expectedMaxEvaluations {
				actualEvals := len(trackingPlugin.evaluations[podName])
				if actualEvals != maxEval {
					t.Errorf("Pod %s had %d placement evaluations %v, wanted %d",
						podName, actualEvals, trackingPlugin.evaluations[podName], maxEval)
				}
			}
		})
	}
}

