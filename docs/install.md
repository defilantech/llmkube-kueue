# Installing llmkube-kueue

This page covers prerequisites, deploying the controller and webhook, and
what to check when something is not working.

## Prerequisites

- A Kubernetes cluster running LLMKube, with a version that supports
  `spec.suspend` on InferenceService ([LLMKube#1251](https://github.com/defilantech/LLMKube/issues/1251)).
- Kueue v0.19.0.
- cert-manager. The suspend-defaulting webhook serves with
  cert-manager-issued certificates, so it is a hard prerequisite, not an
  optional extra.

```bash
make cert-manager
make kueue
```

`make kueue` installs the stock Kueue v0.19.0 manifests and applies
`hack/kueue-config.yaml` as the `kueue-manager-config` ConfigMap. That file
registers the external framework described below, among other stock Kueue
settings.

## Registering the external framework

Kueue only manages job kinds it knows about. `hack/kueue-config.yaml` adds
one entry to `integrations.externalFrameworks`:

```yaml
integrations:
  externalFrameworks:
  - "InferenceService.v1alpha1.inference.llmkube.dev"
```

This tells Kueue's controller manager to treat `InferenceService` as a
manageable external job kind: when llmkube-kueue creates a `Workload` for a
queue-labeled InferenceService, Kueue's ClusterQueue controllers pick it up
for admission, quota accounting, and preemption like any other job kind.

`make kueue` applies this for you: it writes the ConfigMap from
`hack/kueue-config.yaml` and then does a rollout restart of
`deployment/kueue-controller-manager` in the `kueue-system` namespace, since
Kueue only reads its configuration at startup.

If your cluster manages Kueue through GitOps instead of `make kueue`, do the
equivalent by hand: merge the `externalFrameworks` entry into whatever
source you already use for Kueue's `kueue-manager-config` ConfigMap, then
restart `kueue-controller-manager` so it picks up the change.

**What happens if this entry is missing:** Kueue's manager does not
recognize the `InferenceService` GVK, so it never creates or admits a
`Workload` for it. The suspend-defaulting webhook still runs independently
of this setting, so labeled InferenceServices still start suspended, but
nothing ever admits their (nonexistent) Workload to unsuspend them. The
symptom is services that stay suspended forever with no error anywhere. The
fix is the config entry plus a restart, exactly as above.

## Deploying llmkube-kueue

```bash
make deploy
```

This applies `config/default`, which brings up the controller and webhook
Deployments (the webhook runs 2 replicas behind a PodDisruptionBudget for
availability), the RBAC role and bindings, the webhook Service, the
cert-manager `Certificate`/`Issuer`, and the `MutatingWebhookConfiguration`.

Alternatively, deploy the Helm chart with `webhook.enabled` set, if you are
integrating llmkube-kueue into an existing Helm-based rollout.

## Verifying

```bash
kubectl apply -f config/samples/kueue-topology.yaml
kubectl apply -f config/samples/inferenceservice-queued.yaml
```

Apply the topology first: the InferenceService's `queue-name` label
references the `inference-queue` LocalQueue that this file creates.

Watch a `Workload` appear for the InferenceService, and the service's
`spec.suspend` flip to `false` once the ClusterQueue admits it:

```bash
kubectl get workloads
kubectl get inferenceservice qwen3-8b-queued -o jsonpath='{.spec.suspend}'
```

See `docs/queue-topology.md` for a full walkthrough of the sample topology,
including the batch Job sample.

## Failure modes

| Scenario | What happens |
|---|---|
| Webhook down | `failurePolicy: Fail` means labeled InferenceService creates are rejected outright (the API server call errors). Unlabeled creates are unaffected, since the webhook's `objectSelector` never routes them to it. |
| Controller down | No new Workloads are created and no InferenceServices are unsuspended. Anything already suspended stays suspended; nothing is lost. |
| Kueue uninstalled but the label is still present | The webhook still suspends new labeled creates at admission (it does not depend on Kueue being installed), but nothing exists to admit the Workload, so those services stay suspended indefinitely. |
| Opting out | Remove the `kueue.x-k8s.io/queue-name` label from the InferenceService. This atomically restores LLMKube's built-in `GPUQuota` gating, since both llmkube-kueue's webhook and LLMKube's GPUQuota webhook key off the same label (GPUQuota defers only while the label is present). |
| Labeling a running service | The webhook is CREATE-only by design. Adding the label to an already-running InferenceService via UPDATE is not intercepted by the webhook; LLMKube's own jobframework reconciler handles it instead, suspending the service until admission. This bounce is expected, not a bug. |

There is no private-registry or air-gapped note here: that concern belongs
to LLMKube's own deployment gate, not to this component.
