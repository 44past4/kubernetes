# PR: Reduce scheduler allocation churn in the pod-group / topology-aware scheduling hot path

> Everything needed to open a single pull request against `kubernetes/kubernetes`.
> Source branch: **`fix/tas-all-combined`** (3 commits) → base **`master`**.

---

## Title

```
scheduler: cut per-node allocations in the filter/score hot path (TAS/pod-group)
```

## Summary

Topology-aware / pod-group scheduling evaluates every pod against one *placement per
topology domain*, re-running the full per-node Filter and Score path for each. That
multiplies per-node allocations by the number of domains (~50 in the benchmark), and the
scheduling goroutine ends up GC-bound: in the `5000Nodes_2250Gangs_9000Pods` scheduler_perf
workload, ~86 GB is allocated to schedule 9000 pods and GC accounts for ~45 % of CPU samples
while actual scheduler logic is ~1.5 %.

This PR removes three large, avoidable allocations from that hot path. All three are
**behavior-preserving** — no scheduling decision, ordering, filter result, or score changes.
Together they cut scheduling-phase allocations by **~31 %** (85.9 GB → 59.5 GB) and lift
throughput on the benchmark by **~8 %**.

The changes are in shared framework/plugin code, so non-TAS scheduling benefits too; the
effect is simply largest under placement-based scheduling because the hot path runs many more
times per pod.

## Motivation / background

Profiling the `TopologyAwareScheduling` scheduler_perf benchmark showed the throughput ceiling
is set by GC mark-assist stalling the single scheduling goroutine, not by scheduler
computation. The delta heap profile attributes the churn to a handful of per-node/per-resource
allocations that are paid once per node, per placement — i.e. ~50× per pod. Reducing those
allocations directly relieves GC pressure without touching any decision logic.

Full analysis: methodology, CPU/heap breakdown, and per-fix profile evidence are in the
companion analysis (kept out of tree). This PR carries only the code changes.

---

## Changes

Three independent commits, each targeting one allocation site. They touch disjoint code and
can be reviewed independently.

### 1. Guard the per-node named logger in `RunFilterPluginsWithNominatedPods`
`pkg/scheduler/framework/runtime/framework.go`

`RunFilterPlugins` and the entire Score path already build the name-enriched contextual logger
only under `logger.V(4).Enabled()`. `RunFilterPluginsWithNominatedPods` was the one
extension-point runner that did it unconditionally — allocating a new logger **and** a new
`context` on every node in the parallel filter loop.

Wrap it in the same `V(4)` guard. When verbose logging is off (the production default), the
enriched name was never emitted anyway, so output is unchanged; when it is on, output is
identical.

### 2. Allocate `insufficientResources` lazily in `fitsRequest`
`pkg/scheduler/framework/plugins/noderesources/fit.go`

`fitsRequest` runs once per node and pre-sized the result with
`make([]InsufficientResource, 0, 4)`. On a lightly-loaded cluster the node usually fits, so the
slice stays empty and that backing array is allocated for nothing.

Declare it as a nil slice; `append` still allocates on the (rarer) not-fit path. All callers
test `len(...) != 0`, so returning `nil` vs. an empty slice is equivalent.

### 3. Aggregate `PodRequests` once per pod in the resource scorer
`pkg/scheduler/framework/plugins/noderesources/resource_allocation.go`

`calculatePodResourceRequestList` called `calculatePodResourceRequest` — and therefore
`resourcehelper.PodRequests`, a full per-container aggregation that builds a `ResourceList`
map — **once per requested resource** (twice for the default `[cpu, memory]` strategy).

Call `PodRequests` once with the same options, then read each requested resource out of the
returned map. The options and per-resource extraction are byte-for-byte identical to the old
path, so returned values are unchanged.

### Diffstat

```
 pkg/scheduler/framework/plugins/noderesources/fit.go                 |  7 ++++-
 pkg/scheduler/framework/plugins/noderesources/resource_allocation.go | 31 ++++++++++++++++++-
 pkg/scheduler/framework/runtime/framework.go                         |  9 ++++---
 3 files changed, 42 insertions(+), 5 deletions(-)
```

---

## Why this is safe (behavior preservation)

| Change | Argument |
|---|---|
| Filter logger guard | Only affects a debug log *name prefix* that is emitted at V(4)+; at production verbosity nothing was logged. No control flow depends on the logger. |
| Lazy `fitsRequest` slice | Same elements appended in the same order; every caller checks `len()`, and `len(nil) == 0`. Return value is semantically identical. |
| Single `PodRequests` | Same `PodResourcesOptions`, same per-resource `MilliValue()`/`Value()` extraction — just computed once instead of N times. Values are identical. |

No changes to filter outcomes, node scores, placement selection, reservation order, or any
feature-gated path (in-place vertical scaling, pod-level resources, DRA extended resources are
all threaded through unchanged).

---

## Performance impact (measured)

scheduler_perf `BenchmarkPerfScheduling/TopologyAwareScheduling/5000Nodes_2250Gangs_9000Pods`,
`-test.benchtime=1x -perf-memprofile`. Host: Apple M4 Pro (14c), Go 1.26.5, etcd 3.7.0.

Primary metric is **total heap allocated during the scheduling phase** (delta profile,
`alloc_space`) — deterministic, and what the changes target.

| Configuration | Allocated | Δ vs base | Throughput (pods/s) |
|---|--:|--:|--:|
| baseline | 85.87 GB | — | 112.9 |
| + fix 1 (logger) | 73.97 GB | −13.9 % | 121.3 |
| + fix 2 (fitsRequest) | 75.61 GB | −11.9 % | 115.8 |
| + fix 3 (PodRequests) | 81.42 GB | −5.2 % | 112.9 |
| **all three** | **59.54 GB** | **−30.7 %** | **122.1 (+8.1 %)** |

Per-fix profile confirmation (delta heap):
- Fix 1: removes 8.98 GB (`LoggerWithName`) + 3.01 GB (`NewContext`) under
  `RunFilterPluginsWithNominatedPods`.
- Fix 2: `fitsRequest`'s 10.70 GB allocation (line `make([]InsufficientResource, 0, 4)`)
  disappears from the profile.
- Fix 3: `resourcehelper.PodRequests` cumulative drops 15.27 GB → 11.02 GB (−4.25 GB; the
  remainder is the Filter-path and `SignPod` callers, intentionally untouched).

Throughput is measured on a single scheduling goroutine and is timing-sensitive (~±8 % on one
1× run), so it is reported as directional and consistent with the allocation reductions; the
combined-branch number is the fair end-to-end signal.

---

## Testing

- `go build ./pkg/scheduler/...` — passes.
- Existing unit tests for the touched packages should be run in CI:
  ```
  go test ./pkg/scheduler/framework/runtime/...
  go test ./pkg/scheduler/framework/plugins/noderesources/...
  ```
- Benchmarked before/after with scheduler_perf as above (integration benchmark under
  `test/integration/scheduler_perf/podgroup/tas`).

Reproduce:
```shell
export PATH=$PWD/third_party/etcd:$PATH KUBE_CACHE_MUTATION_DETECTOR=false
go test -c -o /tmp/tas.test ./test/integration/scheduler_perf/podgroup/tas/
(cd test/integration/scheduler_perf/podgroup/tas && \
 /tmp/tas.test -test.run='^$' \
   -test.bench='BenchmarkPerfScheduling/TopologyAwareScheduling/5000Nodes_2250Gangs_9000Pods$' \
   -test.benchtime=1x -perf-scheduling-label-filter='performance' \
   -perf-memprofile -data-items-dir=/tmp/out -test.timeout=40m)
go tool pprof -sample_index=alloc_space -top /tmp/out/*-delta-mem.prof | head -1
```

## Risk / rollback

Low risk: three localized, behavior-preserving edits with no API or config surface. Each commit
is independent and individually revertable if a regression is ever attributed to it.

---

## PR metadata (for the GitHub description)

**/kind cleanup**
**/sig scheduling**
**/area scheduling**

Priority: performance / no functional change.

### Release note
```release-note
kube-scheduler: reduced memory allocations in the Filter and Score hot path
(NodeResourcesFit and the framework filter runner), improving scheduling throughput
for pod-group / topology-aware scheduling. No change to scheduling behavior.
```

### Suggested commit layout for the PR
```
scheduler: guard per-node named logger in RunFilterPluginsWithNominatedPods
scheduler/noderesources: allocate insufficientResources lazily in fitsRequest
scheduler/noderesources: aggregate PodRequests once per pod in the resource scorer
```
(These are the three commits already on `fix/tas-all-combined`.)
