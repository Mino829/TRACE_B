# 検証環境の自動デプロイ（GitHub Actions → Cloud Build → Cloud Run）

対象は`Mino829/TRACE_B`の`main`、`trace-staging-async5855`の既存`trace-backend-preview`だけです。停止受付のstaging用でproductionには使いません。
このPRではCloud設定やGitHub variables/secretsを変更せず、自動デプロイも有効化しません。実配備・IAM/WIF設定は別途承認後に行います。
Cloud Buildのpushトリガーは作りません。既存Actionsのテストと別トリガーを並行起動すると、テスト結果を待たずに配信してしまうためです。

## 1. 自動化する範囲

1. push / PRで既存のPostgreSQL統合テスト、vet、ビルドとデプロイ処理のオフラインテストを実行。
2. `main`へのpushかつテスト成功時のみ、`STAGING_DEPLOY_ENABLED=true`ならデプロイへ進む。
3. 古いコミット、DB構造/権限変更、private側の再稼働、未知のrevisionタグ、想定外の実行SA・DB資格・feature有効化を検出したら停止。既存public-invoker＋Edge境界を維持する。
4. Cloud Build専用アカウントでイメージを作成。SHA＋実行ID＋再試行番号の一意タグを使い、配信はdigestで固定。
5. 現在の環境変数・Secret参照・サービスアカウント・CPU等を保持し、APIのイメージだけ更新。最初は既存APIへのトラフィックを維持する。
6. 一時タグ付きURLで、Edgeなしconfigの403と正しいEdgeでのready・受付停止・GPS無効・写真/Push無効・同一bundleを確認。
7. 削除ジョブのイメージを更新して実行し、成功を待つ。
8. APIのトラフィックを候補リビジョンへ100%切り替え、通常URLでも確認。
9. 候補検証または切替前の失敗では、旧トラフィックを維持しジョブのイメージを復旧する。切替後の失敗では旧revisionのGPS受付を復活させず、候補revisionと新ジョブを保持して受付停止のまま修正版で復旧する。並行する手動変更は上書きせず、復旧が失敗した場合もActionsを失敗にする。

長期利用のサービスアカウント鍵やDBパスワードをGitHubに保存しません。Workload Identity Federation（WIF）で認証します。
最大1時間の短期アクセストークンをrunnerの一時領域に置き、Cloud Buildへのソース送信から分離します。認証ファイルの除外設定も追加しています。
HTTP probeにGoogle IDトークンは不要です。既存previewはIAM public-invokerで、GoのEdge secretが参加APIを保護します。WIFの短期access tokenは管理API専用です。

注意：候補APIも起動時・常駐の期限削除を実行し、削除ジョブの試験も実データへ作用します。「トラフィックなし」は「DB書き込みなし」ではありません。
ロールバックで、期限削除された行を復元したりDB構造を巻き戻したりはしません。

## 2. 作業前の確認

- [手動移行手順](google-cloud.md#5-マイグレーション--権限付与--api)を実施し、連続schema001〜016とruntime権限、ready、清掃を実環境で確認すること。2026-10-28時点の実DBは003で、自動化の前提をまだ満たしません。
- `trace-backend-preview`と`trace-cleanup`が同一immutable digestを使い、最新ready revisionへ100%配信すること。
- APIは`trace-preview-runtime`、cleanupは`trace-runtime`。DBは`trace_app`、app-passwordは同じ数値版。所有者資格は渡さないこと。
- `trace-backend`はmanual 0・反映完了・タグなし。2 APIを同時稼働させないこと。
- previewはservice-level max1/min0、1 vCPU/512MiB、request-based CPU。revision max12でもservice max1で制限されるが、revision maxだけでは十分でないこと。
- 受付・端末写真・旧写真upload・Push・到着・描画の各flagを明示falseにすること。常駐Push/描画の本番用CPU・最小台数をこの閉鎖profileで保証しないこと。
- 既存のinvoker IAM checkと`allUsers:roles/run.invoker`を維持すること。CIはIAMを変更しないこと。
- `DATABASE_URL`や旧`TRACE_SESSION_TTL`を設定せず、DB_*と3つのbundle版をAPI/cleanupで一致させること。
- `TRACE_EDGE_SHARED_SECRET`は`trace-preview-edge-secret`の数値版、GitHub Secret `STAGING_EDGE_SHARED_SECRET`は同じ値であること。必須であり、未設定時の無保護互換はないこと。
- 過去のschema001だけのSHAは現在の適用証拠ではない。mainのmergeだけでbaselineを進めないこと。

以降は管理者がCloud Shellで実行する一度限りのセットアップです。リソース作成・権限付与・課金が発生します。
同名リソースが既にある場合はまず内容を確認し、その作成コマンドだけ省略してください。削除して作り直したり、エラーを無視して次へ進んだりしないでください。
GitHubにpushする前は `STAGING_DEPLOY_ENABLED` を未設定またはfalseのままにします。

## 3. ビルド用・デプロイ用アカウントを用意する

```bash
export TRACE_CI_PROJECT="trace-staging-async5855"
export TRACE_CI_REGION="asia-northeast1"
export TRACE_CI_BUILD_SA="trace-build@${TRACE_CI_PROJECT}.iam.gserviceaccount.com"
export TRACE_CI_DEPLOY_SA="trace-github-deploy@${TRACE_CI_PROJECT}.iam.gserviceaccount.com"
export TRACE_CI_API_RUNTIME_SA="trace-preview-runtime@${TRACE_CI_PROJECT}.iam.gserviceaccount.com"
export TRACE_CI_CLEANUP_RUNTIME_SA="trace-runtime@${TRACE_CI_PROJECT}.iam.gserviceaccount.com"
export TRACE_CI_SOURCE_BUCKET="gs://${TRACE_CI_PROJECT}-github-build-source"

gcloud services enable iam.googleapis.com iamcredentials.googleapis.com sts.googleapis.com \
  cloudbuild.googleapis.com run.googleapis.com artifactregistry.googleapis.com \
  cloudresourcemanager.googleapis.com storage.googleapis.com logging.googleapis.com \
  --project="$TRACE_CI_PROJECT"

gcloud iam service-accounts create trace-build --project="$TRACE_CI_PROJECT" \
  --display-name="TRACE image builder"
gcloud iam service-accounts create trace-github-deploy --project="$TRACE_CI_PROJECT" \
  --display-name="TRACE staging deployment"
```

ソース専用バケットを作成します。アプリの写真等を置くバケットではありません。

```bash
gcloud storage buckets create "$TRACE_CI_SOURCE_BUCKET" --project="$TRACE_CI_PROJECT" \
  --location="$TRACE_CI_REGION" --uniform-bucket-level-access --public-access-prevention
```

リポジトリのルートで、7日を過ぎたビルド用ソースを自動削除するルールを設定します。対象はこの専用バケットだけです。

```bash
gcloud storage buckets update "$TRACE_CI_SOURCE_BUCKET" --project="$TRACE_CI_PROJECT" \
  --lifecycle-file=deploy/gcp/ci-source-lifecycle.json
```

### ビルド用アカウントへの権限

```bash
gcloud artifacts repositories add-iam-policy-binding trace \
  --project="$TRACE_CI_PROJECT" --location="$TRACE_CI_REGION" \
  --member="serviceAccount:$TRACE_CI_BUILD_SA" --role=roles/artifactregistry.writer
gcloud projects add-iam-policy-binding "$TRACE_CI_PROJECT" \
  --member="serviceAccount:$TRACE_CI_BUILD_SA" --role=roles/logging.logWriter
gcloud storage buckets add-iam-policy-binding "$TRACE_CI_SOURCE_BUCKET" \
  --member="serviceAccount:$TRACE_CI_BUILD_SA" --role=roles/storage.objectViewer
```

### デプロイ用アカウントへの権限

```bash
gcloud projects add-iam-policy-binding "$TRACE_CI_PROJECT" \
  --member="serviceAccount:$TRACE_CI_DEPLOY_SA" --role=roles/cloudbuild.builds.editor
gcloud projects add-iam-policy-binding "$TRACE_CI_PROJECT" \
  --member="serviceAccount:$TRACE_CI_DEPLOY_SA" --role=roles/serviceusage.serviceUsageConsumer
gcloud artifacts repositories add-iam-policy-binding trace \
  --project="$TRACE_CI_PROJECT" --location="$TRACE_CI_REGION" \
  --member="serviceAccount:$TRACE_CI_DEPLOY_SA" --role=roles/artifactregistry.reader
gcloud storage buckets add-iam-policy-binding "$TRACE_CI_SOURCE_BUCKET" \
  --member="serviceAccount:$TRACE_CI_DEPLOY_SA" --role=roles/storage.objectUser
gcloud storage buckets add-iam-policy-binding "$TRACE_CI_SOURCE_BUCKET" \
  --member="serviceAccount:$TRACE_CI_DEPLOY_SA" --role=roles/storage.legacyBucketReader

gcloud iam service-accounts add-iam-policy-binding "$TRACE_CI_BUILD_SA" \
  --project="$TRACE_CI_PROJECT" --member="serviceAccount:$TRACE_CI_DEPLOY_SA" \
  --role=roles/iam.serviceAccountUser
gcloud iam service-accounts add-iam-policy-binding "$TRACE_CI_API_RUNTIME_SA" \
  --project="$TRACE_CI_PROJECT" --member="serviceAccount:$TRACE_CI_DEPLOY_SA" \
  --role=roles/iam.serviceAccountUser
gcloud iam service-accounts add-iam-policy-binding "$TRACE_CI_CLEANUP_RUNTIME_SA" \
  --project="$TRACE_CI_PROJECT" --member="serviceAccount:$TRACE_CI_DEPLOY_SA" \
  --role=roles/iam.serviceAccountUser

gcloud run services add-iam-policy-binding trace-backend-preview \
  --project="$TRACE_CI_PROJECT" --region="$TRACE_CI_REGION" \
  --member="serviceAccount:$TRACE_CI_DEPLOY_SA" --role=roles/run.developer
# Read the stopped service only; do not give CI permission to restart it.
gcloud run services add-iam-policy-binding trace-backend \
  --project="$TRACE_CI_PROJECT" --region="$TRACE_CI_REGION" \
  --member="serviceAccount:$TRACE_CI_DEPLOY_SA" --role=roles/run.viewer
gcloud run jobs add-iam-policy-binding trace-cleanup \
  --project="$TRACE_CI_PROJECT" --region="$TRACE_CI_REGION" \
  --member="serviceAccount:$TRACE_CI_DEPLOY_SA" --role=roles/run.developer
```

Cloud Runの変更権限は既存サービス・既存ジョブに限定しています。Owner、Editor、Secret Accessor、Cloud SQL Client、Scheduler Adminは追加しません。
`trace-migration` を使用する権限も付けません。ビルド用アカウントにもRun・DB・Secretへの権限を付けません。
ただしデプロイ権限はアプリとしてコードを実行できる強い権限です。mainへのpush権限とCI設定変更のレビューを厳重に管理してください。

## 4. GitHubからの鍵なし認証を設定する

GitHub認証済みのGitHub CLIで、リポジトリと所有者の数値IDを取得します。秘密情報ではありません。
Cloud Shellに`gh`がなければ、GitHub CLIが使える端末で取得し、数値だけCloud Shellへ渡してください。PATやサービスアカウント鍵をチャットへ貼らないでください。

```bash
gh api repos/Mino829/TRACE_B --jq '{repository_id: .id, owner_id: .owner.id}'
```

Cloud Shellで、取得した2つの数値を入力してから以下を進めます。

```bash
read -r -p "GitHub repository_id: " TRACE_GITHUB_REPO_ID
read -r -p "GitHub owner_id: " TRACE_GITHUB_OWNER_ID
[[ "$TRACE_GITHUB_REPO_ID" =~ ^[0-9]+$ && "$TRACE_GITHUB_OWNER_ID" =~ ^[0-9]+$ ]]
```

数値以外なら進めないでください。以下の条件はリポジトリ名だけではなく、再利用されない数値IDにも固定します。

```bash
TRACE_CI_PROJECT_NUMBER=$(gcloud projects describe "$TRACE_CI_PROJECT" --format='value(projectNumber)')

gcloud iam workload-identity-pools create trace-github \
  --project="$TRACE_CI_PROJECT" --location=global --display-name="TRACE GitHub Actions"

gcloud iam workload-identity-pools providers create-oidc trace-backend \
  --project="$TRACE_CI_PROJECT" --location=global --workload-identity-pool=trace-github \
  --issuer-uri="https://token.actions.githubusercontent.com" \
  --attribute-mapping="google.subject=assertion.sub,attribute.repository_id=assertion.repository_id,attribute.repository_owner_id=assertion.repository_owner_id,attribute.repository=assertion.repository,attribute.ref=assertion.ref,attribute.event_name=assertion.event_name,attribute.workflow_ref=assertion.workflow_ref" \
  --attribute-condition="assertion.repository_id == '${TRACE_GITHUB_REPO_ID}' && assertion.repository_owner_id == '${TRACE_GITHUB_OWNER_ID}' && assertion.repository == 'Mino829/TRACE_B' && assertion.ref == 'refs/heads/main' && assertion.event_name == 'push' && assertion.workflow_ref == 'Mino829/TRACE_B/.github/workflows/test.yaml@refs/heads/main'"

gcloud iam service-accounts add-iam-policy-binding "$TRACE_CI_DEPLOY_SA" \
  --project="$TRACE_CI_PROJECT" --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/projects/${TRACE_CI_PROJECT_NUMBER}/locations/global/workloadIdentityPools/trace-github/attribute.repository_id/${TRACE_GITHUB_REPO_ID}"

gcloud iam workload-identity-pools providers describe trace-backend \
  --project="$TRACE_CI_PROJECT" --location=global --workload-identity-pool=trace-github \
  --format='value(name)'
```

最後の `projects/数値/locations/global/workloadIdentityPools/trace-github/providers/trace-backend` を控えます。
GitHub Actionsの設定だけでなく、Google側でも特定リポジトリ・main・push・workflowファイルに制限しています。
認証条件の反映には数分かかることがあります。403を解決するために条件を消したりOwnerを付けたりしないでください。

## 5. GitHubで有効化する

1. `Mino829/TRACE_B` → Settings → Secrets and variables → Actions → Variables を開く。
2. Repository variable `GCP_WORKLOAD_IDENTITY_PROVIDER` に、上で控えたproviderのフル名を設定する。
3. `STAGING_DEPLOY_ENABLED`は未設定/falseに保ち、[manual rollout](google-cloud.md#5-マイグレーション--権限付与--api)で旧両API/Scheduler停止、停止後backup、004〜016、runtime-grants、preview/cleanup更新と権限・清掃・Edge/Worker疎通を実環境で検証する。
4. 実DBへ適用・検証したschemaと権限に対応する **mainのフルコミットSHA** をRepository variable `STAGING_SCHEMA_BASELINE` に設定する。旧version 1のSHAやPRのSHAを、検証済みの代わりに設定しない。未設定・不正値ならCIはクラウド操作前に停止する。
5. その状態でレビュー済みの変更をmainへpushし、`test`ジョブの成功を確認する。Repository variable `STAGING_DEPLOY_ENABLED` を文字列 `true` にするのは、手動配備の疎通とrollback確認が済んだ後だけにする。
6. 現在のmainに対するpush実行を再実行するか、次のレビュー済み変更をmainへpushする。PR実行の再実行ではデプロイされない。
7. Actionsの `Deploy staging after tests` が成功し、Summaryにコミット・digest・リビジョンが表示されることを確認する。
8. Cloud Runの配信先がそのリビジョンであること、削除ジョブが同じdigestで動くことを確認する。

`STAGING_SCHEMA_BASELINE`は常に明示設定が必要です。migration実装・SQL・権限SQLに差分があれば、自動化停止→停止窓で手動適用→実権限・ready・互換性・復旧確認の後に、そのschemaに対応するmainのフルSHAへ更新します。現在の003を016適用済みとみなす設定、mergeだけのbaseline更新は禁止です。
GitHub Environmentの有料プラン機能には依存していません。利用できる場合はmainの保護ルールとCIファイルへのレビュー要件を追加してください。
依存Actionsは検証時のコミットSHAで固定しているため、更新はレビューして行います。

### 必須のEdge共有secretによるprobe

| 資格情報 | 役割 | CIでの扱い |
|---|---|---|
| WIFの短期access token | Cloud Build/Run管理API | runner一時領域のみ。APIの参加者Authorizationには送らない |
| `STAGING_EDGE_SHARED_SECRET` | Goの参加APIへのWorker/CI認証 | 環境変数から取り出し`X-Trace-Edge-Secret`で送る。値をstate/output/Summaryへ保存しない |
| 参加者Bearer / Google・描画資格 | 本人操作・展示 | 受付停止の配備probeでは使わない |

Cloud Runは`trace-preview-edge-secret:<numeric-version>`を参照し、管理者は同じ値をRepository **Secret**へ登録します。Variable、ソース、コマンド引数、チャットへ置きません。CI用の値は32文字以上の空白・改行なしの可視ASCIIです。CLIで手動確認するときも[秘匿環境入力](google-cloud.md#5-マイグレーション--権限付与--api)を使います。

CIアカウントにSecret Accessorを与えません。API側の平文/未指定/非数値参照、GitHub側の欠落、不正ヘッダー値はbuild前に拒否します。値の不一致はEdgeありconfigの403で停止し、未検証候補へ通常trafficを切り替えません。Edgeなしconfigが成功した場合も拒否します。public healthの200は許容します。redirectに追従せず、別originへ資格を渡しません。

Secretローテーションは自動化を止め、進行中の配備終了後に両保管先を同期して確認します。workflow/ mainの変更権限とレビューを保護し、PRのtestジョブには秘密値を渡しません。


### 読み取り専用の配備前確認

以下はGitHub実行identityなしで使えます。管理APIへの書込み、DB credential取得、一時Job作成は行いません。

```sh
python3 deploy/gcp/ci_staging.py check-maintenance
# Active profile uses TRACE_STAGING_EDGE_SHARED_SECRET from a hidden input.
python3 deploy/gcp/ci_staging.py check-active
```

`check-maintenance`は両APIのmanual 0/反映完了/タグなし、Scheduler pause、cleanup非terminal実行なしを検証します。意図して停止したサービスのserving Readyを要求しません。`check-active`はprivate停止、preview/cleanupのprofile・同digest・bundle・秘密参照・SA、実HTTPのEdge拒否/許可と受付停止を確認します。現在の旧環境は2 API稼働なので、停止不足で拒否されるのが期待結果です。

Cloudflare構成の採用は[設計PR #22](https://github.com/Mino829/trace-campus-installation/pull/22)で承認済みです。これは実配備・参加者E2E・大学/運営による公開承認とは別です。正しいWorker configだけで参加→本人メタデータ→到着/再生→停止/削除を確認済みとはせず、Backend #5は閉じません。


## 6. 運用と復旧

- 自動化停止は`STAGING_DEPLOY_ENABLED=false`。実行中Jobは止まらないため完了を待ち、手動変更と重ねない。
- releaseは直列実行。古いmain実行や外部image/config/traffic/tag変更は拒否し、別作業を上書きしない。
- schema/grants変更は[停止窓の手順](google-cloud.md#5-マイグレーション--権限付与--api)で明示適用。DBの自動downgradeや適用済みSQLの書換えはしない。
- migration/grants/serveの失敗ではSchedulerをpause、privateをmanual 0のまま維持する。旧003/GPS/画像受付版を再起動しない。
- 候補検証前やcleanup更新の失敗では、配信中の確認済み旧版を維持し、変更したcleanup imageだけを復旧する。期限清掃で消えた行は戻らない。
- 切替を試行した後の不確実な失敗では、検証済みGPSなし候補と新cleanupを維持しfix-forwardする。旧GPS writerへtrafficを戻さない。外部変更や復旧失敗もActionsを失敗にする。
- 候補失敗後はlatest template/Readyが未昇格候補を指す場合があり、次回prepareが拒否され得る。自動化を止め、同schemaの確認済みGPSなし版で状態を揃えてから再開する。
- 一時タグは当該runが所有する候補だけを外す。未知のタグは勝手に削除しない。タグだけで到達可能な旧revisionをmanual 0のまま残さない。
- 強制キャンセル・runner消失・権限喪失時は自動復旧を保証しない。実状態、タグ、Jobを確認し、同schema対応の修正版で復旧する。
- このCIはmigration/grants差分を検知するが、任意のGoコードのDB変更を完全に防ぐsecurity boundaryではない。main/workflowをレビューする。
- production、参加受付/新feature開始、Secret/同意/Graph/Origin変更、IAM/backup/Scheduler/監視設定変更は自動化しない。
- build/ソース保管/候補起動/Jobに料金が発生する。閉鎖previewはmin0/request-based CPUを維持する。常駐Push/描画の費用・可用性は別途判断する。

位置キャッシュ、レート制限、描画接続数は複数プロセスで共有されません。候補URLの内部probeは一般参加者への並列公開ではなく、全featureを閉じたまま検証します。追跡中/描画中/実参加受付中にこの閉鎖profileを流用しません。

## 7. ローカル検証

```bash
python3 -m unittest discover -s deploy/gcp/tests -v
go test ./...
go vet ./...
```

PythonテストではCloud操作を模擬し、Edge欠落・不一致・redirect拒否をloopback HTTPでも検証します。外部Cloud通信や実配備はしません。manualの入力/backup鮮度/タグによる停止漏れも検証します。
GoのAPI/DB統合テストは、専用DBを指定した `make integration` で別途確認します。GitHubのtestジョブではPostgreSQLを起動して実行します。
実際のWIF・IAM・Cloud Build・Cloud Runの疎通は、セットアップ後の初回Actions実行で検証する必要があります。

## 公式資料

- [テスト成功を待つGitHub Actionsのジョブ依存](https://docs.github.com/en/actions/how-tos/write-workflows/choose-what-workflows-do/use-jobs)
- [GitHubからGoogle Cloudへの認証](https://github.com/google-github-actions/auth)
- [WIFと数値IDによる制限](https://docs.cloud.google.com/iam/docs/workload-identity-federation-with-deployment-pipelines)
- [専用Cloud Buildアカウントとログ保存](https://docs.cloud.google.com/build/docs/securing-builds/configure-user-specified-service-accounts)
- [Cloud Runの段階的配信・タグ・ロールバック](https://docs.cloud.google.com/run/docs/rollouts-rollbacks-traffic-migration)
