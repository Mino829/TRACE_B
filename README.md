# TRACE Backend — PostgreSQL / Google Cloud MVP

匿名参加セッションと短期リアルタイム位置を扱うGo APIです。DBはPostgreSQL 17、公開環境はGoogle Cloud Run＋Cloud SQL for PostgreSQLを想定しています。SQLiteドライバとローカルDBファイルへの保存は廃止しました。

## 実装範囲

- MemorySession / LivePresenceSession / ExperienceLinkの分離発行、別Bearer token認証
- 同意版検証、公開設定、キャンパス範囲・精度・時刻・ジャンプの検証
- 通常GPSの生緯度経度・履歴を保存せず、量子化済みの最新表示位置だけを保存
- 位置の論理TTL 60秒、停止時消去、期限切れセッション・リンク・冪等性記録の清掃
- 個別削除、24時間の冪等再送（同じ資格情報＋キー＋内容に限定）
- 複数プロセスをまたぐDBロックとトランザクション。停止・削除と再送用記録を一括確定
- PostgreSQLの時刻型・外部キー・索引、上限付き接続プール
- CORS応答、16KB JSON上限、ヘルスチェック、JSONログ
- CloudflareサイトWorker向けの共有シークレット検証（設定時、`/v1/*`のみ）
- 明示的なDBマイグレーションと期限削除コマンド

| メソッド | エンドポイント |
|---|---|
| GET | /health/live |
| GET | /health/ready |
| GET | /v1/public/config |
| POST | /v1/experiences |
| GET | /v1/experiences/{experienceId} |
| POST | /v1/live-presence-sessions/{liveSessionId}/position |
| POST | /v1/live-presence-sessions/{liveSessionId}/on-site |
| POST | /v1/live-presence-sessions/{liveSessionId}/stop |
| DELETE | /v1/memory-sessions/{memorySessionId} |
| DELETE | /v1/live-presence-sessions/{liveSessionId} |

写真アップロード、経路再構成、到着QR、描画WebSocket、運営API、レート制限は未実装です。
公開設定の写真機能はfalseのままです。現段階は限定検証用MVPで、無制限な一般公開の準備完了を意味しません。

## 最短のローカル起動

DockerとDocker Compose v2を使用します。

1. `.env.example`を`.env`へコピーする。
2. `POSTGRES_PASSWORD`と`DB_PASSWORD`を同じローカル検証用パスワードへ変更する。
3. 次を実行する。

```sh
docker compose up -d --build
docker compose ps -a
curl --fail http://localhost:8080/health/ready
curl --fail http://localhost:8080/v1/public/config
```

PostgreSQLの起動確認→マイグレーション→API起動の順に動きます。DBは`trace-postgres-data`ボリュームへ保存し、DBとAPIの公開ポートはローカル端末内だけに制限しています。
終了には`docker compose stop`を使えます。ボリューム削除を伴う操作はDBデータも消すため行わないでください。
ComposeのDB管理者兼アプリユーザーはローカル検証専用です。本番はDB所有者と実行ユーザーを分離します。

## Goで起動する場合

Go 1.25以降（検証・Dockerビルドは1.26系）、PostgreSQL 17を用意します。

```sh
# 先にComposeでローカルDBだけを起動
docker compose up -d db
# .envは自分で管理した内容だけを読み込む
set -a
. ./.env
set +a
go run ./cmd/server migrate
go run ./cmd/server
```

接続設定は次のいずれかです。

- `DATABASE_URL`：PostgreSQL URLまたはpgxの接続文字列。設定時はこちらが優先。
- `DB_HOST`、`DB_USER`、`DB_PASSWORD`、`DB_NAME`、`DB_SSLMODE`：個別設定。

DB接続先は必須です。未設定でSQLiteへ戻ることはありません。旧`TRACE_DATABASE_PATH`が残っている場合も起動を拒否します。
Cloud Runでは`PORT`を使用します。`TRACE_ADDRESS`はローカルでの明示的な上書き用です。
`DB_SSLMODE=disable`はローカル検証・Cloud SQL Auth Proxyのローカルソケット接続だけに使用し、外部DBへ直接TCP接続する場合は証明書検証を有効にしてください。

## DB更新と期限削除

```sh
go run ./cmd/server migrate
go run ./cmd/server cleanup
```

初期マイグレーションは埋め込みSQLから1回だけ適用し、再実行・同時実行に対応します。API起動時にDDLを実行しないため、本番APIユーザーにはスキーマ変更権限が不要です。

APIは起動時と稼働中30秒ごとに清掃します。期限切れ位置は清掃前でも読み出し対象から除外します。物理削除は通常、期限から次の清掃まで最大約30秒＋処理時間の遅れがあります。Google CloudではCPU常時割当を使い、別途5分ごとの清掃ジョブを補助として設定します。

既存SQLiteファイルの自動インポートは行いません。今回のMVPは空のPostgreSQLから開始します。旧DBファイルを削除・改変する処理はありません。実データの継続が必要な場合は、同意・保存期限・移行対象を確認した専用のデータ移行を別途実施してください。

## テスト

```sh
go test ./...
go vet ./...
```

DB環境変数がなければAPI・DB統合テストは明示的にskipされます。上の成功だけを統合テスト成功と扱わないでください。

専用の使い捨てテストDBを用意して、次を実行します。本番DBは絶対に指定しないでください。

```sh
export TRACE_TEST_DATABASE_URL='postgresql://USER:PASSWORD@127.0.0.1:5432/trace_test?sslmode=disable'
make integration
```

テストごとにランダムな専用スキーマを作り、終了時にそのスキーマだけを削除します。URL内のパスワードに特殊文字がある場合はURLエンコードが必要です。
GitHub Actionsの設定では専用PostgreSQLサービスを起動して統合テストを必ず実行します。

検証対象：既存API導線、時刻型の往復、資格情報分離、複数接続からの同時削除、停止と位置保存の競合、再送記録失敗時のロールバック、保存期限・関連データ削除、マイグレーション再実行、Cloud Runポート・Cloud SQL接続設定。

## Google Cloudへ配置

[Google Cloud構築・公開手順](docs/google-cloud.md)を参照してください。
[Cloudflare参加者サイトからの接続条件](docs/cloudflare-origin.md)には、公開経路を有効化する前の条件をまとめています。現在のstagingはIAM保護されたままです。

初回デプロイ後の自動更新は[検証環境のCI/CD手順](docs/staging-cicd.md)を参照してください。
既存PostgreSQLテストの成功後だけ、mainの変更をCloud Runへ反映できます。初期状態は無効で、WIF・必要なIAM権限とGitHub変数の設定後に有効化します。
DB構造変更は手動確認が必要です。自動更新では現在の環境設定を保持し、一般公開・参加受付開始を行いません。

- API：Cloud Run（東京、最小1・最大3インスタンス、CPU常時割当）
- DB：Cloud SQL for PostgreSQL 17（東京）
- 接続秘密：Secret Manager、実行用サービスアカウント
- イメージ：Artifact Registry、Cloud Build
- 期限削除：Cloud Run Job＋Cloud Scheduler

このリポジトリだけではクラウドリソースは作成・公開されません。課金先プロジェクトとDBの性能・可用性・保持期間を決めてから手順を実行します。
