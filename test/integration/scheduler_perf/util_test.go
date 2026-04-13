/*
Copyright 2015 The Kubernetes Authors.

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

package benchmark

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	v1 "k8s.io/api/core/v1"
	schedulingv1alpha2 "k8s.io/api/scheduling/v1alpha2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	ktesting "k8s.io/kubernetes/test/utils/ktesting"
)

func Test_uniqueLVCombos(t *testing.T) {
	type args struct {
		lvs []*labelValues
	}
	tests := []struct {
		name string
		args args
		want []map[string]string
	}{
		{
			name: "empty input",
			args: args{
				lvs: []*labelValues{},
			},
			want: []map[string]string{{}},
		},
		{
			name: "single label, multiple values",
			args: args{
				lvs: []*labelValues{
					{"A", []string{"a1", "a2"}},
				},
			},
			want: []map[string]string{
				{"A": "a1"},
				{"A": "a2"},
			},
		},
		{
			name: "multiple labels, single value each",
			args: args{
				lvs: []*labelValues{
					{"A", []string{"a1"}},
					{"B", []string{"b1"}},
				},
			},
			want: []map[string]string{
				{"A": "a1", "B": "b1"},
			},
		},
		{
			name: "multiple labels, multiple values",
			args: args{
				lvs: []*labelValues{
					{"A", []string{"a1", "a2"}},
					{"B", []string{"b1", "b2"}},
				},
			},
			want: []map[string]string{
				{"A": "a1", "B": "b1"},
				{"A": "a1", "B": "b2"},
				{"A": "a2", "B": "b1"},
				{"A": "a2", "B": "b2"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := uniqueLVCombos(tt.args.lvs); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("uniqueLVCombos() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodGroupLatencyCollector_collect(t *testing.T) {
	baseTime := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	makeTime := func(minutes int) time.Time {
		return baseTime.Add(time.Duration(minutes) * time.Minute)
	}

	tests := []struct {
		name          string
		podgroups     []*schedulingv1alpha2.PodGroup
		pods          []*v1.Pod
		events        []*v1.Event
		expectedItems []DataItem
	}{
		{
			name: "3 pods, minCount 3",

			podgroups: []*schedulingv1alpha2.PodGroup{
				st.MakePodGroup().Name("pg").Namespace("default").MinCount(3).Obj(),
			},
			pods: []*v1.Pod{
				st.MakePod().Name("p0").PodGroupName("pg").Namespace("default").CreationTimestamp(metav1.NewTime(makeTime(0))).Obj(),
				st.MakePod().Name("p1").PodGroupName("pg").Namespace("default").CreationTimestamp(metav1.NewTime(makeTime(1))).Obj(),
				st.MakePod().Name("p2").PodGroupName("pg").Namespace("default").CreationTimestamp(metav1.NewTime(makeTime(2))).Obj(),
			},
			events: []*v1.Event{
				makeScheduledEvent("p0", "default", makeTime(10)),
				makeScheduledEvent("p1", "default", makeTime(11)),
				makeScheduledEvent("p2", "default", makeTime(12)),
			},
			expectedItems: []DataItem{
				{
					Labels: map[string]string{"Metric": "PodGroupSchedulingDuration", "PodGroupKey": "default/pg"},
					Data:   map[string]float64{"Duration": float64(makeTime(12).Sub(makeTime(0)).Milliseconds())},
					Unit:   "ms",
				},
			},
		},
		{
			name: "4 pods, minCount 3",
			podgroups: []*schedulingv1alpha2.PodGroup{
				st.MakePodGroup().Name("pg").Namespace("default").MinCount(3).Obj(),
			},
			pods: []*v1.Pod{
				st.MakePod().Name("p0").PodGroupName("pg").Namespace("default").CreationTimestamp(metav1.NewTime(makeTime(0))).Obj(),
				st.MakePod().Name("p1").PodGroupName("pg").Namespace("default").CreationTimestamp(metav1.NewTime(makeTime(1))).Obj(),
				st.MakePod().Name("p2").PodGroupName("pg").Namespace("default").CreationTimestamp(metav1.NewTime(makeTime(2))).Obj(),
				st.MakePod().Name("p3").PodGroupName("pg").Namespace("default").CreationTimestamp(metav1.NewTime(makeTime(3))).Obj(),
			},
			events: []*v1.Event{
				makeScheduledEvent("p0", "default", makeTime(10)), // Scheduled at 10:10
				makeScheduledEvent("p1", "default", makeTime(11)), // Scheduled at 10:11
				makeScheduledEvent("p2", "default", makeTime(12)), // Scheduled at 10:12 (3rd pod)
				makeScheduledEvent("p3", "default", makeTime(13)), // Scheduled at 10:13
			},
			expectedItems: []DataItem{
				{
					Labels: map[string]string{"Metric": "PodGroupSchedulingDuration", "PodGroupKey": "default/pg"},
					Data:   map[string]float64{"Duration": float64(makeTime(12).Sub(makeTime(0)).Milliseconds())},
					Unit:   "ms",
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			tCtx := ctx.(ktesting.TContext)

			// Setup fake client
			scheme := runtime.NewScheme()
			v1.AddToScheme(scheme)
			schedulingv1alpha2.AddToScheme(scheme)

			var objects []runtime.Object
			for _, pg := range tt.podgroups {
				objects = append(objects, pg)
			}
			fakeClient := fake.NewSimpleClientset(objects...)

			informerFactory := informers.NewSharedInformerFactory(fakeClient, 0)
			podInformer := informerFactory.Core().V1().Pods()
			eventInformer := informerFactory.Core().V1().Events()
			podGroupLister := informerFactory.Scheduling().V1alpha2().PodGroups().Lister()

			collector := newPodGroupLatencyCollector(podInformer, eventInformer, podGroupLister)

			// Start collector (starts informer handler)
			collector.init()
			collector.run(tCtx)
			informerFactory.Start(tCtx.Done())
			informerFactory.WaitForCacheSync(tCtx.Done())

			// Create pods and events
			for _, pod := range tt.pods {
				_, err := fakeClient.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create pod %s: %v", pod.Name, err)
				}
			}
			for _, event := range tt.events {
				_, err := fakeClient.CoreV1().Events(event.Namespace).Create(context.Background(), event, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create event %s: %v", event.Name, err)
				}
			}
			time.Sleep(5 * time.Second)
			items := collector.collect(tCtx)
			if len(items) != len(tt.expectedItems) {
				t.Fatalf("Expected %d DataItems, got %d", len(tt.expectedItems), len(items))
			}
			if diff := cmp.Diff(items, tt.expectedItems, cmpopts.IgnoreUnexported(DataItem{})); diff != "" {
				t.Errorf("DataItems mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func makeScheduledEvent(podName, namespace string, scheduleTime time.Time) *v1.Event {
	return &v1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-scheduled", podName),
			Namespace: namespace,
		},
		InvolvedObject: v1.ObjectReference{
			Kind:      "Pod",
			Name:      podName,
			Namespace: namespace,
		},
		Source: v1.EventSource{
			Component: "test-scheduler",
		},
		Reason:    "Scheduled",
		EventTime: metav1.NewMicroTime(scheduleTime),
	}
}
