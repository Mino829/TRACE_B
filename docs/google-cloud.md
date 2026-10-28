# Google Cloud構築・公開手順

対象：Go APIの限定検証MVP。APIの配置をCloud Run、DBをCloud SQL for PostgreSQL 17に統一します。
この手順やスクリプトの追加だけでは、リソース作成・課金・公開は発生しません。

## 1. 構成と前提

参加端末 → Cloud RunのHTTPS → Go API → Cloud SQLのPostgreSQL。

Cloud RunのファイルシステムへDBを置きません。TLS証明書はCloud Runが管理します。Cloud SQL接続は第2世代実行環境の組み込みAuth ProxyとUnixソケットを使います。

- リージョン：API、DB、Artifact Registryを原則`asia-northeast1`へ揃える。
- Cloud SQL：PostgreSQL 17、DB名`trace`。検証用の最小構成と展示本番のHA構成は別に判断する。
- Cloud Run：1 vCPU、512MiB、同時リクエスト40、最小1・最大1、CPU常時割当を初期値とする。描画WS有効時も量子化済み最新位置とレート制限は単一APIプロセスのメモリだけに保持し、PostgreSQLには到着・再生イベントと主描画機leaseだけを保存する。API/WSを同じインスタンスへ接続し、新旧revisionを並存させない。60セッション・8時間の負荷試験で調整する。
- 接続プール：API1台あたり5接続。通常は最大5接続＋ジョブ分。リビジョン更新中の余裕、管理接続も含めてDB接続上限を確認する。
- 最小1＋CPU常時割当、Cloud SQL、バックアップ等には待機中も費用が発生する。無料運用前提ではない。予算アラートは支出の強制停止機能ではない。

Cloud Runのインスタンスは入れ替わるため、常駐タイマーだけに期限削除を任せず、補助のCloud Run JobをSchedulerから起動します。通常の30秒清掃が停止した場合の補助間隔は5分です。期限超過アラートと復旧手順も必要です。

## 2. 管理者が先に用意するもの

Google CloudのコンソールまたはCloud Shellで、対象プロジェクトを明示して作業します。開発・本番は別プロジェクトまたは別DB・別サービス・別Secretにしてください。

1. 課金を有効化したプロジェクト、予算アラート。
2. Cloud Run、Cloud SQL Admin、Artifact Registry、Cloud Build、Secret Manager、Cloud SchedulerのAPI。
3. 東京リージョンのCloud SQL PostgreSQL 17とDB `trace`。
4. Docker形式のArtifact Registryリポジトリ `trace`。
5. 下記3つのサービスアカウント。ユーザー管理鍵のJSONは作らず、実行サービスに割り当てる。
6. DBパスワード2つをSecret Managerへ登録（`trace-owner-db-password`、`trace-app-db-password`）。

| アカウント | 権限 |
|---|---|
| trace-runtime | Cloud SQL Client。アプリ用パスワードSecretだけのSecret Accessor |
| trace-migration | Cloud SQL Client。所有者用パスワードSecretだけのSecret Accessor |
| trace-scheduler | trace-cleanupジョブだけのCloud Run Invoker |

デプロイ担当者にはCloud Run作成・更新、対象サービスアカウントを使用する権限、Cloud Build起動等が別途必要です。Cloud Buildの実行アカウントには対象Artifact Registryへの書き込み権限等を付けます。プロジェクト全体のOwnerを実行アカウントへ付けないでください。

今回のソケット構成ではCloud SQLのPublic IP＋Auth Proxy経由を使用します。DBのAuthorized networksに`0.0.0.0/0`を登録しません。Private IP限定にする場合はDirect VPC egress等を別途構成してください。`/cloudsql/接続名/.s.PGSQL.5432`がUnixソケットのパス長制限内になるようにします。

## 3. DBユーザーと秘密情報

Cloud SQL Auth Proxy等を使った管理者のpsql接続で、新しい`trace` DBに対して以下を実行します。既存ロールがある場合は無条件に上書きせず状態を確認してください。

```sql
CREATE ROLE trace_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE ROLE trace_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
ALTER DATABASE trace OWNER TO trace_owner;
```

psqlの`\password trace_owner`と`\password trace_app`でパスワードを対話入力し、同じ値をそれぞれSecret Managerへ登録します。パスワードをSQLファイル、シェル履歴、Git、チャットへ書かないでください。

マイグレーションは所有者ユーザー、API・清掃ジョブはアプリユーザーで実行します。初回と以降のschema migration後には`trace_owner`として`deploy/gcp/runtime-grants.sql`を実行してください。新しいruntimeテーブルを追加した場合は、このファイルへ必要なテーブル権限を個別に追記し、`trace_app`でアクセスを確認してから新アプリを配信します。
アプリユーザーにDDL権限がなくても、API・清掃・readinessは動作します。readinessはDBへの接続だけでなく、埋め込みmigrationと適用済み連番の一致も確認します（個別テーブルの権限試験は別途必要）。

`014_registration_control.sql`は運営による新規参加停止を永続化します。環境設定で受付停止→旧API/清掃revision停止→バックアップ→明示migration→`runtime-grants.sql`再適用→新API/清掃起動の順に更新し、実行ユーザーで公開設定と非公開`GET /v1/operator/status`を確認してください。`POST /v1/operator/registration/close`の署名・応答・監査は[Backend README](../README.md#新規参加受付の停止)を参照してください。停止は再起動で解除されず、既存参加者の写真・到着・削除や期限清掃は維持します。個人別運営権限・再開API・本番運営入口の承認はこの追加に含みません。

`015_notification_control.sql`は運営による全撮影通知停止を永続化します。参加受付とPushを環境設定で無効化し、旧API/清掃revision停止→バックアップ→明示migration→`runtime-grants.sql`再適用→新API/清掃起動の順に更新します。実行ユーザーでready・公開設定・非公開statusを確認してください。停止APIの認証・監査・送信中要求との順序付けは[全撮影通知の停止](../README.md#全撮影通知の停止)を参照してください。停止は全購読のendpoint・鍵・予定を削除し、再起動後も購読・送信を禁止します。既にproviderが受理した通知は取り消せません。通知再開APIと個人別運営権限は対象外です。

`016_display_fallback.sql`は運営の通常/切替待ち/fallbackモードと30秒の固定期限を永続化します。受付・描画を無効化し、旧API/清掃revision停止→バックアップ→未適用の013〜016を番号順に明示migration→`runtime-grants.sql`再適用→新API/清掃起動の順で更新してください。schema012からの更新では013の待機/再生整理も[READMEのFIFO移行手順](../README.md#投影qrによる到着既定無効)に従います。`trace_app`には制御行のSELECT/UPDATEだけを付与し、readyと非公開statusを確認します。対応するFrontendの合成表示とsnapshot全置換を結合し、再生中30秒期限・待機保持・明示復帰・再起動で解除されないことをstagingで確認するまで描画を公開しません。認証・API・イベントは[運営によるfallback切替と復帰](../README.md#運営によるfallback切替と復帰)を参照してください。

## 4. 設定とビルド

リポジトリルートで次のテンプレートをコピーして値を埋めます。

```sh
cp deploy/gcp/settings.env.example deploy/gcp/settings.env
cp deploy/gcp/runtime.env.yaml.example deploy/gcp/runtime.env.yaml
set -a
. ./deploy/gcp/settings.env
set +a
```

`settings.env`はプロジェクトID、SQL接続名、イメージのリリースタグ、サービスアカウント、Secretの数値バージョンです。DBパスワード自体は含めません。Workerとの接続検証でedge secretを使う場合だけ`GCP_EDGE_SHARED_SECRET=trace-edge-shared-secret:1`のように数値版参照を設定し、実行アカウントにそのSecretのSecret Accessorを付与します。値そのものはファイル・コマンド引数・ログに置かず、GitHubの`STAGING_EDGE_SHARED_SECRET`と同期してください。未指定のまま`serve`を再実行するとAPIからedge secret参照が外れます。公開経路で使用する場合は再実行前に必ず設定を確認してください。
`runtime.env.yaml`はDB名、CORSの参加画面URL、現地の境界・座標原点、承認済み同意文等です。`DB_HOST`のSQL接続名をsettingsと一致させてください。`TRACE_EDGE_SHARED_SECRET`はここに書かず、`settings.env`の数値版参照からSecret Manager経由でAPIにだけ注入します。清掃ジョブには不要です。`TRACE_REGISTRATION_OPEN`は最初はfalseにします。
到着受付を有効化する場合、`runtime.env.yaml`の`TRACE_ARRIVAL_ENABLED`、会場/ブースIDを設定し、`settings.env`に互いに異なる数値版`GCP_ARRIVAL_ISSUER_SECRET`/`GCP_ARRIVAL_PROOF_SECRET`を指定します。`deploy.sh serve`は秘密値をAPIのみに注入し、清掃ジョブや参加者サイトへ渡しません。運営用APIのネットワーク分離・同意と受付停止手順を承認するまで無効のままにしてください。

ローカルで`make integration`と`go vet ./...`を通した後、Cloud BuildでLinux/amd64イメージを作成します。

```sh
gcloud builds submit --project="$GCP_PROJECT_ID" \
  --config=cloudbuild.yaml --substitutions="_IMAGE=$GCP_IMAGE" .
```

このコマンドはソースをGoogle Cloudへアップロードします。`.gcloudignore`は環境設定・DB・Git情報を除外しますが、初回は必ずアップロード対象を確認してください。Cloud Buildはイメージ作成だけを行い、デプロイやDB更新はしません。タグはリリースごとに変え、同じタグを別内容で上書きしないでください。

### 任意の写真用Cloud Storage

写真は既定で無効です。設計Issue #9/#10の承認前に実参加者データを保存したり、ここに記載しただけでbucket作成・権限付与済みと扱ったりしないでください。写真を使わない最初の非公開Backend配備は`TRACE_PHOTO_ENABLED=false`、写真保存先未設定で進められます。

有効にする場合は承認済みの専用bucketを東京`ASIA-NORTHEAST1`に配置し、Uniform bucket-level access、Public access preventionを有効にします。versioning、soft delete、retention/hold、object retention、独立したlifecycleルールを無効にします。soft deleteは新規bucketで既定有効となる場合があるため、明示的に確認してください。アプリはこれらを起動時に検査し、不一致なら停止します。稼働中に変更しないでください。

API・清掃ジョブの両方へ`TRACE_PHOTO_BUCKET`と`TRACE_PHOTO_SIGNER_EMAIL`を設定し、新規写真を許可する段階だけ`TRACE_PHOTO_ENABLED=true`にします。無効化後も既存写真の本人閲覧・削除・清掃に同じ保存先が必要です。写真や短期アップロードフェンスが残る間は保存先を外さないでください。

ADCを使う実行アカウントには、対象bucketだけの`storage.buckets.get`と、必要なobjectのget/create/delete/update/dropContexts権限を付けます。世代を固定した昇格コピーでは、クライアント由来metadata/contextを引き継がないため`storage.objects.dropContexts`も必要です。署名用アカウントにはURL対象のobjectのcreate/get権限、実行アカウントからその署名用アカウントへ`iam.serviceAccounts.signBlob`が必要です。同じ実行アカウントを署名用にする場合も自己署名のIAM権限を確認してください。Secretの読み取り権限とは別であり、Owner/Editorや長期秘密鍵ファイルを使いません。権限付与は管理者のレビュー・承認後に行います。

bucket CORSは承認済み参加画面のoriginと`PUT`/`GET`に限定し、実ブラウザで`Content-Type`、自動設定される`Content-Length`、`x-goog-if-generation-match`、`Cache-Control`を含む署名付きPUTのプリフライトを確認します。`uploadHeaders`は変更せず、5分以内のURLを使用します。画像アクセスログ・監査ログの保持と削除、署名URLの非記録も個人情報責任者と確認してください。SDK独自のclient metricsは無効化しています。

DBは`003_photos.sql`を追加します。自動デプロイを止め、移行窓で手動migration・`runtime-grants.sql`・実行ユーザー権限・readyを確認し、APIと清掃ジョブを同じschema対応版へ更新してからbaselineを進めます。写真の完了・削除・フェンス清掃は承認済みMVP例外として外部I/O中もDB lockを保持するため、10秒のAPI/定期清掃deadlineと接続プールへの影響を実負荷で確認してください。写真の消去成功前にMemorySessionを削除せず、失敗時は`deleting`から再試行します。独立lifecycleでフェンスを早く消すと削除後のPUT再作成を許すため禁止します。

ローカルでは専用PostgreSQLと一時object emulatorによる実HTTP導線、公式SDKのHTTP fixtureで検証しています。実GCSのIAM signBlob・世代条件・CORS・物理消去・清掃ジョブ・負荷/復旧は未検証であり、承認済みstagingで合格するまで写真機能を公開しません。

## 5. マイグレーション → 権限付与 → API

```sh
bash deploy/gcp/deploy.sh migrate
```

所有者用Secretを使う`trace-migrate`ジョブを作成・更新し、成功まで待ちます。初回と新規migration適用後はこの後、前述の`runtime-grants.sql`を実行し、`trace_app`で必要なテーブル操作と`schema_migrations`の参照を確認してください。失敗したらAPIを更新せずログを確認します。

```sh
bash deploy/gcp/deploy.sh serve
```

清掃ジョブとAPIを作成・更新します。最初はGoogle IAM認証必須の限定公開にします。このスクリプトの再実行もIAM必須に戻すため、一般公開後の更新時には公開設定を別途確認してください。
設定ファイルの更新は該当サービス・ジョブの環境変数を置き換えます。設定はファイル側に集約してください。

`/health/ready`を起動確認、`/health/live`を死活確認に使用します。DB障害だけでAPIを再起動し続けないよう、livenessはDBへ問い合わせません。

任意の描画WSは`TRACE_DISPLAY_ENABLED=false`のまま配備し、承認後に最新migration、`runtime-grants.sql`、ブラウザから直接到達できる専用HTTPS/WSS入口を用意します。Google OAuth WebクライアントID、許可するGmail/Workspaceメール、展示ページOriginを`TRACE_DISPLAY_GOOGLE_CLIENT_ID`/`TRACE_DISPLAY_OPERATOR_EMAILS`/`TRACE_DISPLAY_ALLOWED_ORIGINS`へ設定します。Google OAuthの許可JavaScript Originにも同じ展示Originを登録してください。到着・Edgeとは独立した32バイト以上の署名鍵をSecret Managerに置き、`settings.env`の`GCP_DISPLAY_TOKEN_SECRET`に数値版参照だけを書いてAPIへ注入します。旧描画issuer鍵は不要です。実行サービスアカウントのAccessor権限は対象Secretだけに限定します。発行はGoogle IDトークン、WSは最初の`renderer.authenticate`で描画資格と版を検証します。仕様・失効の制約は[README](../README.md#描画websocket既定無効)を参照してください。該当2経路だけEdge secretを免除しますが、IAM保護を迂回するものではありません。実Googleログイン・許可外資格拒否・Frontend再接続・承認済み公開経路の検証後にのみ有効化を検討し、参加者Workerへ描画/運営経路や鍵を渡さないでください。

### PR #37の展示ブラウザ接続設定

以下は[PR #37のコメント](https://github.com/Mino829/TRACE_B/pull/37#issuecomment-6461440141)への配置手順です。依頼者からWebクライアントIDと運営メールの指定を受け、`.env.example`と`runtime.env.yaml.example`へ反映済みです。描画フラグは無効を維持しており、staging適用・Frontendビルドへの反映・実Googleログインを完了済みとは扱いません。

1. 対象プロジェクトの[Google Auth Platform > Clients](https://console.cloud.google.com/auth/clients?project=trace-staging-507216)で、種類「ウェブアプリケーション」のOAuthクライアントを作成または既存クライアントを確認します。承認済みJavaScript生成元には`https://trace-log.async5855.workers.dev`を登録します。画面のパス`/display`は付けません。OAuth同意画面・対象ユーザー・必要なテストユーザーも運営方針に沿って設定します。
2. [runtime.env.yaml.example](../deploy/gcp/runtime.env.yaml.example)の`TRACE_DISPLAY_GOOGLE_CLIENT_ID`をFrontend #49の`NEXT_PUBLIC_GOOGLE_CLIENT_ID`へ同じ値で渡し、`NEXT_PUBLIC_DISPLAY_API_URL`も設定してFrontendをビルドし直します。どちらかが未設定ならFrontendはテスト再生のままです。Google Identity ServicesのIDトークンcallback方式であり、クライアントsecretをBackendやFrontendへ渡す処理はありません。
3. 明示的に許可したGmail/Workspaceメールだけを`TRACE_DISPLAY_OPERATOR_EMAILS`へカンマ区切りで設定します。GitHub名や例示メールから推定しません。`TRACE_DISPLAY_ALLOWED_ORIGINS`には手順1と同じOriginを設定します。現在の設定例と異なるURLへ展示を配置する場合はGoogleとBackendを同時に更新します。
4. [runtime.env.yaml.example](../deploy/gcp/runtime.env.yaml.example)の3つの空間版をそのまま使用し、Backendの固定Graph・Frontend #61と同じbundleにします。到着・写真・再生中のデータを別Graphへ読み替えず、受付を閉じた移行窓で旧revisionを停止してから切り替えます。
5. `TRACE_DISPLAY_ENABLED=false`のまま必要な設定・独立した署名secret・承認済みブラウザ到達経路を用意し、実行ユーザーでreadyと公開設定の`spatial`を確認します。承認後に描画を有効化し、実Googleログイン→描画token発行→初回snapshot→主lease→到着→started/completedのackを確認します。旧版の4409、許可外アカウントの401、許可外Originの403も確認します。実Googleログインと長時間再接続が未確認なら公開完了にしません。

`deploy.sh serve`は描画有効/無効にかかわらずCloud Runリクエスト上限を3600秒へ設定します。WS本体は50分で終了、10秒ping/pong、30秒無応答切断。HTTP 10秒middleware期限はWS upgradeへ適用しません。Cloud Run timeout/instance replacement後はrendererが短期資格を再取得し、指数バックオフで新snapshotへ再接続します。上限の長い接続は請求とconcurrency枠を消費するため、現場60人規模の実負荷で検証してください。WebSocket経路をCloudflare Workerの参加者用proxyへ流さず、HTTPS upgrade透過・権限・秘密ヘッダー非記録を運営経路で確認してください。変更しても既存stagingへのデプロイを意味しません。

## 6. 期限削除の補助ジョブ

まずScheduler用サービスアカウントへ、清掃ジョブだけの実行権限を付与します。

```sh
gcloud run jobs add-iam-policy-binding trace-cleanup \
  --project="$GCP_PROJECT_ID" --region="$GCP_REGION" \
  --member="serviceAccount:$GCP_SCHEDULER_SA" --role=roles/run.invoker
bash deploy/gcp/deploy.sh schedule
gcloud run jobs execute trace-cleanup \
  --project="$GCP_PROJECT_ID" --region="$GCP_REGION" --wait
```

Schedulerは5分ごとにGoogle APIへOAuth付きでPOSTし、清掃ジョブを起動します。公開APIに「秘密の清掃URL」は作りません。重複実行はDB内ロックで直列化します。
ジョブ成功・失敗をCloud Loggingで確認し、失敗と未実行をCloud Monitoringで通知する設定を別途作ってください。Scheduler設定は構成例の同名ジョブを更新するため、既に別用途の`trace-cleanup`が存在するプロジェクトでは実行しないでください。

## 7. 疎通・公開判定

Cloud Runコンソールで発行URLを確認します。ローカルでIAM認証付きの試験を行うなら以下のプロキシを利用できます。

```sh
gcloud run services proxy trace-backend --project="$GCP_PROJECT_ID" \
  --region="$GCP_REGION" --port=8085
```

別ターミナルから`http://localhost:8085/health/ready`を確認します。参加者用トークンはAPIのAuthorizationヘッダーで使い、Google IAM認証と混同しないでください。両方を直送する場合、Cloud Run用IDトークンは`X-Serverless-Authorization`、参加者トークンは`Authorization`へ分離します。

受付を開いて試験するときは、設計Issue #23/#9のGPSなし参加と同意文の承認後、管理者が設定ファイルの`TRACE_REGISTRATION_OPEN`をtrueへ変更して再デプロイします。Backend #25の写真地点契約とFrontendの新規受付が未整合の間は受付を開きません。一般参加者へ公開する前に以下を完了してください。

写真地点の`selectable`対応には追加migrationやGraph版の書換えはありません。同じ固定bundleで新API/清掃を配布し、旧revisionとの並走を避けます。承認済みstagingでは、`selectable:true`のwaypointの登録、選択不可checkpointの400、旧Graph版Sessionへの追加409、非選択の到着地点までの経路、到着後に届いた撮影済みメタデータと削除による描画差分を確認してください。旧判定で受理済みの写真は前倒し削除せず、本人操作と既存の期限清掃を維持します。このローカル実装・検証だけで実データ受付や一般公開の承認とは扱いません。

- [実装済みレート制限](../README.md#公開apiのレート制限)の共有IP・全体負荷検証と429監視。CORSだけでは悪用防止にならない。
- 正しい参加画面URL、旧版と異なる承認済みのMemory同意版・文面、保存期間と問い合わせ先。
- 実機Memoryのみの参加→写真GPS拒否→旧Liveの位置送信拒否・本人停止・削除→期限削除。
- Secret・DB権限、HTTPS、ログの位置情報・トークン非記録。
- 同時負荷、8時間稼働、DB再接続、清掃失敗通知。
- バックアップ保持・PITR保持と削除説明の整合。復元後は期限削除を実行してから受付を再開する。

IAMの一般公開許可と受付開始は別操作です。レート制限は実装済みですが、実経路の負荷・実機試験などの公開判定を省略できず、一般公開は自動化していません。公開判定を通した後、管理者がCloud Runの呼び出し権限を変更してください。

## 8. 更新・復旧・データ移行

初回のAPI・DB接続・削除ジョブの検証後、[検証環境のCI/CD手順](staging-cicd.md)に沿ってGitHub Actionsの自動更新を有効化できます。
手動ビルドは従来の`cloudbuild.yaml`、Actionsからのビルドは専用アカウント・Cloud Loggingを使う`cloudbuild.ci.yaml`に分けています。
自動更新は既存リソースのイメージとAPI配信先のみを変更し、マイグレーションや環境設定の置き換えは行いません。

- 新しい不変イメージをビルド→互換マイグレーション→権限更新→サービス更新→疎通試験。
- 写真PR #18より前のmainに含まれるmigrationはschema version 1・2。`002_remove_live_positions.sql` は旧API/清掃ジョブと非互換のため、受付を停止し、旧API/ジョブを停止してから適用し、対応するAPI・ジョブを起動する（[切替手順](staging-cicd.md#6-運用と復旧)）。写真無効の先行配備にはversion 3は不要。PR #18マージ後は写真の有効/無効を問わずmigration 003と権限確認が必須。適用済みSQLを書き換えない。
- 到着ブランチ統合後は`006_arrivals.sql`を適用し、`runtime-grants.sql`を再適用する。旧revision/清掃ジョブを停止してからmigrationを行い、schema対応版へ切り替える。旧アプリへのイメージロールバックだけでは旧schemaを読めないため、切替前のDBバックアップと復旧手順を確認する。GPSの移行方針は[Backend #26](../README.md#gps受付廃止の移行backend-26)を参照する。
- 描画ブランチ統合後は`007_display.sql`を適用し、`runtime-grants.sql`を再適用する。位置のメモリ保持と描画イベントのDBカーソルは単一Backendインスタンスを前提とするため、旧revisionとの並存を避け、受付停止・バックアップ/復旧準備の後に切り替える。
- アプリ障害時は受付を閉じて修正版を配信する。旧GPS受付を再開する旧イメージへはロールバックしない。DBは自動ダウングレードしない。
- 本MVPは空のPostgreSQLから開始する。SQLiteのファイル・既存記録は自動削除も転送もしない。
- DB復元時も古い位置情報を配信しないよう、まず受付停止、清掃、削除済み記録の扱いの確認を行う。
- 障害時は展示を合成データのフォールバックへ切り替える。別のDBを同時に正本として書き始めない。

## 公式資料（構成確認に使用）

- [Cloud Runコンテナ契約：PORT、HTTPS、永続化制限](https://docs.cloud.google.com/run/docs/container-contract)
- [Cloud RunからCloud SQLへ接続](https://docs.cloud.google.com/sql/docs/postgres/connect-run)
- [CPU割当・課金方式](https://docs.cloud.google.com/run/docs/configuring/billing-settings)
- [Cloud Runジョブの定期実行](https://docs.cloud.google.com/run/docs/execute/jobs-on-schedule)
- [Cloud Runデプロイ設定](https://docs.cloud.google.com/sdk/gcloud/reference/run/deploy)
