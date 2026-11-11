# GitOps

workload-knocker must never make an Argo CD or Flux application OutOfSync. Every actuator follows
the same policy, selected per cluster with `clusters[].actuator` or combined with `clusters[].actuators`
(see [Configuration](configuration.md#cluster-registry)). With several actuators each workload follows the rules
of the actuator that discovered it.

## Policy

- The controller only writes fields that are not in Git: `spec.replicas` through the scale subresource
  (actuators that scale directly) and its own ConfigMaps (the state ConfigMap).
- It never edits workload, ScaledObject or any other manifest that Git owns.
- Workload manifests must omit `spec.replicas`, or the GitOps tool must ignore it.

The default `keda-prometheus` actuator writes nothing to the workloads: it only publishes the
`workload_knocker_activation` gauge and KEDA scales the target. KEDA still changes `spec.replicas`,
so the rule for manifests applies to it too. The `keda-external` actuator behaves the same, with a gRPC
stream instead of the gauge. The [`direct` actuator](#direct-actuator) enforces the rule itself.

## Direct actuator

`actuator: direct` scales the target itself, so it enforces the policy before every start and stop:

- It writes only the scale subresource (`deployments/scale`, `statefulsets/scale`), never the workload.
- It reads `metadata.managedFields`. If a manager from `GITOPS_MANAGERS` (default `argocd-controller`,
  `kustomize-controller`, `helm-controller`) owns `spec.replicas`, Git has priority: the workload is not
  scaled, the operation fails with `replicas is managed by <manager>; remove replicas from the manifest`,
  a warning is logged and `workload_knocker_gitops_conflicts` is 1. Remove `replicas` from the manifest
  (or exclude it as below) and the next operation resets the metric to 0.
- Other managers do not matter: the HPA and `kube-controller-manager` entries on a compliant workload
  do not block it, and neither does an entry written through the scale subresource.

To check that Argo CD stays Synced: sync an Application whose Deployment has no `replicas` and the
annotation `workload-knocker.io/enabled: "true"`, start and stop the workload from chat, and confirm
the Application stays `Synced` and `Healthy` with `workload_knocker_gitops_conflicts` at 0.

## Why

A Git-declared `replicas` value is compared with the live one on every sync. When a controller or KEDA
changes the live value, the tool reports drift and, with self-heal, scales the workload back. That
undoes a start or stop within minutes. Leaving `replicas` out of Git removes the field from the diff.
Where it must stay in the manifest, the tool has to ignore the field and also respect that when syncing.

## Argo CD

Omit `replicas` from the Deployment or StatefulSet. If it must stay, ignore it and make sync honor the
ignore rule:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: games
spec:
  ignoreDifferences:
    - group: apps
      kind: Deployment
      jsonPointers:
        - /spec/replicas
  syncPolicy:
    syncOptions:
      - RespectIgnoreDifferences=true
```

`ignoreDifferences` alone only hides the diff; without `RespectIgnoreDifferences=true` a sync still
applies the Git value.

## Flux

Omit `replicas` from the manifest. Flux server-side apply does not touch fields it does not own, so a
value written by the controller or KEDA stays. If the field must stay in Git, remove it at build time
with a patch on the Kustomization:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: games
spec:
  patches:
    - target:
        kind: Deployment
      patch: |
        - op: remove
          path: /spec/replicas
```
