# TRACE Backend — PostgreSQL / Google Cloud MVP

匿名参加セッション、新規の端末内写真メタデータ、旧セッションの非公開クラウド写真を扱うGo APIです。旧LivePresence・旧写真の本人操作と期限削除を維持します。DBはPostgreSQL 17、公開環境はGoogle Cloud Run＋Cloud SQL for PostgreSQLを想定しています。

## GPS受付廃止の移行（Backend #26）

| 方針 | 互換性・安全性 | 実装・期限 |
|---|---|---|
| **採用：新規受付と書込み停止、旧データは各`deleteAfter`まで保持** | 旧参加者の状態照会・停止・本人削除を維持。新規GPSは入らず、旧データだけ既定の期限清掃対象 | APIで拒否し、旧DB列と削除処理を残す。SQL migration不要。セッションごとの既存`deleteAfter`が削除期限 |
| 即時消去 | 旧記録を早く減らせるが、本人の写真閲覧権・復元バックアップ・派生物・削除証跡の調整が必要 | DB/ストレージ/バックアップの消去計画、大学の個人情報責任者の承認、本人への説明が必要。実施日未承認 |

Backend担当から採用方針の明示を受けた移行実装です。設計Issue #23の要件改訂、設計Issue #9の大学による同意文・保持期間の承認、Frontend/展示の接続契約はこの判断で承認されたものとは扱いません。

`TRACE_REGISTRATION_OPEN=false`が既定です。参加受付を再開する前に、設計Issue #23のGPSなし要件・Backend #25の自己申告写真地点契約・設計Issue #9の同意文と保持期間を承認し、**旧版とは別の**Memory同意版と承認済み文面を設定してください。既定の同意文と版は未承認の開発用草案であり、公開してはいけません。Frontendの新規受付も承認待ちです。

新規作成はMemorySessionのみ可能です。`enableLivePresence:true`は`409 LIVE_PRESENCE_DISABLED`、既存LiveSessionの`POST .../position`は本人認証後に`410 POSITION_DISABLED`、`POST .../on-site`の`continue:true`は`410 ON_SITE_DISABLED`を返します。旧Liveの停止・状態確認・本人削除は`deleteAfter`まで有効です。旧`/v1`の写真登録はGPS `location`を`400 PHOTO_LOCATION_DISABLED`で拒否してきましたが、端末内写真へ切替後は旧写真の登録自体を停止し、新`/v2`だけが自己申告の`checkpointId`を受け付けます。到着QRはMemoryのみ・写真0枚でも使えます。

新規MemorySessionには受付時のCampusGraph版を固定し、作成レスポンスの`memorySession.campusGraphVersion`で返します。写真登録時に現行版と一致しなければ`409 STATE_CONFLICT`で拒否します。旧Sessionには版がないため新しいチェックポイント写真を追加できませんが、既存写真の閲覧・削除は継続します。写真の保存版が現行Graphと違う場合、同じチェックポイントIDが残っていても新しい座標へ読み替えず、その区間を解決不能として扱います。

旧GPS写真と旧LiveSessionは前倒し消去しません。旧写真の本人閲覧・削除、期限清掃、旧位置のTTLを維持します。端末内写真へ切り替えると、旧写真の新規登録・署名付きPUTの新規発行と再発行・completeは停止します。切替前に発行されたPUT URLはすぐには失効しないため、起動時に旧pendingを消去キューへ入れ、ストレージのゼロバイトfenceと期限後の再消去を完了させます。旧ready写真は各`deleteAfter`まで読めます。旧revisionを止めてからmigration・新revisionへ切り替え、旧版へロールバックして画像書込みを再開しないでください。

旧`/v1/public/config`の位置関連項目は互換性のため維持し、新モードでは写真アップロード設定を返しません。旧写真のGPS由来ルートは閲覧・削除のため維持し、新メタデータの経路は本人`/v2/.../memory`で`pathNodeIds`とGraph版を返します。設計/同意審査とstaging結合検証が済むまで一般公開しないでください。

## 端末内写真への移行（Backend写真メタデータ）

`TRACE_PHOTO_METADATA_ENABLED=false`が既定です。有効化すると**新規**MemorySessionは`photo_mode=device`になり、`GET/POST /v2/memory-sessions/{memorySessionId}/photos`、`DELETE /v2/.../photos/{photoMomentId}`、`GET /v2/.../memory`を本人Bearerで使用します。POSTは`Idempotency-Key`と`clientPhotoId`、固定Graphの`checkpointId`、`capturedAt`だけを受け付けます。画像、ハッシュ、サイズ、位置、URLなどの追加フィールドを拒否し、画像バイトをDB・GCSへ保存しません。本人画面は端末IndexedDBの画像を`clientPhotoId`で照合します。Backendは端末内画像の保存・削除や消失を検証・復旧できません。

同じID/同じ内容の再送は同じ記録、別内容は409です。12枚を上限とし、撮影時刻順のチェックポイント経路を作ります。削除はメタデータ・地点・経路を同一トランザクションで消し、削除済み`clientPhotoId`だけのfenceをMemorySessionの`deleteAfter`まで維持するためオフライン再送は404です。0/1枚の到着も維持します。従来のセッションは`photo_mode=legacy`のまま、GCS設定を保持したときだけ旧`/v1`の写真一覧・本人memory画像閲覧・個別削除を期限まで提供します。新モード中は旧`/v1`の登録・upload-url・completeを503で拒否します。

新モードと`TRACE_PHOTO_ENABLED=true`（旧アップロード）は併用できません。旧画像・署名付きURLのfenceが残る間は、旧アップロードを無効にしても`TRACE_PHOTO_BUCKET`と`TRACE_PHOTO_SIGNER_EMAIL`をAPI・清掃ジョブ双方に残します。画像が存在するのにGCSへアクセスできない状態では起動を拒否します。`TRACE_REGISTRATION_OPEN=true`には新モードと、**既定の草案とは別に**設定した同意版・本文が必要です。設定値が存在するだけでは大学・運営の承認を証明できません。設計PR #25の確定、同意・保持期間の承認、Frontend/Worker・展示の統合試験までは受付を閉じたままにします。

`010_device_photo_moments.sql`と`011_push_thirty_minutes.sql`は明示的に適用し、`deploy/gcp/runtime-grants.sql`を再適用します。後者は旧15分通知の次回予定を15分延ばして30分へ移行します。旧revisionを止め、受付停止・バックアップ・旧画像の残数とfence・migration・権限を確認してから新APIと清掃ジョブを起動してください。切替後は旧URLが全て期限切れとなり、fenceの再消去が完了するまでGCS設定を外せません。

## 実装範囲

- MemorySessionの新規発行と、既存LivePresenceSession / ExperienceLinkの本人操作・別Bearer token認証
- GPSなしの新規登録、旧位置のTTLと停止・期限切れ・削除時の消去
- 旧位置の最新値は量子化済みのGoプロセスメモリでTTL・停止・削除時に消去し、DBに座標を保存しない。新規位置は受付・保存しない
- MemorySession / 旧LivePresenceSessionごとの有効期限と削除期限。期限後も各`deleteAfter`までは本人の状態取得・削除が可能
- 個別削除と両トークンによる体験単位削除。後者はLive側から順に個別に確定し、部分成功を24時間の記録と同じキーで再試行
- DBロックとトランザクションで各削除・冪等性記録を一括確定
- CORS応答（許可Originへのプリフライトでは一括削除用`X-Trace-Live-Token`も許可）、16KB JSON上限、ヘルスチェック、JSONログ
- CloudflareサイトWorker向けの共有シークレット検証（設定時、`/v1/*`と`/v2/*`）
- 明示的なDBマイグレーションと期限削除コマンド
- 新規端末写真のメタデータ・経路を返す本人`/v2` API。旧`/v1`のクラウド写真は期限内の閲覧・削除のみ継続
- 任意同意後のWeb Push撮影リマインダー（既定で無効）：購読情報の暗号化、30分間隔、停止・期限消去
- 既定無効の投影QR到着受付、運営専用署名発行、永続演出キュー、本人向け再生状態
- 既定無効の描画WSS：短期専用資格、snapshot→PostgreSQL連番差分、単一主描画機lease、再生開始・完了・失敗と期限切れ・切断skip

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
| POST | /v1/memory-sessions/{memorySessionId}/arrival |
| POST | /v1/memory-sessions/{memorySessionId}/arrival/skip |
| DELETE | /v1/memory-sessions/{memorySessionId} |
| DELETE | /v1/live-presence-sessions/{liveSessionId} |
| GET / POST | /v1/memory-sessions/{memorySessionId}/photos（新モードではPOST停止） |
| POST | /v1/memory-sessions/{memorySessionId}/photos/{photoMomentId}/upload-url（新モードでは停止） |
| POST | /v1/memory-sessions/{memorySessionId}/photos/{photoMomentId}/complete（新モードでは停止） |
| DELETE | /v1/memory-sessions/{memorySessionId}/photos/{photoMomentId} |
| GET | /v1/memory-sessions/{memorySessionId}/memory |
| GET / POST | /v2/memory-sessions/{memorySessionId}/photos |
| DELETE | /v2/memory-sessions/{memorySessionId}/photos/{photoMomentId} |
| GET | /v2/memory-sessions/{memorySessionId}/memory |
| PUT / PATCH / DELETE | /v1/memory-sessions/{memorySessionId}/push-subscription |
| POST | /v1/operator/arrival-proofs |
| GET | /v1/operator/arrivals |
| POST | /v1/operator/arrivals/{arrivalId}/skip |
| POST | /v1/operator/display-tokens |
| POST | /v1/operator/arrivals/{arrivalId}/retry |
| GET (WSS) | /v1/display/ws |

到着QRと描画WebSocketは機能フラグで個別に有効化します。旧写真ストーリーの経路は保存済み写真GPSとCampusGraphから推定し、端末内写真は`checkpointId`から別のメタデータ経路を作ります。端末内写真の到着時・描画機再接続時・到着後の写真追加/削除時には写真地点とノード経路を描画WSへ投影します。Frontend `/display`での再生、旧写真の到着演出素材、写真ピン・フォトスポット、およびその他の運営APIは未統合です。
新規写真メタデータは既定で無効です。旧写真ストレージは既存データが消えるまで必要であり、コードのローカル検証だけで一般公開や実データ利用の準備完了を意味しません。

### 公開APIのレート制限

| 対象 | 1プロセスでの上限（token bucket） |
|---|---:|
| 参加作成 | クライアントIPごとに60回/分・burst 60、全体10回/秒・burst 10 |
| 公開設定取得 | クライアントIPごとに60回/分・burst 60、全体10回/秒・burst 10 |
| 旧位置送信（常に拒否） | Live参加トークンごとに12回/分・burst 5 |
| 本人状態取得、到着、演出skip、会場内選択、停止、個別・一括削除 | 参加トークンごとに30回/分・burst 30 |
| 写真メタデータ登録 | Memory参加トークンごとに6回/分・burst 6、保持中12件まで |
| Web Push購読・変更・解除 | Memory参加トークンごとに10回/分・burst 10 |
| 到着proof発行・候補照会・運営skip | 有効な運営署名の要求を全体で1回/秒・burst 10 |
| 描画token発行 | 有効な発行署名の要求を全体で1回/秒・burst 10 |
| 公開APIの追加上限 | 認証付きAPIはIPごとに1200回/分・burst 120。全API合計100回/秒・burst 100 |

超過時は`429 RATE_LIMITED`と秒数の`Retry-After`を返し、その間は送信を止めて再試行します。失敗要求も制限対象です。新`/v2`の写真POSTは6回/分、本人GET/DELETEは一般のセッション操作枠30回/分を使います。Pushの購読・変更・解除は別の10回/分枠です。描画WSは1接続あたり10メッセージ/秒、各256KiBで制限します。
許可Originには`Retry-After`をCORSで公開します。一括削除はMemoryのBearer tokenと`X-Trace-Live-Token`の両方の操作枠を同時に確認・消費し、片側が超過していれば削除を開始せず、もう片側の枠も消費しません。位置送信と操作のトークン別枠は分離されていますが、IP別・全体の追加上限は共通です。
IPはWorker経由なら共有シークレット検証後の`X-Trace-Client-IP`（WorkerがCloudflareの`CF-Connecting-IP`から設定）、それ以外は接続元`RemoteAddr`を使用します。`X-Forwarded-For`を信用しません。中継IPしか取得できない場合は参加作成・公開設定のIP制限が中継単位になり得るため、公開前に疎通を確認してください。
転送IPは単一のIPv4/IPv6だけを受け付けます。複数の同名ヘッダー、カンマ区切り、IPv6 zone付きなどは接続元IPへフォールバックします。
カウンターは最大10,000件を1プロセスのメモリに保持し、2分以上使われていないものを消去します。容量超過は`429`（`Retry-After: 60`）で安全側に拒否します。再起動でリセットされ、複数インスタンス間では共有されません。`deploy/gcp/deploy.sh`の最大1インスタンスを維持し、新旧revisionの並存も避けてください。増設する場合は共有レート制限ストアまたは信頼できる共通edgeの制限へ移行します。

## 任意の撮影リマインダー

`TRACE_PUSH_ENABLED=false`が既定です。承認済みの通知文面・別途のブラウザ通知許可・stagingでの実Push確認後だけ有効化してください。MemorySession作成・写真撮影・位置共有には通知が不要です。通知を無効化した場合、新規PUT/PATCHは`503 PUSH_UNAVAILABLE`、DELETEは本人認証後に購読を消去します。無効化して起動すると残存購読を全消去し、外部送信は開始しません。公開設定`notification`は`enabled`、`defaultIntervalMinutes:30`、`allowedIntervalsMinutes:[30]`、`exactDeliveryGuaranteed:false`と、有効化時のみ`vapidPublicKey`を返します。

`TRACE_PUSH_VAPID_PUBLIC_KEY`は非圧縮P-256公開鍵（65 byteのbase64url）、`TRACE_PUSH_VAPID_PRIVATE_KEY`は対応する32 byteの秘密scalar、`TRACE_PUSH_VAPID_SUBJECT`は有効な運営先の`mailto:`、`TRACE_PUSH_ENCRYPTION_KEY`は**VAPIDとは別の**ランダムな32 byte AES-GCM鍵（base64url）です。鍵はSecret ManagerからAPIとworkerへ同一の値を渡し、ソース・通常ログ・運営画面・描画へ出さないでください。古い暗号化鍵を失うと既存購読は復号できません。鍵を変更する場合は購読を一旦消去し、端末に再同意・再購読を求めてください。`005_push_subscriptions.sql`の適用後、`deploy/gcp/runtime-grants.sql`を再適用してからAPIを入れ替えます。
Cloud Runの`deploy/gcp/runtime.env.yaml`には公開鍵と`mailto:`だけを記載し、秘密鍵と暗号化鍵は置かないでください。`deploy/gcp/deploy.sh serve`には別々のSecret Manager参照`GCP_PUSH_VAPID_PRIVATE_SECRET`と`GCP_PUSH_ENCRYPTION_SECRET`を渡すと、APIと清掃ジョブの両方へ秘密環境変数として配布します。実行サービスアカウントのSecret Accessor権限を両secretに限定してください。常駐ディスパッチャはCloud Runの`--min=1 --no-cpu-throttling`に依存します。スケール・revision並存時の重複と配信時刻、外部プロバイダへの実到達性はstagingで確認してください。

Memory Bearer tokenで`PUT /v1/memory-sessions/{memorySessionId}/push-subscription`へ`{"consent":true,"subscription":{"endpoint":"https://...","keys":{"p256dh":"base64url","auth":"base64url"}},"intervalMinutes":30}`を送ります。許可・説明後の参加者操作だけで`consent:true`を送信してください。endpointは許可したHTTPS Push serviceだけを受け付け、外部URLを無条件にフェッチしません。`PATCH`は`{"intervalMinutes":30}`または`{"status":"paused"}` / `{"status":"active"}`を受け付け、`DELETE`（空body）は204で未送信予定と鍵を消去します。PUT/PATCHは`status`、`intervalMinutes`、`nextSendAt`、`expiresAt`だけを返し、endpointや鍵は返しません。

10秒ごとに最大50件を抽出し、DB内のMemoryロックと状態再確認のもと、5秒以内にプロバイダへ送信します。送信時刻は厳密には保証しません。payloadは固定の一般文面と相対URL`/photo`だけで、位置・写真・セッションID・参加記号を含みません。正常2xxでは次回を間隔後へ、404/410では鍵を削除、その他の失敗は1分後に再試行します。Web Pushに配信のexactly-once保証はなく、プロバイダ受理後DB commit失敗時には重複し得ます。写真の初回`ready`時は同じトランザクションで次回を延期します。Memory削除準備・Memory/Live削除・Live停止・到着確定・期限清掃は購読も削除します。到着時は同じMemory Storeトランザクション内で`push_stopped=true`を確定し、再購読を禁止します。単純解除は再購読可能です。

## 投影QRによる到着（既定無効）

`TRACE_ARRIVAL_ENABLED=false`が既定です。有効化には`TRACE_ARRIVAL_VENUE`（会場ID）、`TRACE_ARRIVAL_CHECKPOINT_ID`（ブースID）、異なる32バイト以上の秘密値`TRACE_ARRIVAL_PROOF_KEY`（QR署名専用）、`TRACE_ARRIVAL_ISSUER_KEY`（運営発行API認証専用）が必須です。鍵を参加者サイト/QR/ログへ渡さず、Secret Manager等で配置してください。鍵の切替・失効は設定の更新と稼働revision切替で行い、旧proofと旧運営署名を即時失効させます。複数revisionを同時公開すると鍵・キューの一貫性が失われるため、切替前に受付停止し単一APIインスタンスを維持してください。大学の審査と運営導線が整うまでフラグは有効化しないでください。

運営専用の`POST /v1/operator/arrival-proofs`へ空bodyでHTTPSアクセスします。認証ヘッダー`X-Trace-Issuer-Timestamp`には現在のUnix秒を十進表記、`X-Trace-Issuer-Signature`には`base64url(HMAC-SHA256(TRACE_ARRIVAL_ISSUER_KEY, "POST\n/v1/operator/arrival-proofs\n" + timestamp))`（paddingなし）を設定します。有効期間は前後15秒です。固定鍵自体を送信せず、署名を短命の運営資格として使います。投影機が取得した`checkpointToken`という**文字列だけ**をWeb URLではなくQRにエンコードし、例えば15秒ごとに交換します。`expiresAt`（UTC）到達後は掲示しません。端末はWeb App内のカメラで読み、既存Memoryセッションのまま送信します。会場用機器からのみ運営経路に到達できるよう、リバースプロキシ/Workerにも運営APIのアクセス制御を設定してください。ブラウザのOriginを伴う運営API要求は拒否します。参加用Bearer tokenやEdge共有シークレットだけでは発行できません。

配置先が`TRACE_EDGE_SHARED_SECRET`を要求する場合、運営用プライベート経路も`X-Trace-Edge-Secret`を同時に送ります。サイトWorkerは参加者用経路から`/v1/operator/*`への転送を拒否し、運営用の認可済みプライベート入口を別に用意してください。現行運営監査は個人操作者ではなく資格情報を保有する`arrival_issuer`サービス名、操作時刻、到着IDのハッシュ短縮値、結果を24時間保持します。個人別の監査・運営権限分離は運営UIを構築する際に別途必要です。

発行されるproofは`base64url(JSON).base64url(HMAC-SHA256(TRACE_ARRIVAL_PROOF_KEY, base64url(JSON)))`です。JSONには`checkpointId`、`venue`、`issuedAt`/`expiresAt`（Unix秒）、ランダム`nonce`を含み、有効期間は25秒です。参加端末は`POST /v1/memory-sessions/{memorySessionId}/arrival`へMemory Bearer、`Idempotency-Key`（ランダム値）、JSON `{"checkpointToken":"...","experienceLinkToken":"...","clientTime":"..."}`を送ります。`clientTime`は判定に使わずサーバー時刻を正とします。改変・他会場・期限切れ・未来発行は403、別トークンによる別セッションへの操作は認証で拒否します。投影QRは複数来場者が読むため同一proofを**別Memory間では共有可能**です。同一Memoryへの二重到着は1件のArrivalEventとなり、24時間の同一キー/同一内容の再送は期限切れQRでも元の結果を返します。同一キー/別内容は409、Memory削除と同時に到着・再送記録も消えます。

到着済みMemorySessionへ別のキーと新しい有効なQRで再送した場合は新たな到着を作らず、期限切れで待機中ならDBの`timeout` skipと描画差分を確定し、現在の演出状態を返します。初回receiptと状態が異なる場合もあるため、最新状態は本人`GET /v1/experiences/{experienceId}`から取得します。本人閲覧・削除は演出skip後も維持します。

有効なExperienceLinkを持つ旧MemorySessionでは当該link tokenを照合し、到着イベントと追跡中・来場前の対応Liveセッションの`arrived`更新を同一トランザクションで確定します。Liveを本人が既に停止していてもMemoryの到着は記録し、停止済み追跡は再開しません。位置書込みと`on_site`継続は廃止済みで、旧Liveでも明示的な停止・本人削除はできます。新規MemorySessionはLiveとのlinkなしで写真0枚でも到着できます。`GET /v1/experiences/{experienceId}`には`replayStatus`（未到着なら空文字、到着後`queued`/`playing`/`completed`/`skipped`）だけを追加し、待ち順、推定待ち時間、表示aliasの待機通知は返しません。人の列が待ち順です。

到着は写真機能無効でも有効で、0枚・1枚でもキューに残ります。pending写真があれば最大15秒だけ候補抽出を遅らせ、未完了は候補から除きます。永続FIFOの待機上限は10件（`playing`は含まない）。満杯でも到着自体を記録して演出だけ`skipped`（`queue_full`）とし、到着から30秒以内に再生を開始できない候補は`skipped`（`timeout`）へ遷移します。本人`POST .../arrival/skip`も未再生の演出だけをスキップします。

運営側`GET /v1/operator/arrivals`は同じ署名方式（`GET\n/v1/operator/arrivals\n`）で再生可能な到着IDとready写真**件数**だけを返します。`POST /v1/operator/arrivals/{arrivalId}/skip`は当該パスで署名してスキップできます。`POST /v1/operator/arrivals/{arrivalId}/retry`は描画機障害によるskipに限り、到着から30秒以内・待機枠に余裕がある場合だけ同じ運営署名で明示的に再キュー投入します。再投入で元の到着期限は延長しません。本人GETにキュー順は出しません。演出の状態変更は主描画機leaseを確認したWSSだけが行い、運営候補照会では`playing`へ移りません。

`012_arrival_queue_bounds.sql`は旧キューの期限を到着から30秒へ短縮し、期限切れ・新上限からあふれた到着の演出をskipして同じDBトランザクションで描画差分も保存します。到着・本人用データは削除しません。既存の受付停止・旧API revision停止・バックアップ・`010`〜`012`の明示migration・実行ユーザーへの権限再適用後に切り替えてください。旧Live/位置APIの移行方針は冒頭のBackend #26を参照してください。

## 描画WebSocket（既定無効）

`TRACE_DISPLAY_ENABLED=false`が既定です。有効化するには参加者・到着運営・Edgeとは異なる32バイト以上の`TRACE_DISPLAY_ISSUER_KEY`と`TRACE_DISPLAY_TOKEN_KEY`が必須です。写真の有無を問わず起動時に座標/CampusGraph/3D Map版を埋め込みCampusGraphと照合し、既定の未審査bundleとは不一致なら起動を拒否します。秘密値はSecret Managerで個別管理し、署名鍵をローテーションすると既発行資格を即時失効します。描画PCが外向きHTTPS/WSSでアクセスする**承認済み専用経路**を用意してください。参加者サイトWorkerは`/v1/display/*`と`/v1/operator/*`を転送しないでください。Edge共有シークレットが有効なら承認済み経路から`X-Trace-Edge-Secret`も送ります。WSSトークンをURL、ブラウザ、ログ、写真やQRへ載せないでください。

運営の専用発行器から空bodyの`POST /v1/operator/display-tokens`へ、`X-Trace-Display-Issuer-Timestamp: <Unix秒>`と`X-Trace-Display-Issuer-Signature: base64url(HMAC-SHA256(TRACE_DISPLAY_ISSUER_KEY, \"POST\\n/v1/operator/display-tokens\\n\" + timestamp))`を送ります（時差最大15秒）。レスポンス`displayToken`は2分有効で、参加者Bearer/到着運営署名と互換性がありません。`GET /v1/display/ws`へ`Authorization: Bearer <displayToken>`、`X-Trace-Schema-Version: 1`、`X-Trace-Coordinate-Version`、`X-Trace-Campus-Graph-Version`、`X-Trace-Three-D-Map-Version`（現行設定と完全一致）を付け、Origin・query stringは付けません。不一致はHTTP 409、資格エラーは401、機能無効は503です。

WS接続直後の`display.snapshot`は`data.cursor`、旧版の量子化済み最新位置の`participants`、再生待ち/再生中の`arrivals`を含みます。到着・再生イベントの`eventId`はDB連番で、欠番や再接続時はsnapshotで全置換します。旧`position.updated`、`position.expired`、`tracking.stopped`は各接続で最新のGoメモリ状態から生成する一時的な差分であり、`eventId`は不透明な重複排除キーです。位置差分はDBに保存・再送せず、位置のTTLは最大60秒、接続断後は次のsnapshotで復元します。`participant.arrived`とsnapshotの各`arrivals`には端末内写真の`pathNodeIds`、撮影順の`photoCheckpoints`（`checkpointId`と`capturedAt`のみ）、`replayDurationSeconds`を含めます。到着後の`photo.moment.created/deleted`は同じ到着の**更新後の全経路と写真地点**を送るため、同じ地点・撮影時刻の写真が複数でも描画機は`arrivalId`で丸ごと置換できます。到着前の写真変更は描画へ流さず、最初の到着イベントで現在値を渡します。保存Graph版が現行版と合わない、経路が途切れる場合は経路を空にし、旧GPS写真を新しい端末内経路へ混ぜません。画像、写真URL、PhotoMoment ID、MemorySession ID、元のGPS、参加者資格は含みません。映像へ正確な時刻やIDを表示しないでください。

送信JSONはすべて`schemaVersion:1`、3種のversion、`type`を含めます。`renderer.primary.acquire`で主リースを要求し、`renderer.primary.granted`の`data.leaseId`を保存します。他接続には`renderer.primary.conflict`を返します。`arrival.replay.started`、`arrival.replay.completed`、`arrival.replay.failed`にはクライアント生成の一意な`eventId`（8～128文字）、`arrivalId`、`leaseId`を指定します。成功は`renderer.ack`、競合・失効は`renderer.rejected`です。同じeventId・同じ操作の再送は24時間同じ結果となり、異なる内容は拒否します。切断/30秒heartbeat欠落/リース期限切れは再生中を`skipped`に確定し、別端末による自動再生はしません。運営の署名済み明示retryのみが再投入できます。10秒間隔のping/pong（または`renderer.heartbeat`）と30秒応答期限を設け、50分でWSを終了します。Cloud Run設定は3600秒で、クライアント側は切断時に指数バックオフで**新しい資格を取得し**snapshotから再接続します。

旧版で受け付けた最新の量子化済み位置は単一BackendのGoメモリにのみ保持し、PostgreSQLには保存しません。到着・再生イベントと主描画機leaseだけをDBに保存します。旧位置は新たに受け付けず、TTL切れ後は描画snapshotから消えます。複数Backend間で位置は共有されないため、**APIとWebSocketは同じ1インスタンス**へ接続し、Cloud Runの最大インスタンス数を1としてください。DB migration `007_display.sql`と`deploy/gcp/runtime-grants.sql`を適用してください。設計`docs/api.md`の`participant.arrived` JSON例には`sessionId`と`photoMomentId`が残っていますが、同資料の公開情報制限および`docs/privacy.md`の禁止に従い描画へ送信しません。設計例の修正、旧位置差分の廃止、Frontendでの描画統合は別途必要です。

## 旧クラウド写真の設定とライフサイクル

- `TRACE_PHOTO_ENABLED=false`が既定です。`true`にするには`TRACE_PHOTO_BUCKET`と`TRACE_PHOTO_SIGNER_EMAIL`の両方が必要です。
- 保存先設定があれば、新規写真を無効にしても既存写真の本人閲覧・削除と清掃は継続します。写真・アップロード枠が残る間に保存先設定を外すと、起動・清掃コマンドは拒否します。
- bucketは東京`ASIA-NORTHEAST1`、Uniform bucket-level accessとPublic access preventionを有効にし、versioning・soft delete・retention/hold・独立したlifecycleルールを無効にします。起動時に検査し、稼働中に変更しないでください。
- ADCとIAM `signBlob`を使用し、秘密鍵ファイルやローカルファイル保存へのフォールバックはありません。APIと清掃ジョブへ同じ保存先を設定します。必要権限とCORSは[配置手順](docs/google-cloud.md)を参照してください。

写真はJPEG/WebP、5MiB以下、長辺1920px以下、1セッション12枚までです。新規登録ではGPSの`location`を拒否します。保存後にサイズ・SHA-256・実形式・寸法・画像全体の復号・EXIF等の不在を検証するまで、画像URLは返しません。ブラウザの構造的なJFIF/Adobe情報は許可しますが、埋め込みサムネイル・任意メタデータ・アニメーションは拒否します。

WebPはVP8Lの過大割り当てを修正した公式`x/image v0.47.0`を使い、独自デコーダのコピーは保持しません。公式のentropy group上限・疎なindexの再マッピングに加え、プロセス内のWebP全体復号を同時1件に制限します。待機はcontextで取消可能で、画像とALPHの両方の悪意ある入力を境界テストで検証します。512MiB構成での実負荷・接続待ちへの影響はstagingで確認してください。

登録の同じ`clientPhotoId`と同じ内容は同じPhotoMomentへ集約します。同じ冪等性キーで異なる内容は409です。削除済み写真の登録再送は404となり、復活させません。登録の再送でURL期限が切れていた場合は`uploadUrl`を省略するため、`pending`に対して`upload-url`を要求します。再発行は初回を含め5回まで、上限到達は409です。

アップロードURLは最大5分（Memoryの`expiresAt`以下）の新規作成専用PUTです。返却する`uploadHeaders`の`Content-Type`、`x-goog-if-generation-match: 0`、`Cache-Control`を送信し、`Content-Length`はブラウザがBlobサイズから自動設定します。上書き・読み取り・一覧には使えません。`complete`はbodyなしで何度送っても同じ写真の状態へ収束します。未アップロードは409、不正画像は消去後`failed`となり、消去に失敗した間は`deleting`で新しいURLを発行しません。

本人memoryは検証済み`ready`だけを撮影時刻順に返し、画像URLは5分と`deleteAfter`の早い方までです。`moments`の各要素へ`checkpointId`（参加者が選んだチェックポイント、旧GPS写真はnull）を返し、写真一覧でも同じ地点を返します。`locationConfidence`は旧GPS写真だけに返す互換項目で、精度100m超はlow、30m超〜100mはmedium、30m以下はhighです。経路の信頼度ではありません。

経路生成は写真を撮影時刻順に並べ、隣接するready写真の対についてCampusGraph上で接続する。両端ともチェックポイント（または混合でGPSアンカー+チェックポイントノード解決）ならノード間/最短経路で区間を作成し、`algorithmVersion`を`checkpoint-dijkstra-1`とする。両端とも旧GPS由来なら従来のスナップ推定（`photo-edges-dijkstra-1`）を維持する。ノード解決不能（旧GPSの精度不足・経路不在など）は`confidence:low, path:[]`で記録し、撮影順は変更しない。到着イベント確定のトランザクション内で、セッションにready写真が残る場合のみ、最後の写真から到着チェックポイントへの区間を同じ方式で追加する（写真0枚の到着では区間を作らない、写真削除時も再計算で整合を保つ）。到着区間の`toPhotoMomentId`はnullになる。写真一覧には画像URLを含めません。既存の`photoSummary`はDBの実件数から計算します。
写真が`ready`になると、撮影時刻順の隣り合う写真ごとに`RouteSegment`を同一DBトランザクションで作成します。個別削除要求では`deleting`になった写真をただちに除外して残る前後の写真間を再計算し、画像消去完了・失敗時にも整合させます。写真0枚・1枚なら区間は空のままで到着/終了を妨げません。既存写真を保持するDBへ移行したときは本人memory取得時に欠損区間を再計算します。

CampusGraphは`internal/route/campus_graph.json`へ固定同梱します（TRACE_F main、2026-10-23取得、JSON記載版`toyosu-graph-2026-10-21.1.draft`）。Graph付属READMEの記述版`toyosu-graph-2026-10-18.2.draft`とは異なるため、JSON自身の版を正として使います。Graphの`coordinateVersion`=`toyosu-2026-08-28.1`、`threeDMapVersion`=`キャンパス全体.blend 2026-10-21`と公開設定の三つが一致しないと写真ストレージを用いるAPI/清掃ジョブの起動を拒否します。既定の`campus-2026-01`等を黙って上書きしません。承認済みの地図・表示機側にも同じ版を事前配布・ヘルス確認してから`TRACE_COORDINATE_VERSION`、`TRACE_CAMPUS_GRAPH_VERSION`、`TRACE_THREE_D_MAP_VERSION`と豊洲のジオフェンスを設定し、受け付け切替・ロールバックを行ってください。Graphの更新では固定JSON、互換版、試験点の測定を併せて更新します。

アンカーはGraphのローカルENU（東=x、北=z）へ変換し、GPS精度円内の通行可能な`gpsSnap=true`両端を持つ辺だけへ投影します。`passable=false`、`restricted=true`、`closed=true`、時刻付き`closures`内、および`openingHours`（曜日0=日曜〜6=土曜、`open`/`close`は`HH:MM`・日本時間）の時間外辺は候補・探索の両方から除外します。現行Graphは`passable`のみを収録し、現場の一時通行止め・利用時間の運用入力は未提供です。運用データが正式に渡されるまでは実際の臨時閉鎖や時間外利用を防げないため公開運用に使用しないでください。階や屋内はGPSで確定できないため`gpsSnap=false`の辺へ吸着しません。

本人専用memoryレスポンスの`segments`は`routeSegmentId`、両端の写真ID（到着区間の終点はnull）、`coordinateVersion`、`campusGraphVersion`、`threeDMapVersion`、`algorithmVersion`、`confidence`、`lengthM`、`path`、`pathNodeIds`を返します。チェックポイント区間の`pathNodeIds`は最短経路のGraphノードID列で、旧GPS区間と解決不能な区間は空配列です。`path`は従来の表示用ローカル点列`{t,x,z}`を維持します。旧写真のGPS履歴や生緯度経度は返しません。同程度の複数辺・撮影GPS欠損/精度不足・経路不在の旧区間は`confidence=low,path=[]`です。到着地点が現行Graphで解決できない場合や最後の写真が到着後に撮影された場合は到着区間を作らず、本人memoryを閲覧不能にはしません。

削除はまず永続的な`deleting`を確定し、画像の消去が成功するまで参照・認証材料を捨てません。写真DELETE成功は204、写真・Memoryの消去失敗は503となり、同じ冪等性キーで再試行します。体験単位削除は従来どおりLive削除後にMemoryを処理し、写真消去失敗時は`memory: pending`です。期限切れはAPI常駐清掃と清掃ジョブで同じ処理を再開します。

署名付きPUTは削除後も期限まで生きるため、アップロード先はゼロバイトで占有し、期限後だけoutboxから消去します。これにより削除後の再送で画像が復活しません。画像バイト・GPS・PhotoMoment参照とそれに紐づくRouteSegmentは削除対象となり、この短期outboxにはランダムなobject keyと期限だけを保持します。ピン・派生特徴・別サムネイルはまだ生成しません。

`004_route_segments.sql`〜`009_checkpoint_route_versions.sql`を番号順に適用し、`runtime-grants.sql`を再適用してください。`008_photo_checkpoints.sql`適用済み環境でも追加列は`009`で適用されます。旧バイナリは新版schemaを拒否するため、受付停止の移行窓と手動migration確認が必要です。SDKの検証は公式クライアントのHTTP fixture、API実動作は専用PostgreSQLと明示的な一時object emulatorで確認しています。実GCSのIAM・bucket CORS・端末アップロードは、承認済みstagingで別途必要です。

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
APIと清掃ジョブは起動時、APIは`/health/ready`と各`/v1/`・`/v2/`リクエスト、定期清掃は実行前に、埋め込まれたmigrationの最終版とDBの連続した適用履歴が一致することを確認します。DBが古い・新しい・履歴に欠番がある場合は受付・清掃を行いません。既存の旧バイナリは新版の検査を持たないため、非互換migrationの前に停止・置換が必要です。APIユーザーにはスキーマ変更権限を付けず、新規テーブルの必要権限を`deploy/gcp/runtime-grants.sql`へ明示的に追加してから配信します。[stagingでの手順](docs/staging-cicd.md#6-運用と復旧)を参照してください。

APIは起動時と稼働中30秒ごとにDBの期限データを清掃し、表示位置は稼働中毎秒プロセスメモリから消去します。期限切れ位置は清掃前でも読み出せません。別途5分ごとのDB清掃ジョブはAPIプロセスの位置メモリを消去できません。Cloud RunのAPIは最大1インスタンス・CPU常時割当とし、スケールアウト・再起動・新旧revision並存で直近の表示位置が失われたり分岐したりするため、追跡中の切替を避けます。

`002_remove_live_positions.sql`で旧表示位置テーブルは破棄し、移行前の位置点は持ち越しません。既存のExperienceLinkは認証トークンの旧期限を`token_expires_at`へ移し、関連付けの保持期限を両セッションの早い`delete_after`へ更新します。再配備時はDBのschema版が非互換となるため、旧revisionを停止してからmigrationを実施します。切替中の位置表示欠落は仕様です。データ保護承認（設計Issue #9）と到着proofの暫定期限・運営導線の最終仕様（設計Issue #11）は未確定です。

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
`go test ./internal/route -run TestGenerationP95 -count=1 -v`は同梱された実Graph上で100回の生成時間を採取し、昇順95番目（p95）をログへ出します。デプロイ環境で計測し、承認した目標値と照合してください。試験コードを追加しただけでは実測・合格を意味しません。

検証対象：Memoryのみの新規参加、旧GPS受付拒否、写真GPS拒否、新規写真の`checkpointId`必須・不明ID拒否・グラフ版自動記録、チェックポイント経路と到着区間・混合区間・削除時の再計算、旧セッションの資格情報分離・停止・同時削除・削除期限、再送記録失敗時のロールバック、DB migrationの初回・追加・再実行・並行実行・失敗時ロールバック・不整合履歴・旧アプリのready拒否、既存GPS写真の経路/削除、CampusGraph上の既知経路/通行禁止/時間外/複数候補/欠損、Readyと写真削除時の区間再生成、Cloud Runポート・Cloud SQL接続設定。

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
