# Queue topology walkthrough

This walks through `config/samples/kueue-topology.yaml`, the recommended
shape for running LLMKube inference and batch training against the same GPU
budget: inference lends its idle capacity to batch, and batch is always the
side that gives it back.

The sample defines one `ResourceFlavor` (`default-gpu`, matching any GPU
node), two `ClusterQueue`s in a shared cohort (`inference` and `batch`),
and a `LocalQueue` for each.

## Why inference holds the nominal quota and lends

The `inference` ClusterQueue sets `nominalQuota: 8` and `lendingLimit: 4`
on `nvidia.com/gpu`. `nominalQuota` is inference's own guaranteed
allocation: it is the production workload, so it gets the standing claim on
hardware. `lendingLimit` says up to 4 of those 8 GPUs may be borrowed by
cohort peers whenever inference is not using them, instead of sitting idle.
Set `lendingLimit` equal to `nominalQuota` to lend everything when idle, or
to `0` to lend nothing.

## Why batch has nominalQuota 0 and a borrowingLimit

The `batch` ClusterQueue sets `nominalQuota: 0` and `borrowingLimit: 4`.
Batch owns no GPUs of its own; every GPU a batch job runs on is borrowed
from what inference currently lends. `borrowingLimit` caps how much batch
can borrow at once, matching inference's `lendingLimit` so batch can never,
even transiently, claim more than inference is willing to give up.

## reclaimWithinCohort: Any makes batch the preemptible side

Set on the `inference` ClusterQueue's `preemption` block. It means
inference can reclaim its lent capacity from any cohort peer, regardless of
that peer's priority, the moment inference needs the quota back. Since
batch is the only peer borrowing from inference here, batch is always the
one that gets preempted when inference reclaims.

## withinClusterQueue: Never keeps live inference from preempting itself

Also on `inference`'s `preemption` block. This governs preemption between
workloads inside the same ClusterQueue: with `Never`, an admitted
InferenceService is never preempted by another workload queued in
`inference` itself. Live inference traffic is never interrupted to make
room for more inference; the only preemption pressure in this topology
comes from the cohort-level reclaim above, and it only ever lands on batch.

## The scale-to-zero quota-release loop

Scaling an InferenceService to zero deactivates its Workload, which
releases the quota it was holding back to the cohort. Any batch jobs queued
waiting on capacity can now be admitted against the freed GPUs. Scaling the
InferenceService back up re-requests admission through `inference`,
reclaiming from batch again under `reclaimWithinCohort: Any` if the freed
capacity is still in use.

## Applying the sample end to end

Apply in this order: the topology first, then the InferenceService, then
the Job.

```bash
kubectl apply -f config/samples/kueue-topology.yaml
kubectl apply -f config/samples/inferenceservice-queued.yaml
kubectl apply -f config/samples/batch-job-queued.yaml
```

`ls config/samples` sorts these alphabetically the other way
(`batch-job-queued.yaml`, `inferenceservice-queued.yaml`,
`kueue-topology.yaml`), which is the wrong order to apply them in: both
workloads reference LocalQueues that the topology file creates. With all
three applied, scaling the InferenceService down and back up is the
easiest way to see the lend/reclaim cycle in action.

## Adapting this to your cluster

**Per-accelerator flavors.** This sample uses one flavor covering
`nvidia.com/gpu` for any GPU node. If your cluster mixes accelerator
families, split it into one `ResourceFlavor` per family with a
`nodeLabels` selector, covering whichever resource name LLMKube's Model
uses for that hardware: `nvidia.com/gpu` for NVIDIA, `amd.com/gpu` for AMD
ROCm, or `devic.es/dri-render` for the generic Vulkan device-plugin path.
Give each flavor its own quota so families don't share a lending pool they
can't actually use interchangeably.

**MIG (partitioned GPU sharing).** This topology assumes whole-GPU or
shared-mode quota, where the resource name Kueue accounts for matches what
the pod actually requests. LLMKube's gpuSharing partitioned (MIG) mode
schedules against a different resource (`nvidia.com/mig-<profile>`), which
this integration does not yet resolve correctly; see
[issue #9](https://github.com/defilantech/llmkube-kueue/issues/9) before
relying on this topology for MIG-partitioned InferenceServices.
