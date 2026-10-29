# Kubernetes In-Place Resize と Go のメモリ管理：動的なメモリ枠変更に Go アプリはどう追従すべきか

> **バージョン指標・前提**
> 1. **本記事の対象機能が導入されたバージョンと時期**:
>    - **Go**: Go 1.19（`GOMEMLIMIT` 正式導入：2022 年 8 月 GA）
>    - **Kubernetes**: In-Place Pod Resize（KEP-1287: v1.27 Alpha 2023 年 4 月 / v1.33 Beta 2025 年 4 月 / v1.35 GA 2025 年 12 月）
> 2. **執筆時点（2026 年 10 月）の最新バージョンとリリース日**:
>    - **Go**: Go 1.27.3（2026 年 10 月 20 日リリース）
>    - **Kubernetes**: v1.37.1（2026 年 9 月 23 日リリース）
>    - ※ 本記事の実機ログは、手元の検証クラスタ（v1.36.4+k0s: In-Place Resize GA 後の環境）で取得した検証結果です。

---

## 概要（この記事のポイント）

- **この記事が扱う疑問**:  
  「Kubernetes で動いているコンテナのメモリ枠を増やしたとき、中の Go アプリはちゃんと広いメモリを使ってくれるのか？」
- **実験して分かった事実**:  
  実は、**コンテナのメモリ枠を広げても、Go アプリは起動時の古いメモリ上限を覚えたまま動き続ける**ため、自動的には広いメモリを使ってくれません（メモリが余っているのに、ゴミ掃除を焦って CPU を無駄遣いします）。
- **どうすれば解決するのか**:  
  現在はアプリ側から Go のメモリ設定を更新してあげる工夫（本記事で実証する自作ライブラリ `inplace-gomemlimit` 等）が必要です。また、将来的に Go 言語が自動で気づいてくれるようにするための標準機能化の議論も始まっています。

---

## 全体構成

- **第 1 章**: 前提知識：Go のメモリ管理（GOMEMLIMIT）とコンテナの動的リサイズ
- **第 2 章**: 実機検証：メモリが動的に変わったとき、Go アプリの内部で何が起きたか？
- **第 3 章**: なぜ Go ランタイムは追従できないのか？（GC と GOMEMLIMIT の内部構造）
- **第 4 章**: 解決編：cgroup v2 を監視して GOMEMLIMIT をリアルタイム追従させる
- **第 5 章**: 今後の展望：Go 言語本体における標準サポートの検討状況
- **コラム**: （おまけ）なぜ「90%」の決め打ちが難しいのか？（ヒープと RSS の壁）
- **付録 1**: 実機検証の再現手順（詳細手順書へのリンク）
- **付録 2**: 用語集（Glossary）
- **参考リンク・文献**: 公式ドキュメントと関連リソース

---

## 第 1 章：前提知識：Go のメモリ管理とコンテナの動的リサイズ

### 1.1 Go の GC と GOMEMLIMIT の基礎
Go のガベージコレクション（GC）は、デフォルトで「**前回の掃除後のヒープサイズの 2 倍（GOGC=100）**」に達した時点で次の GC を実行します（詳細は [A Guide to the Go Garbage Collector](https://go.dev/doc/gc-guide) を参照）。

- **問題点**: Go ランタイムはコンテナの外側のメモリ上限（例: 500MB）を自動認識しないため、2 倍を目指してヒープを伸ばし、Linux カーネルの OOM Killer によって強制終了（`SIGKILL`, Exit Code 137）される問題が多発していました。
- **GOMEMLIMIT の登場**: Go 1.19 で導入されたソフトメモリリミット。上限値（例: 450MB）を設定すると、メモリがその値に近づいた際に 2 倍ルールを無視して高頻度に GC を実行し、上限の手前でメモリ消費を抑え込みます。

### 1.2 なぜ「再起動なし」が求められたのか？（これまでの VPA の苦悩）

Kubernetes でのリソース自動調整といえば、Pod の台数を増減させる HPA（Horizontal Pod Autoscaler）が一般的ですが、メモリ消費量に応じて Pod 単体のスペックを拡張する VPA（Vertical Pod Autoscaler）も長年求められてきました。

しかし、従来の Kubernetes において Pod は「一度作ったら設定変更できない不変（Immutable）な存在」でした。そのため、従来の VPA（`updateMode: "Recreate"`）には現場を悩ませる深刻な副作用があり、国内の多くの SRE 現場でも **「予期せぬ Eviction（Pod 強制退去）によるダウンタイムが怖すぎて、本番では `updateMode: Off`（推奨値を眺めるだけ）でしか使えない」** という状態が続いていました。

具体的には、以下のような業務インパクトが発生します。

- **ピーク時の強制切断**: メモリが逼迫したまさにその瞬間に Pod が強制終了・再起動されるため、WebSocket や gRPC の常時接続が数万人規模で切断され、再接続スパイク（Thundering Herd）を招く。
- **長時間の計算が巻き戻る**: 数時間かけてメモリ上で計算していたバッチ処理や機械学習の途中データが消し飛び、最初からやり直しになる。
- **キャッシュ消失とコールドスタート**: オンメモリに展開していたキャッシュデータが消え、再起動後のウォームアップに数十分かかって API レスポンスが著しく悪化する。

「メモリ枠を広げたいだけなのに、なぜサービスを停止させなければならないのか」――この長年の痛みを解決するために策定されたのが、**[KEP-1287: In-Place Update of Pod Resources](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/1287-in-place-update-pod-resources)** です（v1.27 Alpha、v1.33 Beta を経て v1.35 で正式に GA 到達）。

さらに、Kubernetes SIG-Autoscaling でも **[AEP-4016: Support for in-place updates in VPA](https://github.com/kubernetes/enhancements/tree/master/keps/sig-autoscaling/4016-in-place-updates-in-vpa)** が推進され、従来の破壊的な Eviction を脱して `InPlaceOrRecreate` モードへと移行する動きが標準化されつつあります。また、大手クラウド監視 SaaS の [Datadog の Kubernetes Autoscaling ドキュメント](https://docs.datadoghq.com/containers/kubernetes/autoscaling/) においても、インプレースリサイズを活用して Pod 再起動を防ぐ垂直スケーリングの重要性が大きくフィーチャーされています。

これにより、Kubelet がコンテナ稼働中のまま Linux カーネルの cgroup 設定（`/sys/fs/cgroup/memory.max` 等）を直接書き換え、**再起動ゼロ（`restartPolicy: NotRequired`）でメモリ枠を動的に拡張できる時代** が本格的に到来しました。

---

### 1.3 浮上した「Go ランタイムとのラストワンマイルの断絶」

インフラ側が何年もかけて「再起動なしのメモリ枠拡張」を完成させたことで、一件落着に見えました。

しかし、ここで新たな疑問が浮上します。

> **「インフラ（cgroup）側でメモリ枠が動的に広がったとき、その上で動いている Go ランタイム（GC やメモリ管理）は、本当に新しい広いメモリを自動認識して使ってくれるのか？」**

もしインフラが再起動ゼロで枠を広げても、中の Go アプリケーションがそれに気づけず、古い上限のまま窒息してしまったら、インフラ側の努力はすべて台無しになってしまいます。

この疑問を検証するため、実際に最新の Kubernetes クラスタ（v1.36.4）上で動的なリサイズを仕掛け、Go アプリの挙動を追試してみました。

---

## 第 2 章：実機検証：メモリが動的に変わったとき、Go アプリの内部で何が起きたか？

### 2.1 実験 ①：メモリ削減の罠（k8s API による拒絶）
稼働中の Pod のメモリ上限を `512Mi` から `256Mi` に減らすパッチを実行したところ、以下のエラーで拒絶されました。

```text
The Pod is invalid: 
spec.containers[0].resources.limits[memory]: 
Forbidden: memory limits cannot be decreased unless resizePolicy is RestartContainer
```

- **判明した事実**: Kubernetes の In-Place Resize は、CPU の増減やメモリの増加は再起動なし（`NotRequired`）を許可しているが、**「メモリの削減」だけはコンテナ再起動（`RestartContainer`）を強制する仕様**になっている。
- **背景**: 稼働中にメモリ上限を引き下げると、プロセスがメモリを使い切っていた場合に Linux カーネルの OOM Killer で即死するリスクが高いため、安全策として再起動が義務付けられている。

### 2.2 実験 ②：メモリ増加の成功と、取り残された Go ランタイム
次に、メモリ上限を `512Mi` から `1Gi` に増やすパッチを実行したところ、**再起動ゼロ（RESTARTS: 0）の一瞬で成功**しました。

- コンテナ内の `/sys/fs/cgroup/memory.max` は即座に `536870912` から `1073741824`（1GB）へ動的に更新された。
- **しかし、中の Go アプリは起動時の 460MB のまま凍結され、直感通りに動いてくれないギャップ**を観測。

---

## 第 3 章：なぜ Go ランタイムは追従できないのか？（GC と GOMEMLIMIT の内部構造）

### 3.1 起動時に 1 度だけ固定される GOMEMLIMIT
Go ランタイムは、起動時の `gcinit()` で 1 回だけ値を読み込み、内部変数に固定します。

- **起動時の読み込み箇所**: [`src/runtime/mgc.go#L189`](https://github.com/golang/go/blob/go1.24.0/src/runtime/mgc.go#L189)
  ```go
  // gcinit() 内で 1 度だけ初期化される
  gcController.init(readGOGC(), readGOMEMLIMIT())
  ```
- **動的変更の唯一の口**: [`runtime/debug.SetMemoryLimit()`](https://github.com/golang/go/blob/go1.24.0/src/runtime/debug/garbage.go#L44-L63)
  ```go
  // プログラム側から明示的に呼び出す必要がある
  func SetMemoryLimit(limit int64) int64 {
      return setMemoryLimit(limit)
  }
  ```

Go ランタイム自身には「cgroup ファイルを再読み込みする仕組み」がないため、インフラ側で枠が広がっても自力では気づけません。

### 3.2 発生する実害：メモリがあるのに CPU を食い潰す

| 項目 | コンテナ（Kubernetes 側） | Go ランタイム（アプリ側） |
| :--- | :--- | :--- |
| **認識している上限** | **1.0 GiB**（拡張済み） | **460 MiB**（古いまま凍結） |
| **使用量が 400 MiB 時の挙動** | 余白 600 MiB（通常動作） | 上限逼迫と判定し、GC を高頻度にトリガー（CPU 浪費） |

- **結果**:  
  **メモリは 600MB も余っているのに、無駄に GC が回り続けて CPU を無駄食いし、アプリの応答が遅くなる**。

---

## 第 4 章：解決編：cgroup v2 を監視して GOMEMLIMIT をリアルタイム追従させる

### 4.1 アプローチと自作ライブラリによる検証
Go ランタイム自身が監視を行わないのであれば、**アプリケーションプロセス側で `/sys/fs/cgroup/memory.max` を監視し、変更を検知したタイミングで `runtime/debug.SetMemoryLimit()` を動的に更新する** アプローチを取ります。

#### 既存ライブラリとの違いと自作の理由
Go のメモリ上限自動設定には定番の `KimMachineGun/automemlimit` がありますが、今回検証用ライブラリ（`inplace-gomemlimit`）を自作した理由は、**「1 行インポートでの動的追従の可否」** にあります。

| 項目 | `automemlimit` | 検証ライブラリ（`inplace-gomemlimit`） |
| :--- | :--- | :--- |
| **1 行インポート時の挙動** | **起動時に 1 回実行して終了**（枠が増えても追従しない） | **常時監視し、枠が増えたら自動追従する** |
| **動的追従させるための手順** | `main.go` 内に初期化コードを明示的に書く必要がある | **1 行インポートするだけで完結** |

`automemlimit` で広く使われている 1 行インポート（ブランクインポート）は、コンテナ起動時に 1 回だけ上限を設定する前提で作られており、そのままでは In-Place Resize に追従できません。そこで、1 行インポートするだけで動的リサイズに自動追従する最小限の仕組みを実証するために本ライブラリを作成しました。

以下は、定期的な監視ループの中で実行される更新処理のコード（[`pkg/watcher/watcher.go`](https://github.com/AobaIwaki123/inplace-gomemlimit/blob/main/pkg/watcher/watcher.go#L106-L130)）です。

```go
func (w *Watcher) update() error {
    // 1. cgroup v2 の memory.max を取得
    data, err := os.ReadFile("/sys/fs/cgroup/memory.max")
    if err != nil {
        return err
    }
    maxBytes, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)

    // 2. 変更がなければスキップ
    if maxBytes == w.lastCgroup {
        return nil
    }

    // 3. 上限が変化していれば、指定比率（デフォルト 90%）で Go ランタイムを更新
    targetLimit := int64(float64(maxBytes) * w.opts.Ratio)
    oldLimit := debug.SetMemoryLimit(targetLimit)
    w.lastCgroup = maxBytes

    // 4. 構造化ログを出力
    slog.Info("inplace-gomemlimit: updated GOMEMLIMIT in-place",
        "cgroup_max_bytes", maxBytes,
        "old_gomemlimit", oldLimit,
        "new_gomemlimit", targetLimit,
    )
    return nil
}
```

### 4.2 `resizePolicy: NotRequired` の役割
Kubernetes で In-Place Resize を利用する際、マニフェスト内で `resizePolicy` を宣言します。

- **仕様書**: [Resize CPU and Memory Resources assigned to containers](https://kubernetes.io/docs/tasks/configure-pod-container/resize-container-resources/)
- **設計書**: [KEP-1287: In-Place Update of Pod Resources](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/1287-in-place-update-pod-resources)

```yaml
resizePolicy:
  - resourceName: memory
    restartPolicy: NotRequired    # メモリ変更時にコンテナを再起動しない
  - resourceName: cpu
    restartPolicy: NotRequired    # CPU 変更時もコンテナを再起動しない
```

仕様書（KEP-1287）にある通り、`restartPolicy: NotRequired` を明示しておくことで、メモリ拡張時にコンテナが再起動されるのを防ぎます。

### 4.3 Go ランタイム内部の切り替えを確認する仕組み
監視処理で出力しているログの `old_gomemlimit`（変更前の値）は、推測値ではありません。

Go の標準関数 [`runtime/debug.SetMemoryLimit(limit int64) int64`](https://github.com/golang/go/blob/go1.24.0/src/runtime/debug/garbage.go#L44-L63) は、新しい上限を設定すると同時に「**直前までランタイム内で有効だった設定値**」を戻り値として返す仕様になっています。これを利用することで、Go ランタイム側で値が実際に切り替わったことを直接確認できます。

### 4.4 実機での追従結果
稼働中の Pod に対し、コンテナのメモリ上限を `512Mi` から `1Gi` に増やすパッチを適用しました。

Pod の再起動回数（`RESTARTS: 0`）を維持したまま、アプリケーション側で以下のログが出力され、動的に追従したことを確認しました。

```text
{"time":"2026-10-28T04:38:27Z","level":"INFO","msg":"runtime memory status","cgroup_max":"536870912","gomemlimit":"460.8 MiB"}
{"time":"2026-10-28T04:38:28Z","level":"INFO","msg":"INFO inplace-gomemlimit: updated GOMEMLIMIT in-place cgroup_max_bytes=1073741824 cgroup_max_human=\"1.0 GiB\" old_gomemlimit=\"460.8 MiB\" new_gomemlimit=\"921.6 MiB\" ratio=0.9"}
{"time":"2026-10-28T04:38:30Z","level":"INFO","msg":"runtime memory status","cgroup_max":"1073741824","gomemlimit":"921.6 MiB"}
```

ログの通り、コンテナの上限が `1.0 GiB` に広がったことを検知し、Go ランタイムのメモリ上限が直前の `460.8 MiB` から新しい `921.6 MiB` へと即座に切り替わっていることが確認できます。

---

## 第 5 章：今後の展望：Go 言語本体における標準サポートの検討状況

### 5.1 先行した CPU（Go 1.25 `GOMAXPROCS`）の自動追従
コンテナ環境での CPU 追従については、Go 1.25 で標準機能として導入されました。

ランタイム内部に「低頻度スキャン機構」が組み込まれており、起動時だけでなくコンテナ稼働中に動的に CPU クォータ（CFS クォータ）が変更された場合でも、外部ライブラリなしで自動追従できるようになっています。

### 5.2 メモリ版プロポーザル（Issue #75164）と現状
CPU と同様のスキャン機構を用いて、メモリ上限（`GOMEMLIMIT`）も自動追従させようという提案（[golang/go#75164](https://github.com/golang/go/issues/75164)）が現在進行中です。

しかし、現在はコードを書く手前の「設計議論」の段階で議論が止まっています。最大の論点は、「**上限の 90% や最低 100MB という数値を言語本体にハードコードしてよいのか**」という点です。

Go の GC を設計した Michael Knyszek 氏は、この提案に対して以下のように慎重な姿勢を示しています。

> *"But the big question in my mind is \"why pick 90%?\" Same for \"why 100 MiB?\" I am loathe to introduce new arbitrary constants into the runtime -- if we do, they should be principled."*  
> （私の中で大きな疑問は『なぜ 90% なのか？』『なぜ 100 MiB なのか？』です。ランタイムに恣意的な定数を導入することは極めて避けたいと考えており、導入するなら理論的な原則に基づくべきです。）

### 5.3 結び
インフラの動的リサイズとアプリケーションの協調は、今後のクラウドネイティブ環境における必然の流れです。

言語本体での自動認識（Issue #75164）の合意形成にはまだ時間がかかりそうですが、当面は本記事のような監視アプローチで対処しつつ、将来的には Java のように割合を指定できるオプトイン形式（`GOMEMLIMIT=90%` 等）での標準サポートを期待したいところです。

---

## コラム：（おまけ）なぜ「90%」の決め打ちが難しいのか？（ヒープと RSS の壁）

Issue #75164 で議論が難航している最大の技術的背景には、「**Go のヒープ**」と「**Linux カーネルが見る RSS**」のギャップがあります。

### 1. Go のヒープと RSS の違い
- **Go のヒープ**:  
  プログラム内で作成した変数やデータ構造が置かれる場所。Go のガベージコレクタ（GC）が掃除でき、`GOMEMLIMIT` で抑えられるのは**このヒープ領域のみ**です。
- **RSS（Resident Set Size）**:  
  「**今まさに物理メモリ（RAM）の上に載っている実データ量**」です。Linux カーネルは Go のヒープ構造など関知せず、プロセス全体が実際に RAM を占有している総量（RSS）だけを監視します。

### 2. ヒープ以外に何が RAM を使っているのか？
RSS には Go のヒープだけでなく、以下の「GC では掃除できないメモリ」が含まれます。

- **ゴルーチンのスタック**: 各ゴルーチンが消費するメモリ（大量のゴルーチンで数百 MB に達する場合がある）
- **Go ランタイム自体の管理領域**: メモリ割り当てキャッシュや GC のメタデータ
- **OS スレッドのスタック**: ランタイムが生成するネイティブスレッド
- **cgo / C 言語のメモリ**: `malloc` などで確保されたヒープ外メモリ

### 3. なぜ一律「90%」固定だと OOM が起きるのか？
たとえばコンテナの上限が 1,000 MB で、`GOMEMLIMIT` を 90% の 900 MB に設定していたとします。

- ヒープ外のメモリ（スタック等）がすでに **200 MB** 使われていた場合
- ヒープが **850 MB** まで伸びた時点で、合計の物理メモリ消費量（RSS）は **1,050 MB** に達します

このとき、Go ランタイム側は「上限 900 MB 未満だからまだ安全」と判断して GC を急ぎません。しかし Linux カーネル側は「コンテナ枠 1,000 MB を超えた」と判定し、**プロセスを即座に強制終了（OOM Kill）** してしまいます。

この「ヒープと RSS の乖離」があるからこそ、単純な固定値のハードコードでは合意が取れず、Java のように割合を指定できるようにする案などを含めて慎重な議論が続けられています。

---

## 付録 1：実機検証の再現手順

本記事で実施した実機検証（Pod の起動、パッチの適用、動的追従ログの確認）を手元で再現するための詳細な手順書は、リポジトリ内の [docs/reproduction-guide.md](https://github.com/AobaIwaki123/inplace-gomemlimit/blob/main/docs/reproduction-guide.md) にまとめています。

前提条件、クロスビルド、マニフェスト適用から後片付けまでの全ステップを記載していますので、手元で動作を試したい方はこちらをご参照ください。

---

## 付録 2：用語集（Glossary）

| 用語 | 概要 |
| :--- | :--- |
| **`GOMEMLIMIT`** | Go 1.19 で導入されたソフトメモリ上限。上限値に達しそうになると GC を高頻度にトリガーし、メモリ消費を抑えて OOM Kill を防ぐ。 |
| **In-Place Pod Resize (KEP-1287)** | Kubernetes で Pod やコンテナを再起動せずに、稼働中のまま CPU やメモリの割り当て枠を変更できる機能。 |
| **cgroup v2 (`memory.max`)** | Linux カーネルが提供するプロセスグループ管理機構。コンテナのハードメモリ上限を指定し、超過すると OOM Killer が発動する。 |
| **RSS (Resident Set Size)** | プロセスが物理メモリ（RAM）上に実際に保持している総容量。Go のヒープ以外にスタック、OS スレッド、cgo、I/O バッファを含む。 |
| **Strategic Merge Patch** | Kubernetes 固有のパッチ形式。キー付きリストの既存要素を壊さずに、指定したフィールドだけを安全にマージ・更新する。 |

---

## 参考リンク・文献（References）

### Go 公式ドキュメント・Issue
- [A Guide to the Go Garbage Collector (公式 GC ガイド)](https://go.dev/doc/gc-guide)
- [runtime/debug.SetMemoryLimit (公式 API リファレンス)](https://pkg.go.dev/runtime/debug#SetMemoryLimit)
- [golang/go#75164: proposal: runtime: cgroup memory limit aware GOMEMLIMIT default](https://github.com/golang/go/issues/75164)
- [golang/go#73193: runtime: make GOMAXPROCS aware of container CPU quota](https://github.com/golang/go/issues/73193)

### Kubernetes 公式ドキュメント・設計書
- [Resize CPU and Memory Resources assigned to containers (公式ドキュメント)](https://kubernetes.io/docs/tasks/configure-pod-container/resize-container-resources/)
- [KEP-1287: In-Place Update of Pod Resources (公式設計仕様書)](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/1287-in-place-update-pod-resources)

### 関連ライブラリ・実装
- [KimMachineGun/automemlimit (GitHub)](https://github.com/KimMachineGun/automemlimit)
- [AobaIwaki123/inplace-gomemlimit (本記事の検証リポジトリ)](https://github.com/AobaIwaki123/inplace-gomemlimit)
