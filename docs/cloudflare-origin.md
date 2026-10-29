# Cloudflare参加者サイトからGo APIへの接続

- 状態: staging統合向け実装。一般参加者への公開手順ではない
- Web: `TRACE_F`のサイトWorker。本番候補`trace-log`とstagingの`trace-staging-preview`を分離
- API: このリポジトリのGoサーバーをCloud Run東京で実行
- 正本DB: Cloud SQL for PostgreSQL東京

## 認証境界

サイトWorkerは許可した/api/backend/*だけをGoの/v1・/v2へ転送し、サーバー側Edge keyを追加します。本人Bearerは別途必要です。Frontend側の許可対象は/v2の写真metadata作成/照会/削除、Memory参加停止、旧Live照会と旧本人写真read/deleteまで拡張し、運営/描画/旧upload/GPS書込は転送しません。GoのEdge例外は既存のdisplay-tokensとdisplay/wsだけで、Google/描画資格と独立Originを検証します。個人operator資格発行もEdge保護対象で、展示PCの認証付きloopback broker等の運営入口が固定経路を中継します。Edge keyはブラウザへ渡さず、OperatorEnabled時は32バイト以上の設定が必須です。

これはWorkerとGoの間のアクセス制限であり、参加者のBearer token、CORS、受付停止、レート制限の代わりにはならない。WorkerはCloudflareが付与した`CF-Connecting-IP`の有効なIPだけを`X-Trace-Client-IP`としてGoへ送り、ブラウザからの`X-Forwarded-For`や同名ヘッダーは破棄する。Goは共有シークレット一致時だけ単一のIPv4/IPv6を制限単位に使い、欠落・不正・複数の同名ヘッダー・IPv6 zone付きなら接続元IPへ戻す。シークレットを設定しない直接到達モードでは転送ヘッダーを一切信用しない。`/health/live`と`/health/ready`はこのヘッダーを要求しない。Workerのsecretをブラウザへ渡さず、Goもヘッダー値をログへ出さない。

2026-10-28の確認では、IAM非公開の`trace-backend`と、既存public-invoker＋Edge認証の`trace-backend-preview`が同じDBへ接続していた。今回の更新計画は**previewを稼働系、privateをmanual 0・タグなしの停止系**とし、既存IAMを変更しない。共有secretだけでIAM非公開サービスへ到達できるわけではない。

Cloudflare Web＋Google Cloud東京は[設計PR #22](https://github.com/Mino829/trace-campus-installation/pull/22)で技術方針として採用済み。実配備・大学/運営による公開承認は別である。[readonly配備前確認](staging-cicd.md#読み取り専用の配備前確認)と[手動配備](google-cloud.md#4-設定とビルド)に従い、APIの`trace-preview-edge-secret:<numeric-version>`とWorker/CIの値を同期する。鍵はブラウザへ渡さない。`serve`はEdge参照必須、全feature無効、既存public-invoker境界を保持し、IAM変更で疎通を通す回避をしない。

## 公開経路を有効化する前の条件

1. stagingとproductionのCloud Run、Cloud SQL、Secret Manager、Worker、ドメイン、データを分離する。
2. Web側の`TRACE_BACKEND_URL`とGo側のsecretを揃え、Worker configとEdge付きGo configは成功、EdgeなしのGo configは403を確認する。healthと描画認証の専用例外を混同しない。
3. `TRACE_REGISTRATION_OPEN=false`で起動し、写真の端末内保存とサーバーメタデータ、旧写真の各`deleteAfter`までの閲覧を含むMemory同意版・文面、連絡先、保存期間を設計Issue #23/#9で承認する。既定の旧クラウド写真草案を再利用しない。
4. 旧両API/Schedulerを停止し、清掃とタグ付き経路を残さず、停止後backup→003から004～018→runtime権限→同一digestのpreview/cleanupへ切り替える。FIFO移行前に待機/再生を整理し、旧写真/fenceのbucket/signerと各deleteAfterの本人閲覧・清掃を維持する。旧upload版を復活させない。
5. Frontend/Workerの実配備で/v2とMemory停止、旧本人read/delete・Live照会/停止を確認する。写真0枚と1枚の到着区間、用途/key IDの不正拒否、期限後の同一到着ID/最新状態回復、個人運営の権限分離/資格失効も確認する。ローカルの実Workerポリシー→Go HTTPと合成Googleのブラウザ検証は済んでいるが、Cloudflare TLS/IAM・実Google・機材の確認は別とする。端末画像の存在/消去をBackendで保証しない。
6. 期限削除ジョブ、DB接続上限、共有NATと429、障害時の受付停止、旧GPS/旧画像受付を復活させない修正版への復旧手順を確認する。

Cloud RunのWebSocketにはリクエスト時間の上限がある。描画用APIを追加するときは、会場クライアントの再接続と状態再同期を検証する。

## 参照

- [Google Cloudの既存配置手順](google-cloud.md)
- [staging CI/CD](staging-cicd.md)
- [Cloud Run WebSocket仕様](https://docs.cloud.google.com/run/docs/triggering/websockets)
