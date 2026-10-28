# Kubernetes の In-Place Pod Resize でメモリを拡張したとき、Go アプリのメモリはどうなるのか？

## 目次と各章のポイント

### はじめに
- 「Pod を再起動せずにメモリを変えられるようになったが、中の Go アプリは追従できているのか？」という疑問

### 第 1 章：そもそも Kubernetes の In-Place Pod Resize とは何か？
- これまでの常識：リソース変更＝Pod の再起動（接続切断やキャッシュ破棄のペイン）
- In-Place Resize の登場：プロセスを殺さず cgroup だけを書き換える
- `resizePolicy: NotRequired` の意味

### 第 2 章：自宅クラスタでやってみた：実機実験と 2 つの衝撃
- 実験①（縮小の罠）：512MiB → 256MiB に縮小しようとしたら k8s API に拒絶された（メモリ縮小は再起動必須という仕様）
- 実験②（拡張の成功）：512MiB → 1GiB へ拡張！RESTARTS: 0 のまま cgroup が書き換わった

### 第 3 章：ここに落とし穴がある！Go ランタイムが取り残される問題
- コンテナは 1GB に広がったのに、Go 内部の GOMEMLIMIT は 460MB で凍結
- メモリが増えたのに GC が頻繁に回り続け、CPU を無駄遣いする

### 第 4 章：解決編：cgroup v2 を監視して GOMEMLIMIT をリアルタイム追従させる
- cgroup v2 の変更を検知して `runtime/debug.SetMemoryLimit` を動的更新
- 1行インポートライブラリ（`inplace-gomemlimit`）の仕組み
- 再びクラスタで検証：再起動ゼロで GOMEMLIMIT が追従したログ

### 第 5 章：おわりに：現在進行形の議論
- Go 公式プロポーザル（#75164: cgroup 自動認識）の現在地
