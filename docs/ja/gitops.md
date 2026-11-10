# GitOps

workload-knocker は Argo CD / Flux のアプリケーションを OutOfSync にしてはいけません。この方針はすべての actuator に共通で、
`clusters[].actuator` で cluster ごとに選びます ([設定](configuration.md#cluster-registry)を参照)。

## 方針

- コントローラが書き込むのは Git に無いフィールドだけです。scale subresource 経由の `spec.replicas` (直接スケールする
  actuator の場合) と、自身の ConfigMap (state ConfigMap) です。
- Git が管理する workload、ScaledObject などのマニフェストは編集しません。
- workload のマニフェストは `spec.replicas` を省略するか、GitOps ツール側で無視する必要があります。

既定の `keda-prometheus` actuator は workload には何も書かず、`workload_knocker_activation` ゲージを公開するだけで、
スケールは KEDA が行います。それでも KEDA が `spec.replicas` を変更するため、マニフェストのルールは同様に適用されます。

## 理由

Git に書いた `replicas` は同期のたびに実際の値と比較されます。コントローラや KEDA が実際の値を変えると、ツールは差分として
検出し、self-heal が有効なら workload を元の台数に戻します。これでは起動や停止が数分で打ち消されます。`replicas` を Git から
省けば、その差分自体がなくなります。マニフェストに残す必要がある場合は、ツールにこのフィールドを無視させ、同期時もその
設定を尊重させる必要があります。

## Argo CD

Deployment / StatefulSet から `replicas` を省略します。残す必要がある場合は無視設定を入れ、同期でも尊重させます。

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

`ignoreDifferences` だけでは差分を隠すだけで、`RespectIgnoreDifferences=true` が無いと同期時に Git の値が再適用されます。

## Flux

マニフェストから `replicas` を省略します。Flux の server-side apply は自分が所有しないフィールドに触れないため、
コントローラや KEDA が書いた値は保たれます。Git に残す必要がある場合は、Kustomization でそのフィールドを取り除きます。

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
