# Kubernetes In-Place Resize と Go のメモリ管理：動的なメモリ枠変更に Go アプリはどう追従すべきか

> **バージョン指標・前提**
> 1. **本記事の対象機能が導入されたバージョンと時期**:
>    - **Go**: Go 1.19（`GOMEMLIMIT` 正式導入：2022 年 8 月 GA）
>    - **Kubernetes**: In-Place Pod Resize（KEP-1287: v1.27 Alpha 2023 年 4 月 / v1.33 Beta 2025 年 4 月 / v1.35 GA 2025 年 12 月）
> 2. **執筆時点のバージョン**:
>    - **Go**: Go 1.27.3（現行安定版）
>    - **Kubernetes**: v1.33.4+k0s（手元クラスタ環境 / GA は v1.35）
>    - ※ 本記事の実機ログは、v1.33 環境での予備検証（参考結果）として扱います。

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
- **付録**: 手元での再現手順

---

## 第 1 章：前提知識：Go のメモリ管理とコンテナの動的リサイズ

### 1.1 Go の GC と GOMEMLIMIT の基礎
Go のガベージコレクション（GC）は、デフォルトで「**前回の掃除後のヒープサイズの 2 倍（GOGC=100）**」に達した時点で次の GC を実行します。

- **問題点**: Go ランタイムはコンテナの外側のメモリ上限（例: 500MB）を自動認識しないため、2 倍を目指してヒープを伸ばし、Linux カーネルの OOM Killer によって強制終了（`SIGKILL`, Exit Code 137）される問題が多発していました。
- **GOMEMLIMIT の登場**: Go 1.19 で導入されたソフトメモリリミット。上限値（例: 450MB）を設定すると、メモリがその値に近づいた際に 2 倍ルールを無視して高頻度に GC を実行し、上限の手前でメモリ消費を抑え込みます。

### 1.2 コンテナのメモリが「再起動なし」で変わる時代 (In-Place Resize)
Kubernetes 1.27+ で策定された In-Place Pod Vertical Scaling (KEP-1287) により、Pod を再起動せずにコンテナの CPU やメモリ制限を変更できるようになりました。

- プロセスを殺さず、kubelet が Linux カーネルの cgroup 設定（`/sys/fs/cgroup/memory.max` など）を直接書き換えます。

### 1.3 今回の検証の問い
「インフラ側でメモリ枠が動的に広がったとき、中の Go ランタイム（GC やメモリ管理）は本当にその変化に追従できるのか？」

---

## 第 2 章：実機検証：メモリが動的に変わったとき、Go アプリの内部で何が起きたか？（予備検証）

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

Pod の再起動回数（`RESTARTS: 0`）を維持したまま、アプリケーション側で以下の構造化ログが出力されます。

```json
{
  "time": "2026-10-28T11:35:37Z",
  "level": "INFO",
  "msg": "inplace-gomemlimit: updated GOMEMLIMIT in-place",
  "cgroup_max_bytes": 1073741824,
  "cgroup_max_human": "1.0 GiB",
  "old_gomemlimit": "450.0 MiB",
  "new_gomemlimit": "921.6 MiB",
  "ratio": 0.9
}
```

ログの通り、コンテナの上限が `1.0 GiB` に広がったことを検知し、Go ランタイムのメモリ上限が古い `450.0 MiB` から新しい `921.6 MiB` へと即座に切り替わっていることが確認できます。

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
インフラの動的リサイズとアプリケーションの協調は、今後のクラウドネイティブ環境における必然の流れです。言語本体での自動認識（Issue #75164）は理想ですが、設計合意にはまだ時間がかかりそうです。

当面は本記事のような監視アプローチで現場の課題に対処しつつ、将来的には一律の固定値ではなく、Java のように割合を指定できるオプトインの仕組み（`GOMEMLIMIT=90%` 等）として標準サポートされることを強く期待しています。

---

## コラム：（おまけ）なぜ「90%」の決め打ちが難しいのか？（ヒープと RSS の壁）

Issue #75164 で議論が難航している最大の技術的背景には、「**Go のヒープ**」と「**Linux カーネルが見るメモリ**」のギャップがあります。

Go の `GOMEMLIMIT` が直接制御できるのは、あくまで Go ランタイムが管理する「**ヒープメモリ**」だけです。

一方、Linux カーネルの OOM Killer が監視しているのは、プロセス全体の物理メモリ使用量である **RSS（Resident Set Size）** です。RSS にはヒープだけでなく、以下のメモリが含まれます。

- ゴルーチンのスタック領域
- OS スレッドのスタック
- cgo 経由で割り当てられた C メモリ
- ネットワークやファイルの I/O バッファ

もし言語ランタイムが一律「90%」と決め打ってしまうと、ヒープ外の RSS が 10% 以上あるアプリケーションでは、**Go がいくら GC を回して上限を守っていてもカーネルに OOM Kill されてしまいます**。

この「ヒープと RSS の乖離」があるからこそ、単純な固定値のハードコードでは合意が取れず、Java のように割合を指定できるようにする案などを含めて慎重な議論が続けられています。

---

## 付録：手元での再現手順

実機検証を手元で再現するためのコマンド手順です。

1. **リポジトリのクローンと移動**:
   ```bash
   git clone https://github.com/AobaIwaki123/inplace-gomemlimit.git
   cd inplace-gomemlimit
   ```
2. **マニフェストの適用**:
   ```bash
   kubectl apply -f manifests/demo-pod.yaml
   ```
3. **インプレース拡張の実行**:
   ```bash
   kubectl patch --subresource=resize pod demo-inplace-gomemlimit \
     --patch '{"spec":{"containers":[{"name":"app","resources":{"limits":{"memory":"1Gi"}}}]}}'
   ```
4. **ログの確認**:
   ```bash
   kubectl logs -f demo-inplace-gomemlimit
   ```
