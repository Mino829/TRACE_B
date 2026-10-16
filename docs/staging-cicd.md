# 検証環境の自動デプロイ（GitHub Actions → Cloud Build → Cloud Run）

対象は `Mino829/TRACE_B` の `main` と `trace-staging-507216` だけです。本番には使いません。
ファイルの追加だけでは自動デプロイは有効になりません。下記のクラウド側設定とGitHubの有効化が必要です。
Cloud Buildのpushトリガーは作りません。既存Actionsのテストと別トリガーを並行起動すると、テスト結果を待たずに配信してしまうためです。

## 1. 自動化する範囲

1. push / PRで既存のPostgreSQL統合テスト、vet、ビルドとデプロイ処理のオフラインテストを実行。
2. `main`へのpushかつテスト成功時のみ、`STAGING_DEPLOY_ENABLED=true`ならデプロイへ進む。
3. 古いコミット、DB構造変更、公開設定、受付開始、想定外のDBユーザー等を検出したら停止。認証なしの実際のHTTP呼び出しが拒否されることも確認する。
4. Cloud Build専用アカウントでイメージを作成。SHA＋実行ID＋再試行番号の一意タグを使い、配信はdigestで固定。
5. 現在の環境変数・Secret参照・サービスアカウント・CPU等を保持し、APIのイメージだけ更新。最初は既存APIへのトラフィックを維持する。
6. 一時タグ付きURLへ認証して `/health/ready` と `registrationOpen=false` を確認。
7. 削除ジョブのイメージを更新して実行し、成功を待つ。
8. APIのトラフィックを候補リビジョンへ100%切り替え、通常URLでも確認。
9. 失敗時は配信先と削除ジョブの旧イメージを復旧する。並行する手動変更は上書きせず、復旧が失敗した場合もActionsを失敗にする。

長期利用のサービスアカウント鍵やDBパスワードをGitHubに保存しません。Workload Identity Federation（WIF）で認証します。
最大1時間の短期アクセストークンをrunnerの一時領域に置き、Cloud Buildへのソース送信から分離します。認証ファイルの除外設定も追加しています。
呼び出し用IDトークンは候補確認時と切替直前に別途取得します。タグ付きURLへ呼ぶ場合も、audienceは通常のサービスURLです。

注意：候補APIも起動時・常駐の期限削除を実行し、削除ジョブの試験も実データへ作用します。「トラフィックなし」は「DB書き込みなし」ではありません。
ロールバックで、期限削除された行を復元したりDB構造を巻き戻したりはしません。

## 2. 作業前の確認

- 初回手動デプロイ済みの `trace-backend`、`trace-cleanup` が存在すること。
- 両方とも `trace-runtime`、DBユーザー `trace_app`、`trace-app-db-password` の数値バージョンを使用すること。
- 受付はfalse。サービスのIAM認証は有効で、allUsers / allAuthenticatedUsersへのバインディングがないこと。
- 現在のサービスが正常で、トラフィックが既存の1リビジョンへ100%向いていること。
- `DATABASE_URL` は設定しないこと（DB_*設定を上書きするため）。
- 初回DB構造の確認済みコミットは `624ecc1bba4a7c61529b609a1110facf58d38746`。この構造を変更するコードは自動更新を停止する。

以降は管理者がCloud Shellで実行する一度限りのセットアップです。リソース作成・権限付与・課金が発生します。
同名リソースが既にある場合はまず内容を確認し、その作成コマンドだけ省略してください。削除して作り直したり、エラーを無視して次へ進んだりしないでください。
GitHubにpushする前は `STAGING_DEPLOY_ENABLED` を未設定またはfalseのままにします。

## 3. ビルド用・デプロイ用アカウントを用意する

```bash
export TRACE_CI_PROJECT="trace-staging-507216"
export TRACE_CI_REGION="asia-northeast1"
export TRACE_CI_BUILD_SA="trace-build@${TRACE_CI_PROJECT}.iam.gserviceaccount.com"
export TRACE_CI_DEPLOY_SA="trace-github-deploy@${TRACE_CI_PROJECT}.iam.gserviceaccount.com"
export TRACE_CI_RUNTIME_SA="trace-runtime@${TRACE_CI_PROJECT}.iam.gserviceaccount.com"
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
gcloud iam service-accounts add-iam-policy-binding "$TRACE_CI_RUNTIME_SA" \
  --project="$TRACE_CI_PROJECT" --member="serviceAccount:$TRACE_CI_DEPLOY_SA" \
  --role=roles/iam.serviceAccountUser

gcloud run services add-iam-policy-binding trace-backend \
  --project="$TRACE_CI_PROJECT" --region="$TRACE_CI_REGION" \
  --member="serviceAccount:$TRACE_CI_DEPLOY_SA" --role=roles/run.developer
gcloud run services add-iam-policy-binding trace-backend \
  --project="$TRACE_CI_PROJECT" --region="$TRACE_CI_REGION" \
  --member="serviceAccount:$TRACE_CI_DEPLOY_SA" --role=roles/run.invoker
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
3. 初回は `STAGING_DEPLOY_ENABLED=false` のまま、この変更をレビューしてmainへpushし、`test`ジョブの成功を確認する。
4. Repository variable `STAGING_DEPLOY_ENABLED` を文字列 `true` にする。
5. 現在のmainに対するpush実行を再実行するか、次のレビュー済み変更をmainへpushする。PR実行の再実行ではデプロイされない。
6. Actionsの `Deploy staging after tests` が成功し、Summaryにコミット・digest・リビジョンが表示されることを確認する。
7. Cloud Runの配信先がそのリビジョンであること、削除ジョブが同じdigestで動くことを確認する。

`STAGING_SCHEMA_BASELINE` は通常は未設定で構いません。migration実装・SQL・権限SQLに変更がある場合は自動デプロイを止め、手動適用・権限・ready・旧版互換性を確認した後に限り、そのDB構造に対応するmainのフルコミットSHAへ更新します。コード変更をマージしただけではbaselineを進めません。
GitHub Environmentの有料プラン機能には依存していません。利用できる場合はmainの保護ルールとCIファイルへのレビュー要件を追加してください。
依存Actionsは検証時のコミットSHAで固定しているため、更新はレビューして行います。

## 6. 運用と復旧

- 自動化を止める：Repository variable `STAGING_DEPLOY_ENABLED=false`。実行中のジョブは止まらず、新しい実行から無効になる。
- デプロイは直列実行。新しいmainがある古い実行は安全側に倒して失敗終了する。途中のpending実行はGitHubによって置き換えられることがある。
- 手動デプロイやDB変更は同じタイミングで行わない。自動化を無効化し、実行中デプロイが完了してから作業する。
- DB変更：自動化を止め、レビュー済みの後方互換migration・必要な権限付与を手動で行い、旧APIと新APIの互換性を確認してからbaselineを進める。適用済みSQLの編集やDBの自動ダウングレードは行わない。
  1. `STAGING_DEPLOY_ENABLED=false`にし、進行中の配信・清掃ジョブが終わったことを確認する。DBをバックアップし、停止できる旧API・清掃ジョブ、直前イメージとschema版を記録する。version 1の存在しか調べない以前のバイナリは新版schemaを検知できないため、非互換変更の前に停止・置換する。
  2. 適用済み`001_initial.sql`は改変せず、`002_名前.sql`以降を連番で追加する。まず専用DBで初回/追加/再実行/競合/失敗時のrollbackを検証し、stagingでトランザクションとロック時間を確認する。DDLはexpand-contractとし、非互換の旧アプリを同じDBで稼働させない。DB更新後、互換性のある旧版へしかアプリを戻せない。
  3. migration用の新しいイメージを手動ビルドして`GCP_IMAGE`へ指定し、所有者の`bash deploy/gcp/deploy.sh migrate`を実行する。`schema_migrations`の版が1から連続し、期待版まで適用されたことを確認する。失敗したら旧APIへトラフィックを移さず原因を調べる（DDLはトランザクションごとrollback、DBを自動で巻き戻さない）。
  4. `trace_owner`で`deploy/gcp/runtime-grants.sql`を適用し、追加テーブルには明示的に必要な`trace_app`権限だけを付与する。`trace_app`接続で`schema_migrations`のSELECT、新規テーブルの必要操作、DDL不可を確認する。DB接続が成功しても権限の誤設定はready検査だけでは見つからない。
  5. 同じ版に対応する新APIを一時タグへデプロイし、`/health/ready`と受付停止設定、既存E2Eを確認した後に昇格する。新しいschemaを扱えない旧API/清掃ジョブはready/起動に失敗する（新版判定実装済みのバイナリのみ）。再実行は適用済み版を飛ばす。
  6. 手動検証を記録して`STAGING_SCHEMA_BASELINE`を対応するmainのフルSHAへ更新し、自動化を再開する。単なるアプリrollbackはDBを戻さないため、直前イメージが現行schemaに対応できない場合は切り戻さず受付停止を維持し、forward migrationなどの復旧計画を決める。
  今回の`002_remove_live_positions.sql`は例外として旧APIと非互換で、位置テーブルの既存データを破棄する。追跡受付を停止し、旧APIと旧清掃ジョブの稼働・トラフィックを止めてから適用する。`TRACE_SESSION_TTL`を削除し、`TRACE_MEMORY_SESSION_TTL`/`TRACE_LIVE_SESSION_TTL`を設定した新APIだけを起動する。旧イメージへの自動rollbackはできない。APIの最大インスタンス数を1へ変更してからCIを再開する。
- スクリプトはmigration実装・SQLディレクトリ・権限SQLの差分を検知するもので、任意のGoコードが行うDB変更を完全に検知するセキュリティ境界ではない。
- 候補起動で失敗：通常のAPIトラフィックは旧版のまま。一時タグは終了処理で外す。
- 削除ジョブまたは切替後の確認で失敗：旧配信先・旧削除イメージへ復旧を試みる。元の失敗はActions上で失敗のまま残す。
- 強制キャンセル・runner消失・権限喪失時は復旧を保証できない。Cloud Runの実状態を確認する。旧リビジョン・旧イメージは削除しない。
- rollback後の既存サービスの最新作成リビジョンが失敗状態なら、自動化の事前確認も停止する。手動で正常な設定を再デプロイしてから再開する。
- 本番、一般公開、受付開始、ドメイン/CORS/同意文/座標の変更、Secretローテーション、Scheduler設定変更、監視アラート作成はこの自動化の対象外。
- Cloud BuildのビルドログはCloud Loggingで確認する。CIアカウントはログ本文の閲覧権限を持たず、ビルド結果だけ参照する。
- ビルド、ソース保管、候補リビジョンの起動、ジョブ実行等に料金が発生する。現在の最小1台・CPU常時割当は維持する。

Goメモリの最新位置は清掃ジョブや別revisionと共有できない。CIの候補revisionと旧revisionが一時的に並存するため、stagingの`TRACE_REGISTRATION_OPEN=false`を維持し、追跡利用者がいない時間に配信する。本番は追跡中の段階的配信・スケールアウトを行わず、位置の欠落を許容できる停止窓で切り替える。

## 7. ローカル検証

```bash
python3 -m unittest discover -s deploy/gcp/tests -v
go test ./...
go vet ./...
```

PythonテストはGoogle CloudとHTTP応答をすべて模擬し、実際のデプロイ・DB削除を行いません。
GoのAPI/DB統合テストは、専用DBを指定した `make integration` で別途確認します。GitHubのtestジョブではPostgreSQLを起動して実行します。
実際のWIF・IAM・Cloud Build・Cloud Runの疎通は、セットアップ後の初回Actions実行で検証する必要があります。

## 公式資料

- [テスト成功を待つGitHub Actionsのジョブ依存](https://docs.github.com/en/actions/how-tos/write-workflows/choose-what-workflows-do/use-jobs)
- [GitHubからGoogle Cloudへの認証](https://github.com/google-github-actions/auth)
- [WIFと数値IDによる制限](https://docs.cloud.google.com/iam/docs/workload-identity-federation-with-deployment-pipelines)
- [専用Cloud Buildアカウントとログ保存](https://docs.cloud.google.com/build/docs/securing-builds/configure-user-specified-service-accounts)
- [Cloud Runの段階的配信・タグ・ロールバック](https://docs.cloud.google.com/run/docs/rollouts-rollbacks-traffic-migration)
