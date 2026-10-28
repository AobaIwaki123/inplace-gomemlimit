# Kubernetes の In-Place Pod Resize でメモリを拡張したとき、Go アプリのメモリはどうなるのか？（cgroup v2 と GOMEMLIMIT のリアルタイム追従実験）

## 目次

- [はじめに](#はじめに)
- [第 1 章：そもそも Kubernetes の In-Place Pod Resize とは何か？](#第-1-章そもそも-kubernetes-の-in-place-pod-resize-とは何か)
  - [1.1 これまでの常識：リソース変更＝Pod の再起動](#11-これまでの常識リソース変更pod-の再起動)
  - [1.2 In-Place Pod Vertical Scaling (KEP-1287) の登場](#12-in-place-pod-vertical-scaling-kep-1287-の登場)
  - [1.3 resizePolicy の役割と意味](#13-resizepolicy-の役割と意味)
- [第 2 章：自宅クラスタでやってみた：実機実験と 2 つの衝撃（執筆中）](#)
- [第 3 章：ここに落とし穴がある！Go ランタイムが取り残される問題（執筆中）](#)
- [第 4 章：解決編：cgroup v2 を監視して GOMEMLIMIT をリアルタイム追従させる（執筆中）](#)
- [第 5 章：おわりに：現在進行形の議論（Go 公式プロポーザル #75164）（執筆中）](#)

---

## はじめに

Kubernetes 上で Go アプリケーションを運用していると、誰もが一度は「メモリ制限と OOMKilled」の悩みに直面します。

コンテナのメモリ上限（`resources.limits.memory`）に近づいたときに、Go ランタイムがそれを知らずにヒープを伸ばしてしまい、Linux カーネルの OOM Killer によって `SIGKILL (Exit Code 137)` で強制終了されてしまう問題です。

Go 1.19 でソフトメモリリミット機能である `GOMEMLIMIT`（および `runtime/debug.SetMemoryLimit`）が導入され、多くの現場ではコンテナメモリ制限の 85%〜90% を `GOMEMLIMIT` に設定することで、この問題を回避してきました。

しかし、**「Kubernetes 側が稼働中に Pod のメモリ制限を動的に変えたらどうなるのか？」** という点については、これまであまり議論されてきませんでした。なぜなら、従来の Kubernetes では「リソース変更＝Pod の再起動」が当たり前だったからです。

近年の Kubernetes において、Pod を再起動せずに稼働中のまま CPU やメモリを変更できる **「In-Place Pod Vertical Scaling」** が実用化されました。

> 「インフラが再起動なしでコンテナのメモリ枠を広げてくれたなら、中の Go アプリケーションも自動的に広いメモリを活用してくれるのだろうか？」

本記事では、この素朴な疑問を出発点として、自宅の Kubernetes クラスタ（cgroup v2 環境）で実際に実機実験を行い、**「再起動なしでメモリが広がったとき、Go ランタイムの内部で何が起きていたのか」** を暴いていきます。

---

## 第 1 章：そもそも Kubernetes の In-Place Pod Resize とは何か？

まずは前提知識として、Kubernetes のリソース管理に起きたパラダイムシフトである「In-Place Pod Vertical Scaling」について整理します。

### 1.1 これまでの常識：リソース変更＝Pod の再起動

従来の Kubernetes において、Deployment や StatefulSet の YAML で `resources.limits.memory` を変更した場合、何が起きていたでしょうか？

Kubernetes は新しい Pod を作成し、古い Pod を停止させる**ローリングアップデート（再起動）**を実行していました。

この挙動には、以下の深刻なペインが伴います。

1. **接続の切断とダウンタイム**: 処理中の HTTP リクエストや WebSocket 接続が切断される。
2. **コールドスタート遅延**: 起動に数秒〜数十秒かかる重量級アプリケーションでは、リソースを増やしたい緊急時ほどユーザーを待たせてしまう。
3. **インメモリキャッシュの破棄**: プロセス内に保持していたキャッシュが全消滅し、再起動直後にバックエンド DB へ急激な負荷（キャッシュスタンピード）が集中する。

「トラフィックが急増したからメモリを一時的に増やしたいだけなのに、そのために Pod を再起動しなければならない」という矛盾を長年抱えていました。

### 1.2 In-Place Pod Vertical Scaling (KEP-1287) の登場

この課題を根本から解決するために策定されたのが、Kubernetes Enhancement Proposal の **[KEP-1287: In-Place Update of Pod Resources](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/1287-in-place-update-pod-resources)** です。

- **概要**: Pod を停止・再作成することなく、稼働中のコンテナに対して `resources.requests` および `resources.limits`（CPU / メモリ）を動的に変更可能にする機能。
- **仕組み**: 
  1. API Server がリソース変更を受け付ける。
  2. ノード上の kubelet がコンテナランタイム（containerd 等）に対し、Linux の cgroups 仮想ファイルシステム（`/sys/fs/cgroup/memory.max` 等）を直接書き換えるよう命令する。
  3. **プロセスは動き続けたまま、OS カーネルレベルでのリソース枠だけが拡張される**。

### 1.3 resizePolicy の役割と意味

In-Place Resize を利用する際、Pod の仕様に新しく導入されたのが `resizePolicy` です。

各コンテナに対して、「CPU やメモリを変更するときに再起動が必要かどうか」をリソースごとに宣言します。

```yaml
spec:
  containers:
    - name: app
      image: my-go-app:latest
      resizePolicy:
        - resourceName: cpu
          restartPolicy: NotRequired    # CPU 変更時は再起動しない
        - resourceName: memory
          restartPolicy: NotRequired    # メモリ変更時も再起動しない！
      resources:
        limits:
          memory: "512Mi"
          cpu: "200m"
```

`restartPolicy: NotRequired` を指定することで、「メモリを増やしてもコンテナを殺さないでくれ！」と Kubernetes に明示的に要求できます。

これで準備は整いました。  
「再起動なしでメモリが 512MiB から 1GiB に広がった世界」で、Go アプリケーションはどう振る舞うのでしょうか？

次章では、実際に自宅クラスタでパッチを叩いて観測した、**2 つの衝撃的な実機実験** の結果へと進みます。
