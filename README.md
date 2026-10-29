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

新規写真の`checkpointId`は固定Graphに存在し、`selectable:true`であることを必須にします。`role`に依存しないため、選択可能なwaypointも登録できます。未選択・未知ID・`selectable:false`・属性欠落・nullは400 `INVALID_CHECKPOINT`です。SessionのGraph版が現行版と違う場合、新規追加は従来どおり409 `STATE_CONFLICT`で拒否します。Frontendのpayloadへ座標・role・selectable・Graph版を追加する必要はありません。

同じID/同じ内容の再送は同じ記録、同じ冪等性キーで別内容は409 `IDEMPOTENCY_CONFLICT`、同じclient IDを別内容で使う場合は409 `STATE_CONFLICT`です。12枚を上限とし、撮影時刻順にチェックポイント経路を作ります。削除はメタデータ・地点・経路を同一transactionで消し、削除済みclient IDのfenceをMemoryのdeleteAfterまで維持するため遅延再送は404です。写真0枚は経路なし、1枚でも到着後は写真地点からShibaLabまでの区間を返します。旧Sessionはlegacyのまま、GCS設定を保持した場合だけ旧本人APIで一覧・画像閲覧・削除を期限まで提供します。新モード中の旧登録・upload-url・completeは503です。

本人用`GET /v2/.../memory`の写真一覧と経路は、同じ読み取り専用`REPEATABLE READ`トランザクションのコミット済みスナップショットから取得します。読取途中に写真追加・個別削除・期限清掃が確定しても、異なる時点の写真と経路を混在させません。セッション単位ロックを経由しない一括期限削除も対象です。読取終了後はトランザクションを閉じ、API形式・写真0/1枚の既存動作・保存期限・受付フラグは変更しません。この修正に追加migrationは不要です。

新モードと`TRACE_PHOTO_ENABLED=true`（旧アップロード）は併用できません。旧画像・署名付きURLのfenceが残る間は、旧アップロードを無効にしても`TRACE_PHOTO_BUCKET`と`TRACE_PHOTO_SIGNER_EMAIL`をAPI・清掃ジョブ双方に残します。画像が存在するのにGCSへアクセスできない状態では起動を拒否します。`TRACE_REGISTRATION_OPEN=true`には新モードと、**既定の草案とは別に**設定した同意版・本文が必要です。設定値が存在するだけでは大学・運営の承認を証明できません。設計PR #25の確定、同意・保持期間の承認、Frontend/Worker・展示の統合試験までは受付を閉じたままにします。

`010_device_photo_moments.sql`と`011_push_thirty_minutes.sql`は明示的に適用し、`deploy/gcp/runtime-grants.sql`を再適用します。後者は旧15分通知の次回予定を15分延ばして30分へ移行します。旧revisionを止め、受付停止・バックアップ・旧画像の残数とfence・migration・権限を確認してから新APIと清掃ジョブを起動してください。切替後は旧URLが全て期限切れとなり、fenceの再消去が完了するまでGCS設定を外せません。

## 参加停止・本人状態と旧Liveの保持

`POST /v1/memory-sessions/{memorySessionId}/stop`は本人Memory Bearer、Idempotency-Key、空bodyで参加を終端化します。最初のstoppedAtを固定し、Push購読・予定を削除、queued/playingをparticipant_requestでskipし、描画除外とfallback進行まで同じtransactionで確定します。以後の新規写真・初回到着・Push再購読・演出開始は拒否します。単純な通知解除とは異なり再開できません。

停止は削除ではありません。expiresAt/deleteAfterを変更せず、本人の状態・写真ストーリーの閲覧と削除はdeleteAfterまで維持します。本人GETのmemorySession.statusは未到着main、到着後arrived、停止または失効後ended。到着前のreplayStatusはnullで、演出completed/skippedだけでは参加状態をendedにしません。displayAliasとonSiteChoiceRequiredは返しません。

旧Liveは`GET /v1/live-presence-sessions/{liveSessionId}`を自身のLive Bearerで照会し、sessionId・trackingStatus・expiresAt・deleteAfterだけを取得できます。Memory削除後やLive失効後も、Live自身のdeleteAfterまで照会・停止・削除を維持します。Memory/他人のBearerや保持期限後は401。停止で期限を延長せず、旧位置送信・on-site継続を復活させません。

017は停止時刻と写真/停止receiptのMemory所有関係を追加します。新receiptは最大24時間かつMemoryのdeleteAfter以内で、Memory全削除時にcascade消去します。識別可能な既存写真receiptも関連付けます。既に写真が消えて一方向scopeから所有者を復元できない旧receiptは既存期限を維持します。内容なしの全体/一括削除の成功・部分成功receiptだけは従来の最大24時間の再送契約を維持します。


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
- 既定無効の投影QR到着受付、個人運営資格によるproof発行、永続演出キュー、本人向け現在状態
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
| GET | /v1/live-presence-sessions/{liveSessionId} |
| POST | /v1/memory-sessions/{memorySessionId}/stop |
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
| POST | /v1/operator/credentials |
| DELETE | /v1/operator/credentials/current |
| DELETE | /v1/operator/credentials |
| POST | /v1/operator/arrival-proofs |
| GET | /v1/operator/arrivals |
| GET | /v1/operator/status |
| POST | /v1/operator/registration/close |
| POST | /v1/operator/notifications/stop |
| POST / DELETE | /v1/operator/fallback |
| POST | /v1/operator/arrivals/{arrivalId}/skip |
| POST | /v1/operator/display-tokens |
| POST | /v1/operator/arrivals/{arrivalId}/retry |
| GET (WSS) | /v1/display/ws |

到着QRと描画WSSは個別flagで有効化します。端末写真のチェックポイント経路を到着・再接続・写真変更時に描画へ投影します。個人別Google運営認証、資格失効、監査、状態照会・受付停止・全通知停止・fallbackまでローカル実装しています。公式Google検証器と合成JWKSによるHTTP/WS・実ブラウザ検証は、実Googleアカウント・staging・投影機材・大学の公開承認の代わりではありません。旧写真ピン・フォトスポット表示は廃止済みです。
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
| 運営操作 | 認証済actorごとに60回/分・burst 30、全体20回/秒・burst 100 |
| 個人運営資格の発行 | 検証前のIPごとに12回/分・burst 5、全体1回/秒・burst 10。検証後のactorごとに6回/分・burst 5 |
| 描画token発行（不正資格を含む） | 接続元IPごとに12回/分・burst 5、全体1回/秒・burst 10 |
| 描画WS接続試行（認証前） | 接続元IPごとに1回/秒・burst 10、全体10回/秒・burst 20 |
| 公開APIの追加上限 | 認証付きAPIはIPごとに1200回/分・burst 120。全API合計100回/秒・burst 100 |

超過時は`429 RATE_LIMITED`と秒数の`Retry-After`を返し、その間は送信を止めて再試行します。失敗要求も制限対象です。新`/v2`の写真POSTは6回/分、本人GET/DELETEは一般のセッション操作枠30回/分を使います。Pushの購読・変更・解除は別の10回/分枠です。描画WSは1接続あたり10メッセージ/秒、各256KiBで制限します。
許可Originには`Retry-After`をCORSで公開します。一括削除はMemoryのBearer tokenと`X-Trace-Live-Token`の両方の操作枠を同時に確認・消費し、片側が超過していれば削除を開始せず、もう片側の枠も消費しません。位置送信と操作のトークン別枠は分離されていますが、IP別・全体の追加上限は共通です。
IPはWorker経由なら共有シークレット検証後の`X-Trace-Client-IP`（WorkerがCloudflareの`CF-Connecting-IP`から設定）、それ以外は接続元`RemoteAddr`を使用します。`X-Forwarded-For`を信用しません。中継IPしか取得できない場合は参加作成・公開設定のIP制限が中継単位になり得るため、公開前に疎通を確認してください。
転送IPは単一のIPv4/IPv6だけを受け付けます。複数の同名ヘッダー、カンマ区切り、IPv6 zone付きなどは接続元IPへフォールバックします。
カウンターは最大10,000件を1プロセスのメモリに保持し、2分以上使われていないものを消去します。容量超過は`429`（`Retry-After: 60`）で安全側に拒否します。再起動でリセットされ、複数インスタンス間では共有されません。`deploy/gcp/deploy.sh`の最大1インスタンスを維持し、新旧revisionの並存も避けてください。増設する場合は共有レート制限ストアまたは信頼できる共通edgeの制限へ移行します。

## 任意の撮影リマインダー

`TRACE_PUSH_ENABLED=false`が既定です。承認済みの通知文面・別途のブラウザ通知許可・stagingでの実Push確認後だけ有効化してください。MemorySession作成・写真撮影・位置共有には通知が不要です。通知を無効化した場合、新規PUT/PATCHは`503 PUSH_UNAVAILABLE`、DELETEは本人認証後に購読を消去します。無効化して起動すると残存購読を全消去し、外部送信は開始しません。公開設定`notification`は`enabled`、`defaultIntervalMinutes:30`、`allowedIntervalsMinutes:[30]`、`exactDeliveryGuaranteed:false`と、有効化時のみ`vapidPublicKey`を返します。

`TRACE_PUSH_VAPID_PUBLIC_KEY`は非圧縮P-256公開鍵（65 byteのbase64url）、`TRACE_PUSH_VAPID_PRIVATE_KEY`は対応する32 byteの秘密scalar、`TRACE_PUSH_VAPID_SUBJECT`は有効な運営先の`mailto:`、`TRACE_PUSH_ENCRYPTION_KEY`は**VAPIDとは別の**ランダムな32 byte AES-GCM鍵（base64url）です。鍵はSecret ManagerからAPIとworkerへ同一の値を渡し、ソース・通常ログ・運営画面・描画へ出さないでください。古い暗号化鍵を失うと既存購読は復号できません。鍵を変更する場合は購読を一旦消去し、端末に再同意・再購読を求めてください。`005_push_subscriptions.sql`の適用後、`deploy/gcp/runtime-grants.sql`を再適用してからAPIを入れ替えます。
公開鍵と`mailto:`は通常設定、秘密鍵と暗号化鍵は別々のSecret Manager参照で配置し、実行SAのAccessorを対象secretへ限定します。現行`deploy/gcp/deploy.sh`は受付停止staging専用でPushを含む全featureを無効にし、旧任意`GCP_PUSH_*_SECRET`は拒否します。通知を有効化する別の承認済み配備では、API/workerへの必要鍵、最小1/CPU常時割当、重複・配信時刻・provider到達性・費用を検証してください。閉鎖previewのmin0/request-based CPUでは常駐dispatcherを保証しません。

Memory Bearer tokenで`PUT /v1/memory-sessions/{memorySessionId}/push-subscription`へ`{"consent":true,"subscription":{"endpoint":"https://...","keys":{"p256dh":"base64url","auth":"base64url"}},"intervalMinutes":30}`を送ります。許可・説明後の参加者操作だけで`consent:true`を送信してください。endpointは許可したHTTPS Push serviceだけを受け付け、外部URLを無条件にフェッチしません。`PATCH`は`{"intervalMinutes":30}`または`{"status":"paused"}` / `{"status":"active"}`を受け付け、`DELETE`（空body）は204で未送信予定と鍵を消去します。PUT/PATCHは`status`、`intervalMinutes`、`nextSendAt`、`expiresAt`だけを返し、endpointや鍵は返しません。

10秒ごとに最大50件を抽出し、DB内のMemoryロックと状態再確認のもと、5秒以内にプロバイダへ送信します。送信時刻は厳密には保証しません。payloadは固定の一般文面と相対URL`/photo`だけで、位置・写真・セッションID・参加記号を含みません。正常2xxでは次回を間隔後へ、404/410では鍵を削除、その他の失敗は1分後に再試行します。Web Pushに配信のexactly-once保証はなく、プロバイダ受理後DB commit失敗時には重複し得ます。写真の初回`ready`時は同じトランザクションで次回を延期します。Memory削除準備・Memory/Live削除・Live停止・到着確定・期限清掃は購読も削除します。到着時は同じMemory Storeトランザクション内で`push_stopped=true`を確定し、再購読を禁止します。単純解除は再購読可能です。

## 個人別Google運営認証（既定無効）

TRACE_OPERATOR_ENABLED=falseが既定です。有効化にはTRACE_OPERATOR_GOOGLE_CLIENT_ID、完全一致のTRACE_OPERATOR_EMAILS、TRACE_OPERATOR_ALLOWED_ORIGINSと32バイト以上のTRACE_EDGE_SHARED_SECRETが必要です。初期許可者はasync5855@gmail.comのみ、Web client IDは768829535838-9v4875nkjopo91uobrmggneqodmvec85.apps.googleusercontent.comです。描画用設定・資格とは独立し、Google本人確認を共有しても描画資格だけで運営操作はできません。

認可済み運営入口から、Google ID tokenを単一のAuthorization Bearerに入れ、許可Origin・空body・queryなしでPOST /v1/operator/credentialsへ送ります。Edge keyは展示PCのloopback broker等のサーバー側だけが追加します。公式Google検証器でRS256署名、issuer、Web audience、sub、時刻、email_verified、必要なazpとGmail/Workspace所有権を確認し、200でoperatorToken・credentialId・expiresAt・role:"operator"を返します。

運営資格は256bitの不透明Bearerで2分有効、Origin束縛です。DBには資格hash、安定したGoogle subject hash、正規化メールの照合hashだけを保存し、生メール・sub・Google tokenを保存/ログ出力しません。ブラウザはGoogle tokenと資格をメモリだけで保持し、URL・永続ストレージ・QRへ載せません。操作ごとに期限・失効・現在の独立許可リストを確認します。Google ID token自体、参加者/描画資格、旧発行署名ヘッダーは運営操作に使えません。

DELETE /v1/operator/credentials/currentは現在の資格だけ、DELETE /v1/operator/credentialsは同じ操作者の保存中の全資格を失効し、成功は204です。commit後の要求は401。認証済みの処理中要求や、その後の新しいGoogleログインまで禁止する操作ではありません。アカウント禁止は許可リストから除いて新設定へ切り替えます。失効後の同資格による再送は401で、失効に失敗したUIは成功扱いしません。

資格発行/失効・proof発行・skip/retry・受付/通知停止・fallbackを、個人actor・メール照合hash・時刻・短縮対象・結果と同じtransactionで24時間監査します。hashは匿名化を意味せず許可メールから照合可能です。履歴のarrival_issuer行には個人を後付けせず従来期限まで保持します。018_operator_credentials.sqlを明示適用して権限も再適用します。旧TRACE_ARRIVAL_ISSUER_KEYとHMAC運営経路は撤去済みで、旧設定が残る起動は拒否します。追加の長期運営署名鍵は不要です。

## 投影QRによる到着（既定無効）

TRACE_ARRIVAL_ENABLED=falseが既定です。有効化にはTRACE_ARRIVAL_VENUE、TRACE_ARRIVAL_CHECKPOINT_ID、独立した32バイト以上のTRACE_ARRIVAL_PROOF_KEYと公開のTRACE_ARRIVAL_PROOF_KEY_ID（既定arrival-proof-v1）を設定します。発行は独立した個人運営資格で認可します。秘密鍵はSecret Manager等のBackend側だけへ配置します。鍵/key ID切替後は旧proofの新規到着を拒否しますが、到着済み本人の状態回復・閲覧・削除は維持します。複数revisionを同時公開せず、公開承認まではflagを無効にします。

POST /v1/operator/arrival-proofsへ許可Origin・個人operator Bearer・空body・queryなしでアクセスします。認証付きloopback brokerがEdge keyをサーバー側で追加し、ブラウザへ渡しません。checkpointTokenはWeb URLではなくQRへエンコードし、例えば15秒ごとに交換、expiresAt後は掲示しません。参加端末はWeb App内のカメラで読み、既存Memoryのまま送信します。認証なしの常時発行や共有QRキャッシュはありません。

参加者用サイトWorkerは/v1/operator/*を転送しません。個人資格の発行・失効・QR発行は認可済み運営入口だけを使い、描画WSSは引き続きブラウザからGoへ直接接続します。Origin許可は認証の代わりではなく、Edge keyだけでも運営操作できません。

proofはbase64url(JSON).base64url(HMAC-SHA256(proof key, base64url(JSON)))で、purpose:"arrival"、keyId、checkpointId、venue、issuedAt/expiresAt（Unix秒）、ランダムnonceを含み25秒有効です。参加者はMemory BearerとIdempotency-Key、{"checkpointToken":"...","clientTime":"..."}を到着APIへ送ります。clientTimeでなくサーバー時刻を正とし、用途・鍵ID・署名・会場・地点・時刻の不正は初回到着を400 INVALID_ARRIVAL_PROOFで拒否します。同一QRは有効期間内に別Memoryからも利用でき、一回限りの物理来場保証ではありません。

同じMemoryの到着はArrivalEventを一度だけ作ります。同じキー/同じ内容、または別キーでの本人再送は、QR期限切れ・鍵切替後も同じ到着IDと現在のstatusを返し、過去のqueued応答に固定しません。同じキーの異なる内容は409 IDEMPOTENCY_CONFLICT。Memory削除時は到着/receiptも消え、演出skipで本人照会・削除期限を短縮しません。

旧Memoryの有効なExperienceLinkは照合し、到着と追跡中・来場前の対応Liveのarrived更新を同じtransactionで確定します。停止済みLiveは再開しません。旧位置書込み・on-site継続は廃止済みで本人照会・停止・削除は保持します。新Memoryはlinkなし・写真0枚でも到着できます。本人GETのreplayStatusは未到着null、到着後queued/playing/completed/skipped。待ち順・推定待ち時間・aliasは返しません。

写真0枚でも到着を受け付け、1枚なら写真地点からShibaLabまでの経路を返します。pending写真があれば最大15秒だけ開始を遅らせ、後続は追い越しません。FIFO上限は有効なMemoryの待機50件（playingを除く）で、通常待機30秒ではskipしません。満杯時は503 ARRIVAL_QUEUE_FULLとRetry-After: 5を返し、到着・receipt・Push停止・旧Live更新を確定しません。空き回復後に再試行でき、初回には有効QRが必要です。Memoryの停止・失効・削除は待機/描画から除外します。本人arrival/skipは未再生だけを対象とします。

運営候補GET・skip/retryも個人operator Bearerを使います。候補はFIFOのIDと写真件数だけです。retryは描画障害skipに限り、有効・未停止Memoryと空き枠が必要です。主描画機が先頭の開始を自動要求し、Backendはlease・先頭・写真待機・単一playingを検証します。先行開始や重複はrenderer.rejected。終了画面4秒とQR非表示はFrontendの責務です。失敗理由は固定分類renderer_failureで記録し、任意の例外文や個人情報は受け付けません。

`013_arrival_fifo_queue.sql`で`replay_deadline`と関連制約・indexを撤去し、`playing`が全体で最大1件となる部分一意indexを追加します。既存の複数`playing`は自動修復せずmigrationを拒否するため、切替前に運営操作で整理してください。**受付停止→既存の待機・再生を処理/明示skipしてキューを空にする→旧API/清掃revisionを停止→バックアップ→明示migration→実行権限再適用→新revisionのready確認**の順に切り替えます。`012`は変更しない履歴であり、未適用環境ではその10件/30秒skipが`013`より先に実行されます。既存待機を残したまま更新しないでください。既にskipされた演出は`013`でも復活させません。schema非互換のため旧バイナリへ直接ロールバックしないでください。旧Live/位置APIの移行方針は冒頭のBackend #26を参照してください。

旧schemaからは018まで欠番なく明示適用しruntime-grants.sqlを再適用します。013のFIFO移行、014～016の運営制御、017の参加停止/receipt所有、018の個人資格/監査が必要です。旧API・清掃revisionを停止してから適用し、旧Session/写真のGraph版・本人資格・同意・deleteAfterを書き換えません。ローカル検証を実staging・実Googleログイン・公開承認の代わりにしません（[配備手順](docs/google-cloud.md#3-dbユーザーと秘密情報)）。

### 運営状態照会

GET /v1/operator/statusは同じ非公開運営経路・個人operator Bearer・許可Origin・Edge保護を使います。参加者/描画資格、Google ID token自体、Edge keyだけでは取得できません。運営無効は503、資格不正/失効は401、Origin不許可は403、制限超過は429。状態照会・受付停止・全通知停止はArrivalEnabledと独立して使えます。

| 応答フィールド | 意味 |
|---|---|
| `observedAt` | DB照会に用いたサーバーUTC時刻 |
| `registrationOpen` | 環境設定で受付が有効、かつDBの運営停止が未設定か |
| `arrivalQueue.queued` | 有効・削除処理中でないMemorySessionの待機件数。pending写真の開始待ちも含む |
| `arrivalQueue.playing` | 同条件のMemorySessionに紐づく、DB上の再生中件数 |
| `notification.enabled` | 通知機能・送信サービスが有効で、運営による全通知停止が未設定か |
| `notification.operatorStopped` | DBに全通知停止が永続化されているか。環境設定による無効化とは別 |
| `display.enabled` | 描画機能と必要な認証設定が有効か。描画機の接続・正常稼働を保証しない |
| `display.connections` | このAPIプロセスの認証済みWSS接続数。主機・待機機を含み、認証待ちの接続は含まない |
| `display.primaryLeaseActive` | DBの主描画機leaseにholderとlease IDがあり、照会時刻より後まで有効か |
| `fallback.mode` | 運営の描画モード。`normal` / `pending` / `fallback` |
| `fallback.switchAt` | `pending`だけが持つUTC切替期限。それ以外は`null` |
| `fallback.changedAt` | 現在のmodeをDBへ確定したUTC時刻。状態を変えない再送では不変 |

キュー・lease・fallbackは一つのSQLスナップショットで読み、接続数は別途プロセス内から取得するため全項目の同時刻性は保証しません。照会は状態を変更せず、失効leaseの清掃前は`playing`が残っていても`primaryLeaseActive:false`になり得ます。接続数は複数APIプロセスの合計ではありません。既定の単一インスタンス運用を維持してください。参加者・到着・描画機のID、lease資格、写真・地点・画像URLは返さず、応答は`Cache-Control: no-store`です。

これらの操作には個人別の独立運営資格と監査を使います。実Googleアカウント、運営入口の配備、機材リハーサル、大学・責任者の公開承認は別で、本実装だけで現地運用完了とはしません。

### 新規参加受付の停止

POST /v1/operator/registration/closeへ個人operator Bearer・許可Origin・空bodyで送ります。Edge keyはサーバー側だけが追加します。参加者/描画資格やGoogle ID token自体では実行できません。運営認証は必要ですが到着機能が無効でも使えます。

成功応答は200 `{"registrationOpen":false}`。停止状態はDBの`registration_control.closed_at`へ永続化し、`TRACE_REGISTRATION_OPEN=true`でも再起動・再デプロイでは再開しません。再送も200で、最初の停止日時を保持します。公開設定と運営statusの`registrationOpen`は、環境設定とこの停止状態の両方を反映します。新規参加は503 `REGISTRATION_CLOSED`となり、MemorySessionやExperienceを作成しません。既に処理中の参加作成と停止はDB行ロックで順序付けられ、停止が成功した後に新しい参加作成を確定しません。

停止は新規参加だけが対象で、既存参加者の写真・到着・本人照会/削除・通知/期限清掃を停止しません。停止と個人actor/メール照合hash、registration.close、固定対象registration、結果closed/already_closed、時刻の24時間監査を同じtransactionで確定し、失敗時はロールバックします。写真・地点・参加者tokenは記録しません。

展開時は受付を閉じ、旧API/清掃を停止してバックアップ後、014の制御表を含む018までの連続migrationとruntime-grantsを明示適用します。実行ユーザーでready・非公開status・公開設定を確認し、承認後だけ受付を有効にします。DBで運営停止済みなら環境設定だけでは再開せず、再開APIは対象外です。旧schemaのバイナリへ直接ロールバックしません。

### 全撮影通知の停止

POST /v1/operator/notifications/stopへ個人operator Bearer・許可Origin・空bodyで送ります。Edge keyはサーバー側だけが追加します。参加者/描画資格、Google ID token自体、Edge key単独では実行できません。到着やPushが無効でも、有効な運営資格で残存購読を消去できます。

成功応答は200 `{"notificationsStopped":true}`。`notification_control.stopped_at`へ停止状態を永続化し、active/pausedを問わず全購読の暗号化endpoint・鍵・送信予定を削除します。同じ操作の再送は200で、最初の停止日時を保持します。`TRACE_PUSH_ENABLED=true`のまま再起動・再デプロイしても停止は解除されません。通知再開APIは対象外で、再開には別途承認済みのDB変更手順と参加者による再購読が必要です。

停止後の公開設定は`notification.enabled:false`と空の`vapidPublicKey`、運営statusは`notification.enabled:false`・`notification.operatorStopped:true`を返します。既存/新規MemorySessionの購読PUT・再開/変更PATCHは503 `PUSH_UNAVAILABLE`、本人のDELETEは204で引き続き利用できます。新規参加受付、写真登録、到着、本人照会・削除、期限清掃は停止しません。

購読・変更・送信は同じ制御行の共有ロックを、運営停止は排他ロックを取得します。停止は送信中のprovider要求が終わるまで待ち、抽出済みの送信候補も送信直前に停止状態を再確認します。ロック待ち失敗では成功を返さず、成功はDB commit後だけです。既にproviderが受理した通知や端末へ届いた通知は取り消せず、停止成功後に端末へ届く場合があります。

停止・全購読削除・個人actor/メール照合hash、notifications.stop、固定対象notifications、結果stopped/already_stopped、時刻の24時間監査を同じtransactionで確定し、失敗時はロールバックします。endpoint・鍵・参加者tokenは記録しません。

展開時は受付/Pushを無効にし、旧API/清掃停止→バックアップ→015の通知制御を含む018までの連続migration→runtime-grants→新API/清掃の順に更新します。実行ユーザーでready・公開設定・非公開statusを確認し、承認後だけ有効化します。旧schemaのバイナリへ直接ロールバックしません。


### 運営によるfallback切替と復帰

POST /v1/operator/fallbackで切替、DELETEで通常運転へ復帰します。個人operator Bearer・許可Origin・空bodyと、サーバー側運営入口のEdge保護を使います。運営認証または描画機能・必要設定が無効なら503、非空bodyは400、上限超過は413。成功は200 {"fallback":{"mode":"fallback","switchAt":null,"changedAt":"2026-10-28T10:00:00Z"}}で、pendingだけが切替期限を持ちます。

- `normal`: 通常のFIFO再生を許可します。切替要求時に再生中がなければ直ちに`fallback`へ移ります。
- `pending`: 切替要求時に再生中だった1件だけの終了を待ち、新しい`arrival.replay.started`は拒否します。現在の再生が完了・失敗・切断・削除等で終われば直ちに`fallback`へ移り、終了しなければ要求時刻から30秒の固定期限で当該1件だけを`skipped`（`skip_reason=operator`）にします。再送は期限を延長しません。
- `fallback`: 参加者の新規再生を拒否し、Frontendが合成経路・抽象映像を表示するための状態です。再起動、再接続、新しい主lease取得では解除せず、運営の明示DELETEだけで`normal`へ戻します。DELETEは`pending`の取り消しにも使えます。

待機列と新しい到着受付は全モードで保持し、既存の待機50件上限・MemorySessionの期限切れ/削除は変えません。fallback切替は参加受付や全通知を停止する操作ではありません。復帰後は残っているFIFO先頭から再開し、中断した`operator` skipを自動再生・retryしません。過去に受理済みのrenderer eventIdの再送は従来どおり保存済みackを返す場合があるため、現在のmodeとsnapshotを優先し、新しい再生許可と誤認しないでください。

mode・固定期限・変更時刻は`display_fallback_control`に保存します。mode遷移では同じトランザクションで`system.fallback.changed`を永続化し、`data`に`{mode,switchAt,changedAt}`を送ります。`display.snapshot.data.fallback`にも同じ状態が入ります。Backendはmode差分の直後に最新snapshotを送り、そのcursorまで進めます。短時間の複数差分をsnapshotでまとめることがあるため、Frontendはsnapshotを正としてmodeと到着一覧を全置換してください。

Backendは保持中の参加者経路を認証済み描画WSへ引き続き配信しますが、**参加者の実データをfallbackのデモ素材には使いません**。Frontend側で`pending`中の新規開始を止め、期限を扱い、`fallback`中の参加者演出を停止して合成表示へ切り替える実装が必要です。合成映像の生成・実機の表示切替は本Backend変更に含みません。期限確定は描画WSの250msポーリング／サーバーの1秒清掃で処理し、DB停止やFrontend未対応時の実機切替は保証しません。

有効な操作/再送は個人actor/メール照合hash、fallback.enable/fallback.disable、固定対象fallback、結果、時刻だけを24時間監査します。参加者ID・写真・地点・資格は記録せず、監査/イベント保存失敗時はmode/skipもロールバックします。切替時刻は制御行と永続イベントに残ります。

展開時は受付/描画/運営flagを無効にし、旧API/清掃停止→バックアップ→014～016の制御表と017/018を含む連続migration→runtime-grants→新API/清掃の順に更新します。実行権限・readyを確認し、対応Frontendとの切替/復帰・待機保持を承認済み環境で検証してから個別flagを有効にします。旧schemaへ直接ロールバックしません。

## 描画WebSocket（既定無効）

`TRACE_DISPLAY_ENABLED=false`が既定です。有効化にはGoogle OAuthのWebクライアントID（`TRACE_DISPLAY_GOOGLE_CLIENT_ID`）、運営者のメールアドレス許可リスト（`TRACE_DISPLAY_OPERATOR_EMAILS`）、展示ページの専用Origin許可リスト（`TRACE_DISPLAY_ALLOWED_ORIGINS`）と、到着・Edgeから独立した32バイト以上の`TRACE_DISPLAY_TOKEN_KEY`が必要です。リストはカンマ区切り・完全一致で、wildcardは認めません。OriginはHTTPS、開発時のみ`http://localhost:<port>`/`http://127.0.0.1:<port>`を許可します。署名鍵はSecret Managerに置き、ブラウザやWorkerへ渡しません。座標/CampusGraph/3D Map版は埋め込みGraphと照合します。Google OAuthの許可JavaScript Originにも展示ページのOriginを登録してください。

PR #37の接続コメントに合わせ、設定例の展示Originは`https://trace-log.async5855.workers.dev`です。実際の画面URLはその`/display`ですが、Backendの許可OriginとGoogle側の承認済みJavaScript生成元にはパスを含めません。依頼者から提供されたWebクライアントIDと許可メールは`.env.example`と`deploy/gcp/runtime.env.yaml.example`へ反映済みです。Frontendの`NEXT_PUBLIC_GOOGLE_CLIENT_ID`にはBackendの`TRACE_DISPLAY_GOOGLE_CLIENT_ID`と**同じ値**を設定し、`NEXT_PUBLIC_DISPLAY_API_URL`と併せてビルドし直してください。stagingへの設定適用と実Googleログインは未完了であり、設定例の更新だけでログイン可能になったとは扱いません。[OAuth設定手順](docs/google-cloud.md#pr-37の展示ブラウザ接続設定)を参照してください。

展示ブラウザはGoogleログインで得たIDトークンを`Authorization: Bearer <Google ID token>`へ設定し、Backendへ直接、空bodyの`POST /v1/operator/display-tokens`を送ります。Googleの署名・audience・issuer・期限・発行時刻・sub・`email_verified`・存在する場合の`azp`を検証し、許可メールだけにOrigin束縛の2分有効な`displayToken`と`expiresAt`を返します。メールはGmail、またはGoogleが管理するWorkspace（署名済み`hd`あり）に限定します。任意の外部メールを登録しただけのGoogleアカウントは、Googleが現在のメール所有者を保証しないため拒否します（[Googleの検証指針](https://developers.google.com/identity/sign-in/web/backend-auth)）。旧HMAC発行とnative rendererの独自HTTPヘッダー接続は廃止しました。参加者資格・描画資格では発行できません。

ブラウザは`new WebSocket("wss://<backend>/v1/display/ws")`で直接接続し、5秒以内に次の**最初のテキストメッセージ**を送ります。版は配備済みbundleと完全一致させます。資格をURL・query・subprotocol・ログ・写真・QRへ載せず、Google IDトークンと描画トークンはブラウザメモリ内だけで保持してください。

```json
{
  "schemaVersion": 1,
  "type": "renderer.authenticate",
  "displayToken": "<issued-display-token>",
  "coordinateVersion": "toyosu-2026-08-28.1",
  "campusGraphVersion": "toyosu-graph-2026-10-26.1.draft",
  "threeDMapVersion": "toyosu-3dmap-2026-10-26"
}
```

認証前はsnapshot・差分・リースを渡しません。認証メッセージは16KiBまで、資格不正・認証未送信はclose 4401、版不一致は4409、サイズ超過は1009で切断します。Originなし・許可外Origin・query・旧Authorizationヘッダー付きWS接続はHTTP 403です。発行の資格不正は401、機能無効は503、レート超過は429/`Retry-After`です。接続前にHTTP認証が済んだと扱わず、最初のsnapshot受信を認証成功として扱ってください。

この2経路だけはブラウザ向け認証を行うためEdge共有secretを要求せず、専用OriginでCORSを返します。参加者の`/v1`・`/v2`とその他の運営APIは引き続きEdge保護の対象です。サイトWorkerは描画・運営経路を中継しません。Cloud Run IAMは別の境界であり、この実装ではIAM非公開サービスへ直接接続できません。承認済み到達経路・OAuth設定・Frontendログイン/更新・実Googleログイン・実機の8時間再接続試験が済むまで本番公開しないでください。

再接続では有効なGoogle IDトークンから新しい描画トークンを発行し直します。Googleの資格が期限切れならGoogle側の再認証が必要です。許可リスト更新後に旧APIを停止・置換すると新規発行を拒否しますが、発行済み資格は最大2分、認証済み接続は最大50分残り得ます。緊急失効は受付/描画を停止し、署名鍵のローテーションと旧API/revision・既存接続の停止を組み合わせてください。接続中の個別強制失効APIは提供しません。

認証成功後の最初の`display.snapshot`は`data.cursor`、再生待ち/再生中の`arrivals`、運営モードの`fallback`を含みます。到着・再生イベントの`eventId`はDB連番で、欠番や再接続時はsnapshotで全置換します。`participant.arrived`とsnapshotの各`arrivals`には端末内写真の`pathNodeIds`、撮影順の`photoCheckpoints`（`checkpointId`と`capturedAt`のみ）、`replayDurationSeconds`を含めます。到着後の`photo.moment.created/deleted`は同じ到着の**更新後の全経路と写真地点**を送るため、同じ地点・撮影時刻の写真が複数でも描画機は`arrivalId`で丸ごと置換できます。到着前の写真変更は描画へ流さず、最初の到着イベントで現在値を渡します。保存Graph版が現行版と合わない、経路が途切れる場合は経路を空にし、旧GPS写真を新しい端末内経路へ混ぜません。旧`position.updated/expired`、`tracking.stopped`、Live位相イベントは廃止し、snapshotの`participants`も返しません。画像、写真URL、PhotoMoment ID、MemorySession ID、旧位置、生GPS、LivePresence ID、色・記号による照合情報、参加者資格は描画へ含みません。映像へ正確な時刻やIDを表示しないでください。

送信JSONはすべて`schemaVersion:1`、3種のversion、`type`を含めます。`renderer.primary.acquire`で主リースを要求し、`renderer.primary.granted`の`data.leaseId`を保存します。他接続には`renderer.primary.conflict`を返します。`arrival.replay.started`、`arrival.replay.completed`、`arrival.replay.failed`にはクライアント生成の一意な`eventId`（8～128文字）、`arrivalId`、`leaseId`を指定します。成功は`renderer.ack`、競合・失効は`renderer.rejected`です。同じeventId・同じ操作の再送は24時間同じ結果となり、異なる内容は拒否します。切断/30秒heartbeat欠落/リース期限切れは再生中を`skipped`に確定し、別端末による自動再生はしません。運営の署名済み明示retryのみが再投入できます。10秒間隔のping/pong（または`renderer.heartbeat`）と30秒応答期限を設け、50分でWSを終了します。Cloud Run設定は3600秒で、クライアント側は切断時に指数バックオフで**新しい資格を取得し**snapshotから再接続します。

旧LiveSessionで受け付けた最新位置は本人照会のため単一BackendのGoメモリにTTLまで保持し、停止・削除時に消去します。新規位置は受け付けず、描画WSへも投影しません。到着・再生イベントと主描画機leaseはDBに保存します。Backendの最大インスタンス数は1のままにし、最新migrationと`deploy/gcp/runtime-grants.sql`を適用してください。ブラウザ認証切替そのものに追加migrationはありませんが、運営制御には014〜016が必要です。設計の公開情報制限に合わせ、描画へ`sessionId`と`photoMomentId`は送信しません。FrontendでのGoogleログイン・資格更新・直接WSSと再生の統合は別途必要です。

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

CampusGraphは`internal/route/campus_graph.json`へ固定同梱します。正本は[TRACE_F #61のコミット`93ad81fc417d7cb327ba54749ae13cc86541a2b2`](https://github.com/Mino829/TRACE_F/blob/93ad81fc417d7cb327ba54749ae13cc86541a2b2/models/trace/campus_graph.json)で、JSON全体を取り込み、版名だけを書き換えていません。`coordinateVersion`=`toyosu-2026-08-28.1`、`campusGraphVersion`=`toyosu-graph-2026-10-26.1.draft`、`threeDMapVersion`=`toyosu-3dmap-2026-10-26`を描画機・設定例と一致させています。三つが設定と一致しない場合、描画/写真メタデータ/写真ストレージを用いるAPI・清掃ジョブの起動を拒否します。承認済みの地図・描画機・Backendを同じbundleで配布し、受付停止と旧revision停止の移行窓で切り替えてください。既存MemorySessionや写真の保存Graph版は書き換えず、旧版のSessionには新Graphの写真地点を追加できません。旧版の保存経路を新Graphの座標へ読み替えず、版不一致区間は従来どおり解決不能として本人閲覧・削除を維持します。このbundle更新自体に追加のDB migrationはありません。

写真の新規受付には`PhotoCheckpointNode`で`selectable:true`を要求し、保存済み写真・経路・到着地点の解決には`NodeIndex`を使います。選択不可の内部ノードも経路途中や署名済み到着地点として使用でき、写真として新規登録できることは意味しません。以前の`role=="checkpoint"`判定で既に受理された写真は、保存Graph版が一致する限り本人の閲覧・同内容の再送・削除と経路/展示への投影を維持します。その地点に別の写真を追加する要求は新しい`selectable`判定で拒否し、削除済みIDの再送は404のまま復活させません。旧`CheckpointNode`は残さず、写真受付と内部ノード解決を分離しています。

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

APIは起動時と稼働中30秒ごとにDBの期限データを清掃し、旧本人向け位置キャッシュは稼働中毎秒プロセスメモリから消去します。期限切れ位置は清掃前でも本人照会で読み出せません。別途5分ごとのDB清掃ジョブはAPIプロセスの位置メモリを消去できません。Cloud RunのAPIは最大1インスタンス・CPU常時割当とし、スケールアウト・再起動・新旧revision並存で旧本人向け位置キャッシュが失われたり分岐したりするため、追跡中の切替を避けます。

002_remove_live_positions.sqlは旧表示位置を除去した履歴です。以後の移行でも本人の旧Session・画像の照会/削除と各deleteAfterを維持し、新しいGPS入力・旧アップロードを再開しません。schema変更前は旧revisionを停止します。データ保護承認と実会場の運営経路・Google設定・公開判定は本実装とは別に必要です。

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

[Google Cloud staging更新・公開判定](docs/google-cloud.md)と[Cloudflare接続条件](docs/cloudflare-origin.md)を参照してください。対象は`trace-staging-async5855`です。既存previewのpublic-invoker＋Edge認証を保持し、更新後はpreviewだけを稼働系、旧private `trace-backend`をmanual 0・タグなしの停止系とします。これは配備準備であり、既存環境を停止・更新したという記録ではありません。

初回デプロイ後の自動更新は[検証環境のCI/CD手順](docs/staging-cicd.md)を参照してください。
既存PostgreSQLテストの成功後だけ、mainの変更をCloud Runへ反映できます。初期状態は無効で、WIF・必要なIAM権限とGitHub変数の設定後に有効化します。
DB構造変更は手動確認が必要です。自動更新では現在の環境設定を保持し、一般公開・参加受付開始を行いません。

- 閉鎖staging API：Cloud Run東京、previewのみ、service-level最小0/最大1・request-based CPU。受付・写真・Push・到着・描画・個人運営は全てfalse
- DB：Cloud SQL for PostgreSQL 17（東京）
- 接続秘密：Secret Manager、実行用サービスアカウント
- イメージ：Artifact Registry、Cloud Build
- 期限削除：Cloud Run Job＋Cloud Scheduler

deploy.sh checkとci_staging.py check-maintenanceは管理APIを変更しません。実更新は承認後にquiesce→停止後backup→003から004～018のmigrate→runtime-grants→serve→清掃→scheduleの順です。閉鎖profileは受付・写真・Push・到着・描画・個人運営flagを全てfalseに強制します。本機能実装では実配備やGoogle設定変更を行っていません。

このリポジトリだけではクラウドリソースは作成・公開されません。課金先プロジェクトとDBの性能・可用性・保持期間を決めてから手順を実行します。
