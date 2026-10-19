# TRACE Backend — PostgreSQL / Google Cloud MVP

匿名参加セッションと短期リアルタイム位置を扱うGo APIです。DBはPostgreSQL 17、公開環境はGoogle Cloud Run＋Cloud SQL for PostgreSQLを想定しています。SQLiteドライバとローカルDBファイルへの保存は廃止しました。

## 実装範囲

- MemorySession / LivePresenceSession / ExperienceLinkの分離発行、別Bearer token認証
- 同意版検証、公開設定、キャンパス範囲・精度・時刻・ジャンプの検証
- 通常GPSの生緯度経度・履歴を保存せず、量子化済み最新表示位置だけを単一Goプロセスのメモリに保持（30～60秒TTL）
- 期限切れ位置の読み出し抑止と毎秒の物理削除、停止・削除時消去。再起動時は表示位置を破棄
- MemorySession / LivePresenceSessionごとの有効期限と削除期限。期限後も各`deleteAfter`までは本人の状態取得・削除が可能
- 個別削除と両トークンによる体験単位削除。後者はLive側から順に個別に確定し、部分成功を24時間の記録と同じキーで再試行
- DBロックとトランザクションで各削除・冪等性記録を一括確定
- CORS応答（許可Originへのプリフライトでは一括削除用`X-Trace-Live-Token`も許可）、16KB JSON上限、ヘルスチェック、JSONログ
- CloudflareサイトWorker向けの共有シークレット検証（設定時、`/v1/*`のみ）
- 明示的なDBマイグレーションと期限削除コマンド
- 任意有効化する非公開写真API：メタデータ登録、署名付きPUT、検証完了、本人ストーリー、写真・セッション・期限削除

| メソッド | エンドポイント |
|---|---|
| GET | /health/live |
| GET | /health/ready |
| GET | /v1/public/config |
| POST | /v1/experiences |
| GET | /v1/experiences/{experienceId} |
| DELETE | /v1/experiences/{experienceId} |
| POST | /v1/live-presence-sessions/{liveSessionId}/position |
| POST | /v1/live-presence-sessions/{liveSessionId}/on-site |
| POST | /v1/live-presence-sessions/{liveSessionId}/stop |
| DELETE | /v1/memory-sessions/{memorySessionId} |
| DELETE | /v1/live-presence-sessions/{liveSessionId} |
| GET / POST | /v1/memory-sessions/{memorySessionId}/photos |
| POST | /v1/memory-sessions/{memorySessionId}/photos/{photoMomentId}/upload-url |
| POST | /v1/memory-sessions/{memorySessionId}/photos/{photoMomentId}/complete |
| DELETE | /v1/memory-sessions/{memorySessionId}/photos/{photoMomentId} |
| GET | /v1/memory-sessions/{memorySessionId}/memory |

経路再構成、到着QR、描画WebSocket、写真ピン・フォトスポット、運営APIは未実装です。写真ストーリーの`segments`は経路実装まで空配列とし、仮の経路を生成しません。
写真機能は既定で無効です。承認済みの非公開ストレージ設定と明示的な有効化が必要で、実装・ローカル検証だけで一般公開や実データ利用の準備完了を意味しません。

### 公開APIのレート制限

| 対象 | 1プロセスでの上限（token bucket） |
|---|---:|
| 参加作成 | クライアントIPごとに60回/分・burst 60、全体10回/秒・burst 10 |
| 公開設定取得 | クライアントIPごとに60回/分・burst 60、全体10回/秒・burst 10 |
| 位置送信 | Live参加トークンごとに12回/分・burst 5 |
| 本人状態取得、会場内選択、停止、個別・一括削除 | 参加トークンごとに30回/分・burst 30 |
| 写真メタデータ登録 | Memory参加トークンごとに6回/分・burst 6、保持中12件まで |
| 公開APIの追加上限 | 認証付きAPIはIPごとに1200回/分・burst 120。全API合計100回/秒・burst 100 |

超過時は`429 RATE_LIMITED`と秒数の`Retry-After`を返し、その間は送信を止めて再試行します。失敗要求も制限対象です。通常の5秒ごとの位置送信は12回/分の補充速度と一致します。写真の一覧・URL再発行・complete・本人memoryは操作枠を共有します。Push・描画・運営の制限は該当APIの実装時に追加します。
許可Originには`Retry-After`をCORSで公開します。一括削除はMemoryのBearer tokenと`X-Trace-Live-Token`の両方の操作枠を同時に確認・消費し、片側が超過していれば削除を開始せず、もう片側の枠も消費しません。位置送信と操作のトークン別枠は分離されていますが、IP別・全体の追加上限は共通です。
IPはWorker経由なら共有シークレット検証後の`X-Trace-Client-IP`（WorkerがCloudflareの`CF-Connecting-IP`から設定）、それ以外は接続元`RemoteAddr`を使用します。`X-Forwarded-For`を信用しません。中継IPしか取得できない場合は参加作成・公開設定のIP制限が中継単位になり得るため、公開前に疎通を確認してください。
転送IPは単一のIPv4/IPv6だけを受け付けます。複数の同名ヘッダー、カンマ区切り、IPv6 zone付きなどは接続元IPへフォールバックします。
カウンターは最大10,000件を1プロセスのメモリに保持し、2分以上使われていないものを消去します。容量超過は`429`（`Retry-After: 60`）で安全側に拒否します。再起動でリセットされ、複数インスタンス間では共有されません。`deploy/gcp/deploy.sh`の最大1インスタンスを維持し、新旧revisionの並存も避けてください。増設する場合は共有レート制限ストアまたは信頼できる共通edgeの制限へ移行します。

## 非公開写真の設定とライフサイクル

- `TRACE_PHOTO_ENABLED=false`が既定です。`true`にするには`TRACE_PHOTO_BUCKET`と`TRACE_PHOTO_SIGNER_EMAIL`の両方が必要です。
- 保存先設定があれば、新規写真を無効にしても既存写真の本人閲覧・削除と清掃は継続します。写真・アップロード枠が残る間に保存先設定を外すと、起動・清掃コマンドは拒否します。
- bucketは東京`ASIA-NORTHEAST1`、Uniform bucket-level accessとPublic access preventionを有効にし、versioning・soft delete・retention/hold・独立したlifecycleルールを無効にします。起動時に検査し、稼働中に変更しないでください。
- ADCとIAM `signBlob`を使用し、秘密鍵ファイルやローカルファイル保存へのフォールバックはありません。APIと清掃ジョブへ同じ保存先を設定します。必要権限とCORSは[配置手順](docs/google-cloud.md)を参照してください。

写真はJPEG/WebP、5MiB以下、長辺1920px以下、1セッション12枚までです。登録時に撮影時GPSを省略でき、通常GPSから補完しません。保存後にサイズ・SHA-256・実形式・寸法・画像全体の復号・EXIF等の不在を検証するまで、画像URLは返しません。ブラウザの構造的なJFIF/Adobe情報は許可しますが、埋め込みサムネイル・任意メタデータ・アニメーションは拒否します。

WebPはVP8Lの過大割り当てを修正した公式`x/image v0.47.0`を使い、独自デコーダのコピーは保持しません。公式のentropy group上限・疎なindexの再マッピングに加え、プロセス内のWebP全体復号を同時1件に制限します。待機はcontextで取消可能で、画像とALPHの両方の悪意ある入力を境界テストで検証します。512MiB構成での実負荷・接続待ちへの影響はstagingで確認してください。

登録の同じ`clientPhotoId`と同じ内容は同じPhotoMomentへ集約します。同じ冪等性キーで異なる内容は409です。削除済み写真の登録再送は404となり、復活させません。登録の再送でURL期限が切れていた場合は`uploadUrl`を省略するため、`pending`に対して`upload-url`を要求します。再発行は初回を含め5回まで、上限到達は409です。

アップロードURLは最大5分（Memoryの`expiresAt`以下）の新規作成専用PUTです。返却する`uploadHeaders`の`Content-Type`、`x-goog-if-generation-match: 0`、`Cache-Control`を送信し、`Content-Length`はブラウザがBlobサイズから自動設定します。上書き・読み取り・一覧には使えません。`complete`はbodyなしで何度送っても同じ写真の状態へ収束します。未アップロードは409、不正画像は消去後`failed`となり、消去に失敗した間は`deleting`で新しいURLを発行しません。

本人memoryは検証済み`ready`だけを撮影時刻順に返し、画像URLは5分と`deleteAfter`の早い方までです。`locationConfidence`はGPSなし/精度100m超をlow、30m超〜100mをmedium、30m以下をhighとする暫定値で、経路の信頼度ではありません。写真一覧には画像URLを含めません。既存の`photoSummary`はDBの実件数から計算します。

削除はまず永続的な`deleting`を確定し、画像の消去が成功するまで参照・認証材料を捨てません。写真DELETE成功は204、写真・Memoryの消去失敗は503となり、同じ冪等性キーで再試行します。体験単位削除は従来どおりLive削除後にMemoryを処理し、写真消去失敗時は`memory: pending`です。期限切れはAPI常駐清掃と清掃ジョブで同じ処理を再開します。

署名付きPUTは削除後も期限まで生きるため、アップロード先はゼロバイトで占有し、期限後だけoutboxから消去します。これにより削除後の再送で画像が復活しません。画像バイト・GPS・PhotoMoment参照は削除対象となり、この短期outboxにはランダムなobject keyと期限だけを保持します。写真の派生特徴・別サムネイル・経路・ピンはまだ生成しないため、その隠れた複製はありません。

DBは`003_photos.sql`を追加します。適用後に`runtime-grants.sql`を実行し、APIと清掃ジョブを同じschema対応版へ更新してください。旧バイナリは新版schemaを拒否するため、受付停止の移行窓と手動migration確認が必要です。SDKの検証は公式クライアントのHTTP fixture、API実動作は専用PostgreSQLと明示的な一時object emulatorで確認しています。実GCSのIAM・bucket CORS・端末アップロードは、承認済みstagingで別途必要です。

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

Go 1.26.9以降、PostgreSQL 17を用意します。CI・Dockerも1.26.9へ揃え、標準ライブラリと写真SDKの間接依存の既知脆弱性を修正した版で検証します。

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

DB接続先は必須です。未設定でSQLiteへ戻ることはありません。旧`TRACE_DATABASE_PATH`や`TRACE_SESSION_TTL`が残っている場合も起動を拒否します。セッション期限は`TRACE_MEMORY_SESSION_TTL`と`TRACE_LIVE_SESSION_TTL`で別々に指定します。
Cloud Runでは`PORT`を使用します。`TRACE_ADDRESS`はローカルでの明示的な上書き用です。
`DB_SSLMODE=disable`はローカル検証・Cloud SQL Auth Proxyのローカルソケット接続だけに使用し、外部DBへ直接TCP接続する場合は証明書検証を有効にしてください。

## DB更新と期限削除

```sh
go run ./cmd/server migrate
go run ./cmd/server cleanup
```

`internal/store/migrations/001_initial.sql`は変更せず、追加分は`002_名前.sql`以降を欠番・重複のない連番で作成します。`migrate`は所有者ユーザーで埋め込みSQLを番号順に単一トランザクションで適用し、`schema_migrations`へ各版を記録します。再実行は適用済み版を飛ばし、並行実行はDBロックで直列化します。失敗時はその実行分をロールバックします。SQLにはトランザクション制御文や`CREATE INDEX CONCURRENTLY`などトランザクション外でのみ使える操作を含めないでください。
APIと清掃ジョブは起動時、APIは`/health/ready`と各`/v1/`リクエスト、定期清掃は実行前に、埋め込まれたmigrationの最終版とDBの連続した適用履歴が一致することを確認します。DBが古い・新しい・履歴に欠番がある場合は受付・清掃を行いません。既存の旧バイナリは新版の検査を持たないため、非互換migrationの前に停止・置換が必要です。APIユーザーにはスキーマ変更権限を付けず、新規テーブルの必要権限を`deploy/gcp/runtime-grants.sql`へ明示的に追加してから配信します。[stagingでの手順](docs/staging-cicd.md#6-運用と復旧)を参照してください。

APIは起動時と稼働中30秒ごとにDBの期限データを清掃し、表示位置は稼働中毎秒プロセスメモリから消去します。期限切れ位置は清掃前でも読み出せません。別途5分ごとのDB清掃ジョブはAPIプロセスの位置メモリを消去できません。Cloud RunのAPIは最大1インスタンス・CPU常時割当とし、スケールアウト・再起動・新旧revision並存で直近の表示位置が失われたり分岐したりするため、追跡中の切替を避けます。

`002_remove_live_positions.sql`で旧表示位置テーブルは破棄し、移行前の位置点は持ち越しません。既存のExperienceLinkは認証トークンの旧期限を`token_expires_at`へ移し、関連付けの保持期限を両セッションの早い`delete_after`へ更新します。再配備時はDBのschema版が非互換となるため、旧revisionを停止してからmigrationを実施します。切替中の位置表示欠落は仕様です。データ保護承認（設計Issue #9）、到着判定の経路（設計Issue #11/Backend Issue #8）は別途未確定です。

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

検証対象：既存API導線、時刻型の往復、資格情報分離、複数接続からの同時削除、停止と位置保存の競合、再送記録失敗時のロールバック、保存期限・関連データ削除、DB migrationの初回・追加・再実行・並行実行・失敗時ロールバック・不整合履歴・旧アプリのready拒否、Cloud Runポート・Cloud SQL接続設定。

## Google Cloudへ配置

[Google Cloud構築・公開手順](docs/google-cloud.md)を参照してください。
[Cloudflare参加者サイトからの接続条件](docs/cloudflare-origin.md)には、公開経路を有効化する前の条件をまとめています。現在のstagingはIAM保護されたままです。

初回デプロイ後の自動更新は[検証環境のCI/CD手順](docs/staging-cicd.md)を参照してください。
既存PostgreSQLテストの成功後だけ、mainの変更をCloud Runへ反映できます。初期状態は無効で、WIF・必要なIAM権限とGitHub変数の設定後に有効化します。
DB構造変更は手動確認が必要です。自動更新では現在の環境設定を保持し、一般公開・参加受付開始を行いません。

- API：Cloud Run（東京、最小1・最大1インスタンス、CPU常時割当）
- DB：Cloud SQL for PostgreSQL 17（東京）
- 接続秘密：Secret Manager、実行用サービスアカウント
- イメージ：Artifact Registry、Cloud Build
- 期限削除：Cloud Run Job＋Cloud Scheduler

このリポジトリだけではクラウドリソースは作成・公開されません。課金先プロジェクトとDBの性能・可用性・保持期間を決めてから手順を実行します。
