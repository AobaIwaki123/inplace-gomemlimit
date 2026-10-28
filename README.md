# inplace-gomemlimit

Kubernetes の **In-Place Pod Vertical Scaling (KEP-1287)** および Linux cgroup v2 に完全自動追従し、Pod を再起動することなく `runtime/debug.SetMemoryLimit` (GOMEMLIMIT) を動的に最適化する Go ライブラリです。

コードに **1行インポートを追加するだけ** で動作し、外部依存ゼロ（Go 標準ライブラリのみ）で動作します。

```go
import _ "github.com/AobaIwaki123/inplace-gomemlimit/auto"
```

---

## 背景と解決する課題

### 1. In-Place Resize と Go ランタイムの断絶
Kubernetes 1.27+ で導入された In-Place Pod Vertical Scaling により、コンテナを再起動（コールドスタート・接続切断）することなく、稼働中のまま `limits.memory` を `512Mi` から `1Gi` などへ動的に拡張できるようになりました。

しかし、Go アプリケーションは通常、**プロセスの起動時に 1 度だけ環境変数や cgroups を読み取って内部キャッシュ** します。

そのため、Kubernetes がせっかくメモリ上限を倍増してくれても：
- コンテナは再起動しない（RESTARTS: 0）。
- cgroup v2 の `/sys/fs/cgroup/memory.max` は即座に 1GB に広がる。
- **しかし、Go プロセス内部の `GOMEMLIMIT` は起動時（512MB 時代）の「約 460MB」で凍結されたままになる**。

結果として、Go の GC は「まだ 460MB しか使えない」と誤認して高頻度で GC を走らせ続け、**拡張されたメモリを全く活用できず、CPU を無駄に浪費し続ける** という深刻なギャップが発生します。

### 2. 本ライブラリの解決策
本ライブラリはバックグラウンドで cgroup v2 の `memory.max` の動的変更を監視し、Kubernetes 側でリソースが変更された瞬間に、**再起動なし（インプレース）で即座に `runtime/debug.SetMemoryLimit` を最新の上限（デフォルト 90%）へ追従更新** します。

---

## 主な特徴

- **1行で導入完了**: `auto` パッケージをブランクインポートするだけで、バックグラウンド監視が自動起動。
- **外部依存ゼロ**: Go 標準ライブラリ（`os`, `runtime/debug`, `time`, `log/slog` 等）のみで実装。フットプリントはほぼ 0MB。
- **低リソース・安全設計**: Ticker 駆動で cgroup 変更時のみアトミックにリミットを更新。CPU 負荷は 0.001% 未満。
- **柔軟なカスタマイズ**: 比率（デフォルト 90%）、監視間隔、構造化ロガー、変更時コールバックを設定可能。

---

## 使い方

### 1. 自動起動モード (最も簡単)

`main.go` にブランクインポートを追加するだけです。

```go
package main

import (
    "net/http"
    _ "github.com/AobaIwaki123/inplace-gomemlimit/auto"
)

func main() {
    http.ListenAndServe(":8080", nil)
}
```

### 2. カスタマイズ設定モード

比率やログ出力をカスタマイズしたい場合：

```go
package main

import (
    "context"
    "log/slog"
    "time"

    "github.com/AobaIwaki123/inplace-gomemlimit/pkg/watcher"
)

func main() {
    w := watcher.New(watcher.Options{
        Ratio:    0.85,             // メモリ上限の 85% を GOMEMLIMIT に設定
        Interval: 2 * time.Second,  // 2 秒ごとに cgroup 変更をチェック
        OnLimitChanged: func(oldLimit, newLimit int64, cgroupMax int64) {
            slog.Info("GOMEMLIMIT changed!", 
                "old", oldLimit, 
                "new", newLimit, 
                "cgroupMax", cgroupMax,
            )
        },
    })

    ctx := context.Background()
    _ = w.Start(ctx)

    // ... アプリケーションの起動 ...
}
```

---

## Kubernetes 上での実機検証

Kubernetes クラスタ（v1.33+ / cgroup v2）上で実際に動作確認済みです。

### 1. Pod のデプロイ
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: demo-inplace-gomemlimit
spec:
  containers:
    - name: app
      image: ghcr.io/aobaiwaki123/inplace-gomemlimit-demo:latest
      resizePolicy:
        - resourceName: memory
          restartPolicy: NotRequired
        - resourceName: cpu
          restartPolicy: NotRequired
      resources:
        limits:
          memory: "512Mi"
          cpu: "200m"
```

### 2. インプレースリサイズの実行
```bash
kubectl patch --subresource=resize pod demo-inplace-gomemlimit \
  --patch '{"spec":{"containers":[{"name":"app","resources":{"limits":{"memory":"1Gi"}}}]}}'
```

### 3. 出力ログ (再起動なしで自動追従)
```text
{"time":"2026-10-28T11:35:25Z","level":"INFO","msg":"inplace-gomemlimit: updated GOMEMLIMIT in-place","cgroup_max_bytes":536870912,"cgroup_max_human":"512.0 MiB","old_gomemlimit":"math.MaxInt64","new_gomemlimit":"460.8 MiB","ratio":0.9}

# ... kubectl patch 実行直後 (Pod は再起動せず Running のまま) ...

{"time":"2026-10-28T11:35:27Z","level":"INFO","msg":"inplace-gomemlimit: updated GOMEMLIMIT in-place","cgroup_max_bytes":1073741824,"cgroup_max_human":"1.0 GiB","old_gomemlimit":"460.8 MiB","new_gomemlimit":"921.6 MiB","ratio":0.9}
```

---

## 参考文献

- [KEP-1287: In-Place Update of Pod Resources (Kubernetes Enhancement Proposals)](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/1287-in-place-update-pod-resources)
- [proposal: runtime: cgroup memory limit aware GOMEMLIMIT default (golang/go#75164)](https://github.com/golang/go/issues/75164)
- [Go GC Guide - GOMEMLIMIT (Go Official Documentation)](https://go.dev/doc/gc-guide)

---

## ライセンス

MIT License
