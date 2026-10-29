# Google Cloud staging更新・公開判定

対象は既存の`trace-staging-async5855`（東京）の停止受付stagingです。新規環境の構築・productionへの流用・一般公開の手順ではありません。配置方針は[設計PR #22](https://github.com/Mino829/trace-campus-installation/pull/22)、実環境の確認記録は[Backend #5](https://github.com/Mino829/TRACE_B/issues/5)を参照してください。

**このPRは配備準備のみです。以下の変更コマンドは次の実配備承認後に限って実行します。既存サービス停止、migration、Secret/IAM変更、CI有効化を実施済みとは扱いません。**

## 1. 構成と前提

更新後の目標は次のとおりです。

| リソース | 役割・境界 |
|---|---|
| Cloudflare `trace-staging-preview` Worker | 同一originの`/api/backend/*`を許可リスト経由で中継し、Edge secretを追加 |
| `trace-backend-preview` | 稼働系。既存の`allUsers:roles/run.invoker`とGoのEdge認証を維持。受付・写真・Push・到着・描画は全て無効 |
| `trace-backend` | 停止系。IAM非公開を維持し、明示的なmanual scaling 0・タグなし。旧URLは利用不可 |
| Cloud SQL `trace-postgres` | `trace-staging-async5855:asia-northeast1:trace-postgres`、PostgreSQL 17、DB `trace` |
| `trace-cleanup` Job / Scheduler | アプリ権限で期限削除。移行中はSchedulerを止め、実行中Jobの終了を待つ |
| `trace-migrate` Job | 所有者権限で明示migration。API起動ではDDLを実行しない |

停止受付profileは1 vCPU・512MiB・service-level最小0/最大1・request-based CPUです。既存previewの課金方式を維持し、常時CPU・最小1を勝手に追加しません。これは常駐タイマー、Push、描画WSの継続稼働を保証する構成ではありません。これらを有効にする配備と費用・最小1/CPU常時割当の判断は別途必要です。補助清掃は5分ごとなので、無通信時の30秒清掃は保証しません。

APIごとのレート制限・接続数はプロセスメモリです。2サービスを同じDBへ接続して各max1にしても単一Backendにはなりません。privateを停止したままにします。タグ付きrevisionは通常trafficが0でも起動し得るため、manual 0だけで停止を判定せず、未知のタグも拒否します。タグの用途を確認せず削除しません。

APIの接続プールは最大5。実DBの`max_connections=25`に対し、Job・候補revision・管理接続の余裕も確認してください。Cloud SQL、バックアップ、ビルド、Job、APIの実行には料金が発生します。Cloudflareの処理、D1/KV、Cloud Runのrequest logまで東京保存と確認済みとは扱いません。

### 2026-10-28の配備前確認

- 2 APIは旧`manual-20261021-1311-144cc03`。mainのmergeだけでは更新されていません。
- 実DBは連続001～003で、当時のアプリ用7テーブルは0件、migration履歴は3件でした。本変更の必要最終版は018ですが、実DBを更新した記録ではありません。
- `trace_app`は非superuser、CREATEDB/CREATEROLEなし、publicへのCREATEなし、migration表はSELECTのみ。
- 既存バックアップは1世代、PITR無効、保存先`asia`。保持・処理地域・復元後消去の承認を省略できません。既存ポリシーを自動変更しません。
- `trace-build`、preview/cleanup/migration/Schedulerの実行SAは存在します。GitHub用deploy SAは未作成、Actions variables/secretsは未設定です。

これらは確認時点の事実です。別環境のデータやCloudflare D1/KV、未知のストレージが空だという証拠ではありません。

### 2026-10-29の再確認と反映準備

- 承認済み一時Jobから`trace_app`・`READ ONLY`で実SQLを照会し、連続schema001～003、PostgreSQL 17.11、`max_connections=25`を再確認しました。publicのアプリ表7件はCRUD可能、`schema_migrations`はSELECTのみ、DB/publicへのCREATEとsuperuser/CREATEDB/CREATEROLEは不可です。今回、個人行やデータ件数は取得していないため、10月28日の「0件」を現在値として流用しません。
- API2サービス、cleanup/migrate Job、Artifact RegistryのBackendイメージは旧`manual-20261021-1311-144cc03`のままです。新コードのイメージbuild・003→018のmigration・新表のruntime grants再適用は未実施です。private APIも最小1で稼働し、Schedulerは5分間隔で有効のため、停止窓の確保が必要です。
- Cloudflareの実配信`trace-log`には`TRACE_BACKEND_URL`・`TRACE_EDGE_SHARED_SECRET`がどちらもなく、APIは503です。既存staging Workerは200ですが旧bundleを返しています。binding名・型は[読取専用Actions診断](https://github.com/Mino829/TRACE_F/actions/runs/40842529656)で確認しました。secret値は出力していません。
- Frontend #75は機能ブランチへマージされただけでmainに未反映でした。[main取り込みPR #77](https://github.com/Mino829/TRACE_F/pull/77)は配備順を確定するまでDraftにします。mainへのマージは既存のD1 migration・trace-push・静的build・trace-log配備を自動起動するため、単なるソース整理として先にマージしません。
- 診断用一時Jobは削除済みで、既存サービス・Job・DB設定・IAMは変更していません。これは配備・権限変更・公開承認を済ませた記録ではありません。

## 2. 管理者が先に用意するもの

既存リソースを再作成しません。対象project/region、同名リソースの用途、Cloud SQL接続名、Secretの数値版を照合してください。長期サービスアカウント鍵JSONは不要です。

| アカウント | 用途・必要な境界 |
|---|---|
| `trace-preview-runtime` | preview API。Cloud SQL Client、app-passwordと既存preview Edge secretのAccessor |
| `trace-runtime` | cleanup。Cloud SQL Client、app-passwordのAccessor。API専用Edge/描画/到着鍵は渡さない |
| `trace-migration` | migration/grants。Cloud SQL Client、owner-passwordのAccessor。APIには使用しない |
| `trace-scheduler` | 既存`trace-cleanup` JobだけのInvoker |

将来の更新担当者には対象SAのactAs、既存Runリソース更新、Scheduler pause/resume、バックアップ作成・確認、ビルドの権限が必要です。SQL確認/grants用には一時Jobのcreate/execute/get/deleteも使います。既存SAへの追加権限が必要なら別途承認し、Owner/Editorやproject全体のSecret Accessorを実行SAへ与えません。CI用WIF/IAMの設定は[CI/CD手順](staging-cicd.md)に分離します。

## 3. DBユーザーと秘密情報

既存の`trace_owner` / `trace_app`とパスワードを変更・再作成しません。migrationとgrantsはowner、API/cleanupはappで実行します。

- `GCP_APP_PASSWORD_SECRET=trace-app-db-password:1`
- `GCP_OWNER_PASSWORD_SECRET=trace-owner-db-password:1`
- `GCP_EDGE_SHARED_SECRET=trace-preview-edge-secret:2`（API専用、必須）

上記は参照名です。値を設定ファイル・コマンド引数・Git・チャット・ログへ入れません。`latest`は使いません。

`runtime-grants`はレビュー済みの`deploy/gcp/runtime-grants.sql`を所有者として再適用します。新制御表は必要なSELECT/UPDATEのみです。`serve`前にappとしてmanifestの全権限、連続schema、public/database CREATE不可、migration表書込み不可を実SQLで検証します。readyだけでは個別テーブルの権限を証明できません。

ローカルからのCloud SQL Proxy接続は3307 timeoutで実SQLまで到達できませんでした。`gcloud sql connect`でAuthorized networksを変更する回避はしません。SQL確認/grantsには、digest固定した公式PostgreSQL 17クライアントの一時Cloud Run Jobと既存のUnixソケット接続を使用します。DBサーバーは起動せず、DBパスワードはSecret参照で渡します。UUID名と所有labelを照合して終了時に削除し、削除確認不能も失敗にします。Job実行と通常のCloud Logging保管は発生します。

固定PostgreSQL imageの`base64`はBusyBoxです。SQL転送は`base64 -d`と`set -o pipefail`を使い、decoderが失敗してpsqlが空入力で成功してもJobを成功にしません。修正後の`remote_sql`を同じ固定image・実Cloud SQLでREAD ONLY実行し、実際のSQL結果と一時Job削除404を確認しました。これは`runtime-grants`やmigrationを実行した証拠ではありません。

## 4. 設定とビルド

Python 3標準ライブラリとgcloudを使います。`deploy.sh`が`manual_staging.py`を起動し、CIと同じreadonly guardを利用します。gcloud 591.0.0でreadonlyコマンドとmanual scalingのSDK仕様を確認しています。リポジトリルートで準備します。

```sh
cp deploy/gcp/settings.env.example deploy/gcp/settings.env
cp deploy/gcp/runtime.env.yaml.example deploy/gcp/runtime.env.yaml
set -a
. ./deploy/gcp/settings.env
set +a
```

旧`GCP_RUNTIME_SA`は`GCP_API_RUNTIME_SA` / `GCP_CLEANUP_RUNTIME_SA`へ分離しました。旧任意`GCP_PUSH_*_SECRET` / `GCP_ARRIVAL_*_SECRET` / `GCP_DISPLAY_TOKEN_SECRET`もこの閉鎖profileでは拒否します。値を指定しても黙って無視する互換動作はありません。

runtime例の参加者originは既存preview Workerです。同意版は未承認の現行draft、本文は受付停止noticeであり、承認済み参加同意ではありません。旧GPS版`2026-memory-01`は無効です。新GPS入力は廃止済みなので未確認のキャンパス境界を作らず、未使用の旧geofence/originはConfig.Defaultを維持します。全feature falseをscriptでも強制し、この例から参加受付を開きません。

3つの空間版はFrontend #61と同じ固定bundleです。データを別Graphへ読み替えません。runtime YAMLは重複キーのないトップレベルのquoted stringだけを受け付け、秘密値、DB URL override、未置換値を拒否します。旧画像やPUT fenceが残る環境では、旧bucket/signerをAPI/cleanupの双方に残す必要があります。今回の空DB確認を他環境へ一般化しません。

### リポジトリ間の反映順と実施前条件

1. SQL転送・失敗伝播の修正をレビューし、Backendのrelease commitを確定します。Frontend #77はまだマージしません。
2. 対象を既存の閉鎖stagingと明示し、旧Session/写真/署名PUT fenceの現在値、必要なbucket/signer、稼働revision・タグ・実行中清掃を確認します。過去の空DB記録で省略しません。
3. 次のbuildで得る不変digest、停止窓、復旧点、実行責任者と費用・変更承認を確定します。旧003イメージを018対応の候補やrollback先へ流用しません。
4. 承認後だけ、以下の順で実施します：新イメージbuild → Scheduler/両API停止・drain確認 → 停止後の明示backup → 番号順migration → runtime grants/実SQL権限確認 → 同一digestのpreview/cleanup更新 → 閉鎖状態のready/config/清掃確認。詳細は第5～6節に従います。
5. Workerの接続先・環境用途を承認してから既存Edge secretの数値版参照と安全に同期します。`trace-log`をstaging正本へ無断接続したり、IAM非公開APIを公開して解決したりしません。Front mainの自動配備を含めた切替順をレビューした後にだけ#77をマージし、Worker経由の閉鎖configと/v2・停止・本人権利を確認します。

新しいイメージdigest・停止後backup ID・停止完了時刻は、実操作前には存在しません。設定の未置換値や古いbackupを埋めて「準備済み」にせず、取得・検証後にだけ変更コマンドへ渡します。実Google・描画/運営・実Push・一般参加受付の有効化は、この閉鎖staging更新とは別の承認・受入試験です。

実ビルドも次回承認後です。リリース固有タグで作り、返された不変digestを`GCP_IMAGE`へ設定します。

```sh
export RELEASE_TAG="manual-$(date -u +%Y%m%d-%H%M%S)-$(git rev-parse --short=12 HEAD)"
export RELEASE_IMAGE="asia-northeast1-docker.pkg.dev/${GCP_PROJECT_ID}/trace/backend:${RELEASE_TAG}"
gcloud meta list-files-for-upload
gcloud builds submit --project="$GCP_PROJECT_ID" --region="$GCP_REGION" \
  --config=cloudbuild.yaml --substitutions="_IMAGE=$RELEASE_IMAGE" .
RELEASE_DIGEST="$(gcloud artifacts docker images describe "$RELEASE_IMAGE" \
  --project="$GCP_PROJECT_ID" --format='value(image_summary.digest)')"
export GCP_IMAGE="asia-northeast1-docker.pkg.dev/${GCP_PROJECT_ID}/trace/backend@${RELEASE_DIGEST}"
```

レビュー済みcommitの作業ツリーからビルドし、タグを別内容で上書きしません。`GCP_IMAGE`のテンプレートを残したまま変更操作はできません。ソース送信前に`.gcloudignore`で設定・認証ファイルが除外されていることを確認します。

## 5. マイグレーション → 権限付与 → API

以下は**停止・課金・DB変更を伴う次回承認後の操作**です。CIを無効のままにし、進行中の配信を終了させてから実施します。schema012等の環境では、既存の待機・再生を処理/明示skipしてキューを空にする必要があります。今回のschema003には到着表自体がありません（[FIFO移行契約](../README.md#投影qrによる到着既定無効)）。

```sh
bash deploy/gcp/deploy.sh quiesce
```

Schedulerをpause、private/previewをmanual 0とし、両APIの反映完了・タグなし・cleanupの非terminal実行なしを確認します。Jobが実行中なら終了まで待ち、guardを再実行します。どこかで失敗したら以降へ進みません。

manual 0の反映は、既存requestが全て終了したという実測ではありません。Cloud Runは処理中requestの完了を許容します。実配備では旧revisionのtimeout・終了猶予と、停止後の新しいMonitoring値による実インスタンス数0、実行中request/DB処理・清掃の終了を確認してください。本PRでは実停止、0のままのrevision更新、activationのprovider動作を実行検証していません。このdrain確認が取れてから停止時刻を記録します。

```sh
export GCP_QUIESCED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
```

停止完了後に、承認した保存場所で明示バックアップを作ります。東京指定の例です。既存の`asia`バックアップや保持/PITR設定は変更しません。

```sh
gcloud sql backups create --instance=trace-postgres --project="$GCP_PROJECT_ID" \
  --location=asia-northeast1 --description="reviewed schema003-to018 maintenance"
gcloud sql backups list --instance=trace-postgres --project="$GCP_PROJECT_ID" \
  --format='table(id,type,status,startTime,endTime,location)'
# Select the completed backup from THIS maintenance, never an old scheduled backup.
export GCP_BACKUP_ID=RETURNED_NUMERIC_BACKUP_ID
bash deploy/gcp/deploy.sh migrate
bash deploy/gcp/deploy.sh runtime-grants
```

backup ID・対象instance・ON_DEMAND/SUCCESSFUL・停止以後の開始/完了時刻を検証します。migrateは連続003または適用済み現行版から004～018を番号順に適用し、連続001～018を確認します。017の参加停止/receipt所有と018の個人運営資格/監査も必要です。成功後の再実行は適用済み版を飛ばします。001/002や不整合履歴はこの手順で自動処理しません。

次に既存Edge keyを、画面非表示入力でこのシェルの環境だけへ読み込みます。ファイルや引数へ渡しません。

```bash
IFS= read -r -s -p 'Existing preview Edge key: ' TRACE_STAGING_EDGE_SHARED_SECRET
printf '\n'
export TRACE_STAGING_EDGE_SHARED_SECRET
bash deploy/gcp/deploy.sh serve
unset TRACE_STAGING_EDGE_SHARED_SECRET
```

`serve`はapp権限を実SQLで確認し、両API停止・Scheduler pauseのまま既存cleanupとpreviewのimage/configを更新します。APIはDB+Edge、cleanupはDB秘密だけです。image/環境/Secret参照/SAと既存IAMが一致してからpreviewだけをauto min0/max1へ戻し、Edge拒否・ready・受付停止・bundleを確認します。失敗時はpreviewをmanual 0へ戻し、privateを復活させません。Schedulerはまだpauseのままです。

### PR #37の展示ブラウザ接続設定

指定済みのWeb client ID、運営メール、`https://trace-log.async5855.workers.dev`はruntime例に保持しています。[PR #37のコメント](https://github.com/Mino829/TRACE_B/pull/37#issuecomment-6461440141)と[描画契約](../README.md#描画websocket既定無効)を参照してください。Googleの承認済みJavaScript originには`/display`を含めません。

閉鎖profileはTRACE_DISPLAY_ENABLED=falseとTRACE_OPERATOR_ENABLED=falseを強制し、描画/到着署名鍵を配布しません。実Google設定・直接HTTPS/WSS・署名鍵/Accessor・運営入口の配備は別途承認が必要です。個人operator資格は独立のGoogle audience/メール/Origin設定から発行し、描画資格で運営操作はできません。初期許可者はasync5855@gmail.comのみです。会場PCの認証付きloopback brokerはEdge keyをサーバー側だけに保持し、資格取得/失効・QR発行を中継します。ブラウザへ長期鍵を渡さず、WSSは中継しません。ローカル検証を実Google/staging/機材確認済みとしません。

## 6. 期限削除の補助ジョブ

ready/configと権限確認後、同じEdge入力を用意して以下を明示実行します。

```bash
IFS= read -r -s -p 'Existing preview Edge key: ' TRACE_STAGING_EDGE_SHARED_SECRET
printf '\n'
export TRACE_STAGING_EDGE_SHARED_SECRET
gcloud run jobs execute trace-cleanup --project="$GCP_PROJECT_ID" --region="$GCP_REGION" --wait
bash deploy/gcp/deploy.sh schedule
unset TRACE_STAGING_EDGE_SHARED_SECRET
```

`schedule`はprivate停止、API/cleanup同一reviewed digest、閉鎖profile、既存Schedulerのpause/5分間隔/UTC/対象URI/OAuth SAを確認してresumeするだけです。作成、IAM付与、対象の書換えはしません。清掃の失敗/未実行の監視は別途必要です。復元後は期限削除を成功させ、削除済み記録の扱いを確認してから受付再開を判断します。

## 7. 疎通・公開判定

変更しない事前確認は`bash deploy/gcp/deploy.sh check`、停止窓の確認は`python3 deploy/gcp/ci_staging.py check-maintenance`です。active確認には既存Edge keyの秘匿環境入力が必要です。管理APIへの更新、DB credentialの取得、Job作成は行いません。現状は2 API稼働なので、manual 0不足で拒否されるのが期待結果です。

更新後は次を記録します。

- privateはmanual 0・タグなし、previewは単一配信revision、API/cleanup同digest、最新連続schemaとapp権限。
- Go直通のno-edge configは403、正しいEdgeでready/configは200。public healthの200は認証漏れではありません。
- Worker configも同じbundle/受付停止/GPS無効/写真・Push無効。これだけでは参加者E2E合格ではありません。
- 一般提供前に、設計Issue #23/#9/#10のGPSなし要件・同意/保持・処理地域/提供者・予算/責任者を承認。
- 別途承認された試験導線で参加→本人メタデータ→到着/再生→停止/本人削除→期限清掃、実Googleログイン、Frontendのfallback/再接続、共有NAT/429、60セッション/8時間、DB再接続と復旧を検証。

公開IAMの存在と参加受付開始は別です。既存previewの境界を保持することは、新しい一般公開や実参加者データ利用の承認ではありません。

## 8. 更新・復旧・データ移行

[CI/CD手順](staging-cicd.md)を有効にするのは、今回の手動移行・権限・Edge/Worker疎通・清掃・復旧を実環境で検証し、mainの対応SHAをschema baselineへ登録した後だけです。mergeのみでbaselineを進めません。

- 失敗時は受付停止を維持し、Schedulerをpause、旧privateを停止のままにする。schemaを扱えない旧003バイナリやGPS/画像受付を再開する版へ戻さない。
- DBを自動downgradeせず、同schema対応の修正版でfix-forward。バックアップ復元は別の破壊的操作として、停止・削除/期限・失われる更新・復元後消去を確認し明示承認する。
- migration/grants失敗、一時Jobの削除確認不能、外部IAM/config/traffic変更は成功扱いにせず、実状態を確認する。強制終了時の自動回収を保証しない。
- 既存Session/写真のGraph版、本人資格、同意、`deleteAfter`を新版へ書き換えない。旧GPS/旧画像の保護・移行方針は[Backend #26](../README.md#gps受付廃止の移行backend-26)に従う。
- 写真は端末内保存が新規方式。旧GCS画像/PUT fenceがある別環境ではbucket/signer・必要権限・期限清掃を残し、独立lifecycleや前倒し削除を追加しない。このPRでbucketの作成・削除・CORS/IAM変更をしない。

## 公式資料

- [Cloud Run manual scalingとタグ付きrevision](https://docs.cloud.google.com/run/docs/configuring/services/manual-scaling)
- [Cloud RunからCloud SQL](https://docs.cloud.google.com/sql/docs/postgres/connect-run)
- [Cloud SQLのオンデマンドbackup](https://docs.cloud.google.com/sql/docs/postgres/backup-recovery/backing-up)
- [CPU割当・課金方式](https://docs.cloud.google.com/run/docs/configuring/billing-settings)
- [Cloud Run Jobの定期実行](https://docs.cloud.google.com/run/docs/execute/jobs-on-schedule)
