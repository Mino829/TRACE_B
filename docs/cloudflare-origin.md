# Cloudflare参加者サイトからGo APIへの接続

- 状態: staging統合向け実装。一般参加者への公開手順ではない
- Web: `TRACE_F`のサイトWorker。本番候補`trace-log`とstagingの`trace-staging-preview`を分離
- API: このリポジトリのGoサーバーをCloud Run東京で実行
- 正本DB: Cloud SQL for PostgreSQL東京

## 認証境界

サイトWorkerは許可した`/api/backend/*`だけをGoの`/v1/*`や新`/v2/*`へ転送し、`X-Trace-Edge-Secret`を追加する。Goは`TRACE_EDGE_SHARED_SECRET`が設定されている場合、参加者APIと運営APIで一致を要求する。例外は直接ブラウザ認証を行う`/v1/operator/display-tokens`と`/v1/display/ws`の2経路だけで、専用Origin許可リストとGoogle/描画資格を別途検証する（[描画契約](../README.md#描画websocket既定無効)）。値が未設定なら既存のAPI動作を維持するため、**公開用Cloud RunではSecret Managerから設定することが必須**。32バイト未満の値では起動を拒否する。Worker側で`/v2`を許可・転送する改修はこのBackend PRに含まれず、Frontend側の接続まで一般公開できない。

これはWorkerとGoの間のアクセス制限であり、参加者のBearer token、CORS、受付停止、レート制限の代わりにはならない。WorkerはCloudflareが付与した`CF-Connecting-IP`の有効なIPだけを`X-Trace-Client-IP`としてGoへ送り、ブラウザからの`X-Forwarded-For`や同名ヘッダーは破棄する。Goは共有シークレット一致時だけ単一のIPv4/IPv6を制限単位に使い、欠落・不正・複数の同名ヘッダー・IPv6 zone付きなら接続元IPへ戻す。シークレットを設定しない直接到達モードでは転送ヘッダーを一切信用しない。`/health/live`と`/health/ready`はこのヘッダーを要求しない。Workerのsecretをブラウザへ渡さず、Goもヘッダー値をログへ出さない。

2026-10-28の確認では、IAM非公開の`trace-backend`と、既存public-invoker＋Edge認証の`trace-backend-preview`が同じDBへ接続していた。今回の更新計画は**previewを稼働系、privateをmanual 0・タグなしの停止系**とし、既存IAMを変更しない。共有secretだけでIAM非公開サービスへ到達できるわけではない。

Cloudflare Web＋Google Cloud東京は[設計PR #22](https://github.com/Mino829/trace-campus-installation/pull/22)で技術方針として採用済み。実配備・大学/運営による公開承認は別である。[readonly配備前確認](staging-cicd.md#読み取り専用の配備前確認)と[手動配備](google-cloud.md#4-設定とビルド)に従い、APIの`trace-preview-edge-secret:<numeric-version>`とWorker/CIの値を同期する。鍵はブラウザへ渡さない。`serve`はEdge参照必須、全feature無効、既存public-invoker境界を保持し、IAM変更で疎通を通す回避をしない。

## 公開経路を有効化する前の条件

1. stagingとproductionのCloud Run、Cloud SQL、Secret Manager、Worker、ドメイン、データを分離する。
2. Web側の`TRACE_BACKEND_URL`とGo側のsecretを揃え、Worker configとEdge付きGo configは成功、EdgeなしのGo configは403を確認する。healthと描画認証の専用例外を混同しない。
3. `TRACE_REGISTRATION_OPEN=false`で起動し、写真の端末内保存とサーバーメタデータ、旧写真の各`deleteAfter`までの閲覧を含むMemory同意版・文面、連絡先、保存期間を設計Issue #23/#9で承認する。既定の旧クラウド写真草案を再利用しない。
4. 旧両API・Schedulerを停止し、実行中清掃とタグ付き経路を残さず、停止後backup→現在の003から004〜016→runtime権限→同一digestの新preview/cleanupへ切り替える。既存待機・再生はFIFO移行前に整理する。旧写真/fenceがある環境はbucket/signerと各`deleteAfter`の本人閲覧・清掃を維持し、旧upload版を復活させない。
5. Frontend/Workerの実配備で`/v2`許可・転送、本人GET/POST/DELETEと旧本人閲覧・削除を実機結合確認する。旧Live停止・GPS受付拒否・写真0枚の到着も確認する。IndexedDB保存/消去・画像消失表示はFrontendの責務で、Backendで保証しない。
6. 期限削除ジョブ、DB接続上限、共有NATと429、障害時の受付停止、旧GPS/旧画像受付を復活させない修正版への復旧手順を確認する。

Cloud RunのWebSocketにはリクエスト時間の上限がある。描画用APIを追加するときは、会場クライアントの再接続と状態再同期を検証する。

## 参照

- [Google Cloudの既存配置手順](google-cloud.md)
- [staging CI/CD](staging-cicd.md)
- [Cloud Run WebSocket仕様](https://docs.cloud.google.com/run/docs/triggering/websockets)
