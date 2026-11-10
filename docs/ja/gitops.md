# GitOps

workload-knocker は Argo CD / Flux のアプリケーションを OutOfSync にしてはいけません。この方針はすべての actuator に共通で、
`clusters[].actuator` で cluster ごとに選びます ([設定](configuration.md#cluster-registry)を参照)。

## 方針

- コントローラが書き込むのは Git に無いフィールドだけです。scale subresource 経由の `spec.replicas` (直接スケールする
  actuator の場合) と、自身の ConfigMap (state ConfigMap) です。
- Git が管理する workload、ScaledObject などのマニフェストは編集しません。
- workload のマニフェストは `spec.replicas` を省略するか、GitOps ツール側で無視する必要があります。

既定の `keda-prometheus` actuator は workload には何も書かず、`workload_knocker_activation` ゲージを公開するだけで、
スケールは KEDA が行います。それでも KEDA が `spec.replicas` を変更するため、マニフェストのルールは同様に適用されます。`keda-external` actuator も、ゲージの代わりに gRPC ストリームを使う点以外は同じです。[`direct` actuator](#direct-actuator) はこのルールを自分で強制します。

## direct actuator

`actuator: direct` は対象を自分でスケールするため、start / stop の前に毎回この方針を確認します。

- 書き込むのは scale subresource (`deployments/scale`, `statefulsets/scale`) だけで、workload 自体には書きません。
- `metadata.managedFields` を読み、`GITOPS_MANAGERS` (既定は `argocd-controller`、`kustomize-controller`、
  `helm-controller`) の manager が `spec.replicas` を所有していれば Git を優先します。workload はスケールされず、
  操作は `replicas is managed by <manager>; remove replicas from the manifest` で失敗し、warning を出力して
  `workload_knocker_gitops_conflicts` が 1 になります。マニフェストから `replicas` を省く (または下記のように除外する) と、
  次の操作でメトリクスが 0 に戻ります。
- それ以外の manager は影響しません。方針に沿った workload にある HPA や `kube-controller-manager` のエントリ、
  scale subresource 経由のエントリでブロックされることはありません。

Argo CD が Synced のままであることの確認: `replicas` を持たず `workload-knocker.io/enabled: "true"` を付けた
Deployment の Application を同期し、チャットから start / stop して、Application が `Synced` / `Healthy` のまま、
`workload_knocker_gitops_conflicts` が 0 であることを確認します。

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
