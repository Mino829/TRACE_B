# Cloudflare参加者サイトからGo APIへの接続

- 状態: staging統合向け実装。一般参加者への公開手順ではない
- Web: `TRACE_F`の静的サイト用`trace-log` Worker
- API: このリポジトリのGoサーバーをCloud Run東京で実行
- 正本DB: Cloud SQL for PostgreSQL東京

## 認証境界

サイトWorkerは許可した`/api/backend/*`だけをGoの`/v1/*`や新`/v2/*`へ転送し、`X-Trace-Edge-Secret`を追加する。Goは`TRACE_EDGE_SHARED_SECRET`が設定されている場合、両版のすべてのAPIで一致を要求する。値が未設定なら既存のAPI動作を維持するため、**公開用Cloud RunではSecret Managerから設定することが必須**。32バイト未満の値では起動を拒否する。Worker側で`/v2`を許可・転送する改修はこのBackend PRに含まれず、Frontend側の接続まで一般公開できない。

これはWorkerとGoの間のアクセス制限であり、参加者のBearer token、CORS、受付停止、レート制限の代わりにはならない。WorkerはCloudflareが付与した`CF-Connecting-IP`の有効なIPだけを`X-Trace-Client-IP`としてGoへ送り、ブラウザからの`X-Forwarded-For`や同名ヘッダーは破棄する。Goは共有シークレット一致時だけ単一のIPv4/IPv6を制限単位に使い、欠落・不正・複数の同名ヘッダー・IPv6 zone付きなら接続元IPへ戻す。シークレットを設定しない直接到達モードでは転送ヘッダーを一切信用しない。`/health/live`と`/health/ready`はこのヘッダーを要求しない。Workerのsecretをブラウザへ渡さず、Goもヘッダー値をログへ出さない。

既存のstagingはCloud Run IAMで非公開にしている。共有ヘッダーだけではIAM認証を通過できないため、stagingの公開権限を変更せず、検証用の接続経路を別途設計する。本番用サービスも`--allow-unauthenticated`を設定する前に、以下を完了する。既存の`deploy/gcp/deploy.sh`は非公開設定のまま維持する。

最初のstaging配備は非公開Backendを先行し、[認証付き開発者プロキシとCI probe](staging-cicd.md#最初は非公開backendだけを確認する)で確認する。edge secretを有効にする場合は[手動配備設定](google-cloud.md#4-設定とビルド)の`GCP_EDGE_SHARED_SECRET`でSecret Managerの数値版をAPIへ指定し、GitHub Actions Secret `STAGING_EDGE_SHARED_SECRET`と値を同期する。`serve`再実行時も数値版参照を維持する。これはCloudflare案の正式採用ではなく、WorkerからのIAM認証・参加者E2Eは未完了のまま残す。共有secretだけでIAMを迂回したり、接続試験のためにstagingを一般公開したりしない。

## 公開経路を有効化する前の条件

1. stagingとproductionのCloud Run、Cloud SQL、Secret Manager、Worker、ドメイン、データを分離する。
2. Web側の`TRACE_BACKEND_URL`とGo側のsecretを設定し、Worker経由は成功、Goへの直接リクエストは403になることを確認する。
3. `TRACE_REGISTRATION_OPEN=false`で起動し、写真の端末内保存とサーバーメタデータ、旧写真の各`deleteAfter`までの閲覧を含むMemory同意版・文面、連絡先、保存期間を設計Issue #23/#9で承認する。既定の旧クラウド写真草案を再利用しない。
4. 新旧revisionを停止・切替する間に旧写真の署名PUTを止め、旧pendingのfenceとready写真の期限内閲覧、`010`/`011` migration・runtime権限をstagingで確認する。旧バイナリへ戻して旧アップロードを再開しない。
5. Frontend Workerから`/v2`の本人GET/POST/DELETE、旧`/v1`の本人閲覧・削除を実機で確認する。Goの直接リクエストは403とし、旧Live停止・位置受付拒否、写真0枚の到着も確認する。Web AppのIndexedDB保存/消去・画像消失表示はBackendで保証できずFrontend側の責務。
6. 期限削除ジョブ、DB接続上限、共有NATと429、障害時の受付停止、旧GPS/旧画像受付を復活させない修正版への復旧手順を確認する。

Cloud RunのWebSocketにはリクエスト時間の上限がある。描画用APIを追加するときは、会場クライアントの再接続と状態再同期を検証する。

## 参照

- [Google Cloudの既存配置手順](google-cloud.md)
- [staging CI/CD](staging-cicd.md)
- [Cloud Run WebSocket仕様](https://docs.cloud.google.com/run/docs/triggering/websockets)
