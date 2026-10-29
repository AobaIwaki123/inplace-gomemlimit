# 実機検証の完全再現ガイド (Reproduction Guide)

本ドキュメントは、Kubernetes In-Place Resize と Go ランタイム（`inplace-gomemlimit`）によるメモリ動的追従の実機検証を手元環境で 100% 再現するための詳細な実行手順書です。

---

## 1. 前提条件と動作確認環境

- **Kubernetes クラスタ**: v1.33+（In-Place Pod Resize が有効化されていること。v1.35+ はデフォルト有効）
- **kubectl**: クラスタ管理者権限で実行可能であること
- **Docker**: コンテナイメージのビルド環境（手元でビルドする場合）
- **Go**: Go 1.24+

---

## 2. リポジトリの準備

```bash
git clone https://github.com/AobaIwaki123/inplace-gomemlimit.git
cd inplace-gomemlimit
```

---

## 3. コンテナイメージの準備

### オプション A: 公開イメージをそのまま使用する場合
クラスタノードから GitHub Container Registry（GHCR）へアクセス可能な場合は、マニフェスト既定のイメージが自動プルされます。

### オプション B: 手元でビルドしてクラスタにロードする場合
クラスタノードのアーキテクチャ（amd64 / arm64）に合わせてビルドし、ノードへインポートします。

```bash
# 1. amd64 ノード向けにクロスビルド
docker build --platform linux/amd64 -t ghcr.io/aobaiwaki123/inplace-gomemlimit-demo:latest .

# 2. k0s ノードへ直接転送・インポートする場合（例: SSH 経由）
docker save ghcr.io/aobaiwaki123/inplace-gomemlimit-demo:latest | ssh <user>@<node-ip> "sudo k0s ctr -n k8s.io images import -"
```

---

## 4. デモ用 Pod の起動と初期状態確認

### 4.1 Pod の作成
マニフェスト（`manifests/demo-pod.yaml`）を適用します。初期状態として `limits.memory: 512Mi`, `requests.memory: 256Mi`, `resizePolicy: NotRequired` が設定されています。

```bash
kubectl apply -f manifests/demo-pod.yaml
```

### 4.2 起動状態の確認
Pod が `Running` になり、再起動回数（`RESTARTS`）が 0 であることを確認します。

```bash
kubectl get pod demo-inplace-gomemlimit
```

### 4.3 初期ログの確認
ログを流し、cgroup の初期上限 `512.0 MiB` を検知して `GOMEMLIMIT` が安全マージン（90%）の `460.8 MiB` で初期化されたことを確認します。

```bash
kubectl logs -f demo-inplace-gomemlimit
```

出力例:
```text
INFO inplace-gomemlimit: updated GOMEMLIMIT in-place cgroup_max_bytes=536870912 cgroup_max_human="512.0 MiB" old_gomemlimit="8.0 EiB" new_gomemlimit="460.8 MiB" ratio=0.9
{"time":"...","level":"INFO","msg":"starting inplace-gomemlimit demo server","addr":":8080"}
{"time":"...","level":"INFO","msg":"runtime memory status","cgroup_max":"536870912","gomemlimit":"460.8 MiB"}
```

---

## 5. In-Place メモリ拡張パッチの適用

### 5.1 パッチファイルの内容確認
`manifests/resize-patch.yaml` を使用します。

```yaml
spec:
  containers:
    - name: app
      resources:
        limits:
          memory: "1Gi"
```

### 5.2 パッチの実行
`--subresource=resize` を指定してパッチを適用します。

```bash
kubectl patch --subresource=resize pod demo-inplace-gomemlimit --patch-file manifests/resize-patch.yaml
```

出力:
```text
pod/demo-inplace-gomemlimit patched
```

---

## 6. 動的追従の検証

### 6.1 Pod が再起動していないことの確認
コンテナ再起動回数が 0（`RESTARTS: 0`）のままであることを確認します。

```bash
kubectl get pod demo-inplace-gomemlimit
```

### 6.2 Go ランタイムの動的更新ログの確認

```bash
kubectl logs demo-inplace-gomemlimit --tail=10
```

期待される出力:
```text
{"time":"...","level":"INFO","msg":"runtime memory status","cgroup_max":"536870912","gomemlimit":"460.8 MiB"}
{"time":"...","level":"INFO","msg":"INFO inplace-gomemlimit: updated GOMEMLIMIT in-place cgroup_max_bytes=1073741824 cgroup_max_human=\"1.0 GiB\" old_gomemlimit=\"460.8 MiB\" new_gomemlimit=\"921.6 MiB\" ratio=0.9"}
{"time":"...","level":"INFO","msg":"runtime memory status","cgroup_max":"1073741824","gomemlimit":"921.6 MiB"}
```

- cgroup の上限が `1.0 GiB` に拡張されたことをウォッチャーが検知
- `runtime/debug.SetMemoryLimit()` により、古い上限 `460.8 MiB` から新しい上限 `921.6 MiB` へとプロセス生存のまま即座に更新されたことが確認できます。

---

## 7. 後片付け (Cleanup)

検証完了後、Pod を削除します。

```bash
kubectl delete -f manifests/demo-pod.yaml
```
