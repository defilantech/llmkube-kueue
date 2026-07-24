# llmkube-kueue

Kueue integration for [LLMKube](https://github.com/defilantech/LLMKube): an
external [jobframework](https://kueue.sigs.k8s.io/docs/tasks/dev/integrate_a_custom_job/)
component that lets [Kueue](https://kueue.sigs.k8s.io/) admit, queue, and
account GPU quota for `InferenceService` workloads, so one stack manages GPU
budgets for both training and inference.

## How it works

- An `InferenceService` labeled with `kueue.x-k8s.io/queue-name` starts
  suspended (a mutating webhook defaults `spec.suspend: true`).
- This component implements Kueue's `GenericJob` interface for
  `InferenceService` and creates the corresponding Kueue `Workload`.
- When the ClusterQueue admits it, the service is unsuspended and starts.
- Scaling to zero deactivates the Workload and releases its quota; scaling
  back up re-queues for admission.
- Unlabeled InferenceServices are untouched: LLMKube's built-in `GPUQuota`
  keeps gating them, and it defers to Kueue for queue-labeled services.

The recommended topology keeps inference in a high-priority ClusterQueue that
lends idle GPU (`lendingLimit`) while batch borrows and is the preemptible
side. Live inference is not preempted.

Modeled on [konflux-ci/tekton-kueue](https://github.com/konflux-ci/tekton-kueue),
per the guidance in [LLMKube#1249](https://github.com/defilantech/LLMKube/issues/1249).

## Status

Early development. Tracking:
[epic LLMKube#1253](https://github.com/defilantech/LLMKube/issues/1253),
[component issue LLMKube#1252](https://github.com/defilantech/LLMKube/issues/1252),
and this repo's issues. See `docs/install.md` for prerequisites and
deployment steps.

## Docs

- [docs/install.md](docs/install.md): prerequisites, deploying the
  controller and webhook, registering the external framework with Kueue,
  and failure modes.
- [docs/queue-topology.md](docs/queue-topology.md): a walkthrough of the
  sample lend/borrow queue topology in `config/samples/kueue-topology.yaml`.

## License

Apache-2.0
