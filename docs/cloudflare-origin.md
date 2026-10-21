# Cloudflare参加者サイトからGo APIへの接続

- 状態: staging統合向け実装。一般参加者への公開手順ではない
- Web: `TRACE_F`の静的サイト用`trace-log` Worker
- API: このリポジトリのGoサーバーをCloud Run東京で実行
- 正本DB: Cloud SQL for PostgreSQL東京

## 認証境界

サイトWorkerは許可した`/api/backend/*`だけをGoの`/v1/*`などへ転送し、`X-Trace-Edge-Secret`を追加する。Goは`TRACE_EDGE_SHARED_SECRET`が設定されている場合、すべての`/v1/*`リクエストで一致を要求する。値が未設定なら既存のAPI動作を維持するため、**公開用Cloud RunではSecret Managerから設定することが必須**。32バイト未満の値では起動を拒否する。

これはWorkerとGoの間のアクセス制限であり、参加者のBearer token、CORS、受付停止、レート制限の代わりにはならない。WorkerはCloudflareが付与した`CF-Connecting-IP`の有効なIPだけを`X-Trace-Client-IP`としてGoへ送り、ブラウザからの`X-Forwarded-For`や同名ヘッダーは破棄する。Goは共有シークレット一致時だけ単一のIPv4/IPv6を制限単位に使い、欠落・不正・複数の同名ヘッダー・IPv6 zone付きなら接続元IPへ戻す。シークレットを設定しない直接到達モードでは転送ヘッダーを一切信用しない。`/health/live`と`/health/ready`はこのヘッダーを要求しない。Workerのsecretをブラウザへ渡さず、Goもヘッダー値をログへ出さない。

既存のstagingはCloud Run IAMで非公開にしている。共有ヘッダーだけではIAM認証を通過できないため、stagingの公開権限を変更せず、検証用の接続経路を別途設計する。本番用サービスも`--allow-unauthenticated`を設定する前に、以下を完了する。既存の`deploy/gcp/deploy.sh`は非公開設定のまま維持する。

最初のstaging配備は非公開Backendを先行し、[認証付き開発者プロキシとCI probe](staging-cicd.md#最初は非公開backendだけを確認する)で確認する。edge secretを有効にする場合は[手動配備設定](google-cloud.md#4-設定とビルド)の`GCP_EDGE_SHARED_SECRET`でSecret Managerの数値版をAPIへ指定し、GitHub Actions Secret `STAGING_EDGE_SHARED_SECRET`と値を同期する。`serve`再実行時も数値版参照を維持する。これはCloudflare案の正式採用ではなく、WorkerからのIAM認証・参加者E2Eは未完了のまま残す。共有secretだけでIAMを迂回したり、接続試験のためにstagingを一般公開したりしない。

## 公開経路を有効化する前の条件

1. stagingとproductionのCloud Run、Cloud SQL、Secret Manager、Worker、ドメイン、データを分離する。
2. Web側の`TRACE_BACKEND_URL`とGo側のsecretを設定し、Worker経由は成功、Goへの直接リクエストは403になることを確認する。
3. `TRACE_REGISTRATION_OPEN=false`で起動し、承認済み同意文、連絡先、保存期間、豊洲キャンパスのgeofenceと原点を設定する。コードの初期座標はサンプル値である。
4. 匿名の参加作成と位置送信には[用途別の制限](../README.md#公開apiのレート制限)を実装済み。公開前にWorkerからのクライアントIP転送、共有NATでの通常参加、429と`Retry-After`、異常な超過、再起動時のカウンター消失をstagingで監視・検証する。単一プロセス内の集計なのでAPI最大1インスタンスを維持し、revision並存を避ける。
5. Cloudflareサイトからの参加開始、位置送信、停止、両セッション削除を実機と検証DBで通す。写真、到着QR、描画WebSocketは実装後に別途試験する。
6. 期限削除ジョブ、DB接続上限、障害時の受付停止、WebとGoのロールバック手順を確認する。

Cloud RunのWebSocketにはリクエスト時間の上限がある。描画用APIを追加するときは、会場クライアントの再接続と状態再同期を検証する。

## 参照

- [Google Cloudの既存配置手順](google-cloud.md)
- [staging CI/CD](staging-cicd.md)
- [Cloud Run WebSocket仕様](https://docs.cloud.google.com/run/docs/triggering/websockets)
