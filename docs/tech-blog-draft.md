# Go ランタイムは動的なコンテナ環境に追従できるのか？（cgroup v2 と GOMEMLIMIT の境界を追う）

> **バージョン指標・前提**
> 1. **本記事の対象機能が導入されたバージョン**:
>    - **Go**: Go 1.19（`GOMEMLIMIT` の正式導入）
>    - **Kubernetes**: In-Place Pod Resize (KEP-1287: v1.27 Alpha / v1.33 Beta / v1.35 GA)
> 2. **執筆時点のバージョン**:
>    - **Go**: Go 1.27.3（現行安定版）
>    - **Kubernetes**: v1.33.4+k0s（手元クラスタ環境 / GA は v1.35）
>    - ※ 本記事の実機ログは、v1.33 環境での予備検証（参考結果）として扱います。

---

## 全体構成（ドラフト）

- **第 1 章**: 前提知識：Go のメモリ管理（GOMEMLIMIT）とコンテナの動的リサイズ
- **第 2 章**: 実機検証：メモリが動的に変わったとき、Go アプリの内部で何が起きたか？
- **第 3 章**: なぜ Go ランタイムは追従できないのか？（GC と GOMEMLIMIT の内部構造）
- **第 4 章**: 解決編：cgroup v2 を監視して GOMEMLIMIT をリアルタイム追従させる
- **第 5 章**: Go 公式プロポーザル（#75164）の現在地と今後の展望

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
| **認識している上限** | **1.0 GiB**（広がった！） | **460 MiB**（古いまま凍結） |
| **使用量が 400 MiB 時の挙動** | 「まだ 600 MiB も余裕がある」 | 「上限突破寸前！超高頻度で GC を回せ！」 |

- **結果**:  
  **メモリは 600MB も余っているのに、無駄に GC が回り続けて CPU を無駄食いし、アプリの応答が遅くなる**。

---

## 第 5 章：おわりに（メモ）
- 将来的な展望メモ：インフラとアプリが事前通知（シグナル等）で対話し、`FreeOSMemory()` 等でメモリを退避させてから安全にインプレース縮小する協調プロトコルの可能性。
