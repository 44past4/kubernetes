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
	internalcache "k8s.io/kubernetes/pkg/scheduler/backend/cache"
	internalqueue "k8s.io/kubernetes/pkg/scheduler/backend/queue"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/gangscheduling"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/profile"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
	"k8s.io/utils/ptr"
)

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

	tests := []struct {
		name     string
		children func() []*framework.PodGroupInfo
		want     bool
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
			name: "composite child (non-leaf)",
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
			name: "different priority",
			children: func() []*framework.PodGroupInfo {
				specB := basePodSpec()
				specB.Priority = ptr.To(int32(200))
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
			name: "different container counts",
			children: func() []*framework.PodGroupInfo {
				specB := basePodSpec()
				specB.Containers = append(specB.Containers, v1.Container{Name: "sidecar"})
				return []*framework.PodGroupInfo{
					makePGInfo("child-0", basePodSpec(), 2, 2, "rack"),
					makePGInfo("child-1", specB, 2, 2, "rack"),
				}
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := areChildPodGroupsIdentical(tt.children())
			if got != tt.want {
				t.Errorf("areChildPodGroupsIdentical() = %v, want %v", got, tt.want)
			}
		})
	}
}

// evalTrackingPlacementPlugin records which placements were evaluated for each pod.
type evalTrackingPlacementPlugin struct {
	fakePlacementPlugin
	mu          sync.Mutex
	evaluations map[string][]string
}

func (p *evalTrackingPlacementPlugin) Filter(ctx context.Context, state fwk.CycleState, pod *v1.Pod, nodeInfo fwk.NodeInfo) *fwk.Status {
	p.mu.Lock()
	p.evaluations[pod.Name] = append(p.evaluations[pod.Name], nodeInfo.Node().Name)
	p.mu.Unlock()
	return p.fakePlacementPlugin.Filter(ctx, state, pod, nodeInfo)
}

func TestCPGPlacementOptimization_CacheAndEvaluationCount(t *testing.T) {
	nodes := []*v1.Node{
		st.MakeNode().Name("node1").Obj(),
		st.MakeNode().Name("node2").Obj(),
		st.MakeNode().Name("node3").Obj(),
		st.MakeNode().Name("node4").Obj(),
	}

	makeQueuedPodInfo := func(name, pgName string) (*v1.Pod, *framework.QueuedPodInfo) {
		p := st.MakePod().Name(name).UID(name).PodGroupName(pgName).Obj()
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
		podPerNode             bool
		injectedFilterStatus   map[string]*fwk.Status
		expectedHosts          map[string]string
		expectedMaxEvaluations map[string]int
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

				p, qpInfo := makeQueuedPodInfo(podName, pgName)
				pods = append(pods, p)
				queuedPodInfos = append(queuedPodInfos, qpInfo)

				pgInfo := &framework.PodGroupInfo{
					GenericPodGroup: fwk.NewGenericPodGroup(pg),
					UnscheduledPods: []*v1.Pod{p},
				}
				if tt.nonIdenticalChildren && i == 2 {
					// Add an extra pod to child 2 to break equivalence
					extraPod, extraQpInfo := makeQueuedPodInfo("p2-extra", pgName)
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

			// Generate 4 candidate placements, one per node
			childPlacements := map[string][]string{
				"placement1": {nodes[0].Name},
				"placement2": {nodes[1].Name},
				"placement3": {nodes[2].Name},
				"placement4": {nodes[3].Name},
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
				scorePlacementsResult[cInfo.GetKey()] = map[string]int64{
					"placement1": 100,
					"placement2": 80,
					"placement3": 60,
					"placement4": 40,
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
				evaluations: make(map[string][]string),
			}

			orderedPlugin := &orderedPlacementPlugin{&trackingPlugin.fakePlacementPlugin}
			gangPluginFactory := func(ctx context.Context, obj runtime.Object, handle fwk.Handle) (fwk.Plugin, error) {
				return gangscheduling.New(ctx, obj, handle, feature.Features{EnableTopologyAwareWorkloadScheduling: true})
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
		})
	}
}
