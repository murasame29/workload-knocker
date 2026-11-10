# GitOps

workload-knocker must never make an Argo CD or Flux application OutOfSync. Every actuator follows
the same policy, selected per cluster with `clusters[].actuator` (see [Configuration](configuration.md#cluster-registry)).

## Policy

- The controller only writes fields that are not in Git: `spec.replicas` through the scale subresource
  (actuators that scale directly) and its own ConfigMaps (the state ConfigMap).
- It never edits workload, ScaledObject or any other manifest that Git owns.
- Workload manifests must omit `spec.replicas`, or the GitOps tool must ignore it.

The default `keda-prometheus` actuator writes nothing to the workloads: it only publishes the
`workload_knocker_activation` gauge and KEDA scales the target. KEDA still changes `spec.replicas`,
so the rule for manifests applies to it too.

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
