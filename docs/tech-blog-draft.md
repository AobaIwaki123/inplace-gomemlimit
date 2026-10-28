# Kubernetes の In-Place Pod Resize でメモリを拡張したとき、Go アプリのメモリはどうなるのか？

## 目次

- [はじめに](#はじめに)
- [第 1 章：そもそも Kubernetes の In-Place Pod Resize とは何か？](#第-1-章そもそも-kubernetes-の-in-place-pod-resize-とは何か)
  - [1.1 これまでの常識：10 年間抱えていたリソース変更のペイン](#11-これまでの常識10-年間抱えていたリソース変更のペイン)
  - [1.2 コミュニティ・界隈からの熱狂的な期待](#12-コミュニティ界隈からの熱狂的な期待)
  - [1.3 In-Place Pod Resize (KEP-1287) の仕組みと resizePolicy](#13-in-place-pod-resize-kep-1287-の仕組みと-resizepolicy)
- [第 2 章：自宅クラスタでやってみた：実機実験と 2 つの衝撃（執筆中）](#)
- [第 3 章：ここに落とし穴がある！Go ランタイムが取り残される問題（執筆中）](#)
- [第 4 章：解決編：cgroup v2 を監視して GOMEMLIMIT をリアルタイム追従させる（執筆中）](#)
- [第 5 章：おわりに：現在進行形の議論（執筆中）](#)

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

### 1.1 これまでの常識：10 年間抱えていたリソース変更のペイン

従来の Kubernetes において、Pod のリソース（`resources.requests` や `resources.limits`）を変更したい場合、何が起きていたでしょうか？

Kubernetes 公式ブログ（2023年5月 [In-place Resource Resize for Kubernetes Pods](https://kubernetes.io/blog/2023/05/12/in-place-pod-resize-alpha/)）では、これまでの課題を次のように率直に述べています。

> *"Currently, changing resource allocation requires Pods to be recreated, since the PodSpec's Containers' Resources is immutable. This recreation causes workloads to restart, which can be disruptive..."*  
> （現在、PodSpec のコンテナリソース設定がイミュータブル（変更不可）であるため、リソース割り当てを変更するには Pod を再作成する必要がある。この再作成はワークロードの再起動を引き起こし、サービスの停止や中断をもたらす…）

リソースを変えたい動機は日常茶飯事です。
1. **負荷の急増**: トラフィックが増えて現在のメモリや CPU では足りなくなった
2. **無駄なコストの削減**: アクセスが落ち着いたので過剰なリソースを返却したい
3. **初期設定のミス**: そもそも最初のサイジング見積もりが間違っていた

それにもかかわらず、リソースを変更するたびに Pod の再作成（ローリングアップデート）が走るため、以下の深刻なペインを長年強いられていました。
- **通信の切断**: 処理中のリクエストやコネクションが切断される。
- **コールドスタート遅延**: 起動に時間のかかるサービスほど、リソースを増やしたい緊急時に復帰が遅れる。
- **キャッシュの蒸発**: インメモリキャッシュが全消滅し、再起動直後にバックエンド DB へ急激な負荷が集中する。

### 1.2 コミュニティ・界隈からの熱狂的な期待

この機能は、GitHub の Issue 起票（[kubernetes/kubernetes#5774](https://github.com/kubernetes/kubernetes/issues/5774)）から約 10 年越しで実現したため、コミュニティからは「最も待ち望まれていた機能の 1 つ」として熱狂的に迎えられました。

特に以下のユースケースで決定的なゲームチェンジャーとなります。

1. **DB やキャッシュなどのステートフルアプリ**:
   PostgreSQL や Redis などを Pod で動かしている場合、再起動はフェイルオーバーやキャッシュ消滅を伴う大リスクでした。「動かしたままメモリを増やせる」ことは悲願でした。
2. **Java / JVM や大規模 Go などの起動が重いワークロード**:
   初期化やウォームアップに数分かかるアプリにとって、スケールアップ時の再起動は致命的でした。
3. **VPA（Vertical Pod Autoscaler）の真の実用化**:
   負荷に応じて自動で CPU やメモリを伸縮させる VPA は、従来の「都度再起動」では本番運用が困難でしたが、In-Place Resize によって自律的な垂直スケーリングが現実味を帯びてきました。

### 1.3 In-Place Pod Resize (KEP-1287) の仕組みと resizePolicy

In-Place Resize は、**[KEP-1287: In-Place Update of Pod Resources](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/1287-in-place-update-pod-resources)** で策定されました。

プロセスを殺すのではなく、ノード上の kubelet がコンテナランタイムを介して、Linux カーネルの cgroups 設定（`/sys/fs/cgroup/memory.max` など）を直接書き換えることで、**プロセスを動かしたまま枠だけを広げます**。

このとき Pod に設定するのが `resizePolicy` です。

```yaml
spec:
  containers:
    - name: app
      image: my-go-app:latest
      resizePolicy:
        - resourceName: cpu
          restartPolicy: NotRequired    # CPU 変更時は再起動しない
        - resourceName: memory
          restartPolicy: NotRequired    # メモリ変更時も再起動しない
      resources:
        limits:
          memory: "512Mi"
          cpu: "200m"
```

`restartPolicy: NotRequired` を指定することで、「リソースを書き換えてもコンテナを殺さないでくれ！」と Kubernetes に要求できます。

---

ここまでの準備が整ったところで、いよいよ本題です。  
次章では、実際に自宅クラスタ上でパッチを叩いて観測した、**2 つの衝撃的な実機実験** の結果へ進みます。
