# TRACE Backend — PostgreSQL / Google Cloud MVP

匿名参加セッションと短期リアルタイム位置を扱うGo APIです。DBはPostgreSQL 17、公開環境はGoogle Cloud Run＋Cloud SQL for PostgreSQLを想定しています。SQLiteドライバとローカルDBファイルへの保存は廃止しました。

## 実装範囲

- MemorySession / LivePresenceSession / ExperienceLinkの分離発行、別Bearer token認証
- 同意版検証、公開設定、キャンパス範囲・精度・時刻・ジャンプの検証
- 生の通常GPSや履歴は保存しない。描画無効（既定）では量子化済み最新位置をGoプロセス内だけで30～60秒保持し、停止・期限切れ・削除時に消去する
- 描画有効時は再起動・複数Backendのsnapshot再同期用に量子化済み最新位置1件だけをPostgreSQLへ保存し、TTL・停止・削除時にDBから消去する。過去位置を蓄積せず、投影用の不透明IDと安全な表示aliasだけを配信する
- MemorySession / LivePresenceSessionごとの有効期限と削除期限。期限後も各`deleteAfter`までは本人の状態取得・削除が可能
- 個別削除と両トークンによる体験単位削除。後者はLive側から順に個別に確定し、部分成功を24時間の記録と同じキーで再試行
- DBロックとトランザクションで各削除・冪等性記録を一括確定
- CORS応答（許可Originへのプリフライトでは一括削除用`X-Trace-Live-Token`も許可）、16KB JSON上限、ヘルスチェック、JSONログ
- CloudflareサイトWorker向けの共有シークレット検証（設定時、`/v1/*`のみ）
- 明示的なDBマイグレーションと期限削除コマンド
- 任意有効化する非公開写真API：メタデータ登録、署名付きPUT、検証完了、本人ストーリー、写真・セッション・期限削除
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
| GET / POST | /v1/memory-sessions/{memorySessionId}/photos |
| POST | /v1/memory-sessions/{memorySessionId}/photos/{photoMomentId}/upload-url |
| POST | /v1/memory-sessions/{memorySessionId}/photos/{photoMomentId}/complete |
| DELETE | /v1/memory-sessions/{memorySessionId}/photos/{photoMomentId} |
| GET | /v1/memory-sessions/{memorySessionId}/memory |
| POST | /v1/operator/arrival-proofs |
| GET | /v1/operator/arrivals |
| POST | /v1/operator/arrivals/{arrivalId}/skip |
| POST | /v1/operator/display-tokens |
| POST | /v1/operator/arrivals/{arrivalId}/retry |
| GET (WSS) | /v1/display/ws |

到着QRと描画WebSocketは機能フラグで個別に有効化します。写真ストーリーは写真撮影時GPSとCampusGraphから経路を推定し、通常GPS履歴を使いません。写真ピン・フォトスポット描画イベントと画像/経路を伴う到着演出素材、およびその他の運営APIはまだ統合されていません。
写真機能は既定で無効です。承認済みの非公開ストレージ設定と明示的な有効化が必要で、実装・ローカル検証だけで一般公開や実データ利用の準備完了を意味しません。

### 公開APIのレート制限

| 対象 | 1プロセスでの上限（token bucket） |
|---|---:|
| 参加作成 | クライアントIPごとに60回/分・burst 60、全体10回/秒・burst 10 |
| 公開設定取得 | クライアントIPごとに60回/分・burst 60、全体10回/秒・burst 10 |
| 位置送信 | Live参加トークンごとに12回/分・burst 5 |
| 本人状態取得、到着、演出skip、会場内選択、停止、個別・一括削除 | 参加トークンごとに30回/分・burst 30 |
| 写真メタデータ登録 | Memory参加トークンごとに6回/分・burst 6、保持中12件まで |
| 到着proof発行・候補照会・運営skip | 有効な運営署名の要求を全体で1回/秒・burst 10 |
| 公開APIの追加上限 | 認証付きAPIはIPごとに1200回/分・burst 120。全API合計100回/秒・burst 100 |

超過時は`429 RATE_LIMITED`と秒数の`Retry-After`を返し、その間は送信を止めて再試行します。失敗要求も制限対象です。通常の5秒ごとの位置送信は12回/分の補充速度と一致します。写真の一覧・URL再発行・complete・本人memoryは操作枠を共有します。描画WSは1接続あたり10メッセージ/秒、各256KiBで制限し、到着以外の運営APIは該当機能実装時に追加します。
許可Originには`Retry-After`をCORSで公開します。一括削除はMemoryのBearer tokenと`X-Trace-Live-Token`の両方の操作枠を同時に確認・消費し、片側が超過していれば削除を開始せず、もう片側の枠も消費しません。位置送信と操作のトークン別枠は分離されていますが、IP別・全体の追加上限は共通です。
IPはWorker経由なら共有シークレット検証後の`X-Trace-Client-IP`（WorkerがCloudflareの`CF-Connecting-IP`から設定）、それ以外は接続元`RemoteAddr`を使用します。`X-Forwarded-For`を信用しません。中継IPしか取得できない場合は参加作成・公開設定のIP制限が中継単位になり得るため、公開前に疎通を確認してください。
転送IPは単一のIPv4/IPv6だけを受け付けます。複数の同名ヘッダー、カンマ区切り、IPv6 zone付きなどは接続元IPへフォールバックします。
カウンターは最大10,000件を1プロセスのメモリに保持し、2分以上使われていないものを消去します。容量超過は`429`（`Retry-After: 60`）で安全側に拒否します。再起動でリセットされ、複数インスタンス間では共有されません。`deploy/gcp/deploy.sh`の最大1インスタンスを維持し、新旧revisionの並存も避けてください。増設する場合は共有レート制限ストアまたは信頼できる共通edgeの制限へ移行します。

## 投影QRによる到着（既定無効）

`TRACE_ARRIVAL_ENABLED=false`が既定です。有効化には`TRACE_ARRIVAL_VENUE`（会場ID）、`TRACE_ARRIVAL_CHECKPOINT_ID`（ブースID）、異なる32バイト以上の秘密値`TRACE_ARRIVAL_PROOF_KEY`（QR署名専用）、`TRACE_ARRIVAL_ISSUER_KEY`（運営発行API認証専用）が必須です。鍵を参加者サイト/QR/ログへ渡さず、Secret Manager等で配置してください。鍵の切替・失効は設定の更新と稼働revision切替で行い、旧proofと旧運営署名を即時失効させます。複数revisionを同時公開すると鍵・キューの一貫性が失われるため、切替前に受付停止し単一APIインスタンスを維持してください。設計Issue #11で未確定の秒数・容量は以下の暫定実装契約です。大学の審査と運営導線が整うまでフラグは有効化しないでください。

運営専用の`POST /v1/operator/arrival-proofs`へ空bodyでHTTPSアクセスします。認証ヘッダー`X-Trace-Issuer-Timestamp`には現在のUnix秒を十進表記、`X-Trace-Issuer-Signature`には`base64url(HMAC-SHA256(TRACE_ARRIVAL_ISSUER_KEY, \"POST\\n/v1/operator/arrival-proofs\\n\" + timestamp))`（paddingなし）を設定します。有効期間は前後15秒、リクエスト署名はメソッドとパスに限定されます。固定鍵自体をネットワーク送信せず、署名を短命の運営資格として使います。投影機/運営サーバーがこの方法で取得したレスポンスの`checkpointToken`という**文字列だけ**をWeb URLではなくQRにエンコードし、例えば15秒ごとに交換します。`expiresAt`（UTC）到達後は掲示しません。端末はWeb App内のカメラで読み、既存Memoryセッションのまま送信します。会場用機器からのみ運営経路に到達できるよう、リバースプロキシ/Workerにも運営APIのアクセス制御を設定してください。ブラウザのOriginを伴う運営API要求は拒否します。参加用Bearer tokenやEdge共有シークレットだけでは発行できません。
配置先が`TRACE_EDGE_SHARED_SECRET`を要求する場合、運営用プライベート経路も`X-Trace-Edge-Secret`を同時に送ります。サイトWorkerは参加者用`/api/backend/*`から`/v1/operator/*`への転送を拒否し、運営用の認可済みプライベート入口を別に用意してください（署名認証だけをネットワーク分離の代替にしない）。現行運営監査は個人操作者ではなく資格情報を保有する`arrival_issuer`サービス名、操作時刻、到着IDのハッシュ短縮値、結果を24時間保持します。個人別の監査・運営権限分離は運営UIを構築する際に別途必要です。

発行されるproofは`base64url(JSON).base64url(HMAC-SHA256(TRACE_ARRIVAL_PROOF_KEY, base64url(JSON)))`です。JSONには`checkpointId`、`venue`、`issuedAt`/`expiresAt`（Unix秒）、ランダム`nonce`を含み、有効期間は25秒です。参加端末は`POST /v1/memory-sessions/{memorySessionId}/arrival`へMemory Bearer、`Idempotency-Key`（ランダム値）、JSON `{\"checkpointToken\":\"...\",\"experienceLinkToken\":\"...\",\"clientTime\":\"...\"}`を送ります。`clientTime`は判定に使わずサーバー時刻を正とします。改変・他会場・期限切れ・未来発行は403、別トークンによる別セッションへの操作は認証で拒否します。投影QRは複数来場者が読むため同一proofを**別Memory間では共有可能**です。同一Memoryへの二重到着は1件のArrivalEventとなり、24時間の同一キー/同一内容の再送は期限切れQRでも元の結果を返します。同一キー/別内容は409、Memory削除と同時に到着・再送記録も消えます。

有効なExperienceLinkを持つ場合は当該link tokenを照合し、到着イベントと追跡中・来場前の対応Liveセッションの`arrived`更新を同一トランザクションで確定します。Liveを本人が既に停止していてもMemoryの到着は記録し、停止済み追跡は再開しません。GPSは`tracking`のまま、`on_site`は従来どおり本人の追加同意版が必要です。`GET /v1/experiences/{experienceId}`には`replayStatus`（未到着なら空文字、到着後`queued`/`playing`/`completed`/`skipped`）だけを追加し、待ち順、推定待ち時間、表示aliasの待機通知は返しません。人の列が待ち順です。到着は写真機能無効でも有効、0枚・1枚でもキューに残ります。pending写真があれば最大15秒だけ候補抽出を遅らせ、未完了は候補から除きます。永続FIFOは最大32件、満杯でも到着自体を記録して`skipped`（`queue_full`）とし、開始されず2分経った候補も`skipped`（`timeout`）へ遷移します。本人`POST .../arrival/skip`は未再生の演出をスキップします。

運営側`GET /v1/operator/arrivals`は同じ署名方式（`GET\n/v1/operator/arrivals\n`）で再生可能な到着IDとready写真**件数**だけを返します。`POST /v1/operator/arrivals/{arrivalId}/skip`は当該パスで署名してスキップできます。`POST /v1/operator/arrivals/{arrivalId}/retry`は描画機障害によるskipに限り、同じ運営署名で明示的に再キュー投入します。本人GETにキュー順は出しません。演出の状態変更は主描画機leaseを確認したWSSだけが行い、運営候補照会では`playing`へ移りません。適用DBは`004_route_segments.sql`、`005_arrivals.sql`、`006_display.sql`の順です。展開前に受付停止・API旧revision停止・明示migration・実行ユーザーへの権限再適用が必要です。

## 描画WebSocket（既定無効）

`TRACE_DISPLAY_ENABLED=false`が既定です。有効化するには参加者・到着運営・Edgeとは異なる32バイト以上の`TRACE_DISPLAY_ISSUER_KEY`と`TRACE_DISPLAY_TOKEN_KEY`が必須です。写真の有無を問わず起動時に座標/CampusGraph/3D Map版を埋め込みCampusGraphと照合し、既定の未審査bundleとは不一致なら起動を拒否します。秘密値はSecret Managerで個別管理し、署名鍵をローテーションすると既発行資格を即時失効します。描画PCが外向きHTTPS/WSSでアクセスする**承認済み専用経路**を用意してください。参加者サイトWorkerは`/v1/display/*`と`/v1/operator/*`を転送しないでください。Edge共有シークレットが有効なら承認済み経路から`X-Trace-Edge-Secret`も送ります。WSSトークンをURL、ブラウザ、ログ、写真やQRへ載せないでください。

運営の専用発行器から空bodyの`POST /v1/operator/display-tokens`へ、`X-Trace-Display-Issuer-Timestamp: <Unix秒>`と`X-Trace-Display-Issuer-Signature: base64url(HMAC-SHA256(TRACE_DISPLAY_ISSUER_KEY, \"POST\\n/v1/operator/display-tokens\\n\" + timestamp))`を送ります（時差最大15秒）。レスポンス`displayToken`は2分有効で、参加者Bearer/到着運営署名と互換性がありません。`GET /v1/display/ws`へ`Authorization: Bearer <displayToken>`、`X-Trace-Schema-Version: 1`、`X-Trace-Coordinate-Version`、`X-Trace-Campus-Graph-Version`、`X-Trace-Three-D-Map-Version`（現行設定と完全一致）を付け、Origin・query stringは付けません。不一致はHTTP 409、資格エラーは401、機能無効は503です。

WS接続直後の`display.snapshot`は`data.cursor`、量子化済み最新位置の`participants`、再生待ち/再生中の`arrivals`を含みます。写真marker配列は#10統合前なので**含まれません**。到着・再生イベントの`eventId`はDB連番で、欠番や再接続時はsnapshotで全置換します。`position.updated`、`position.expired`、`tracking.stopped`は各接続で最新のGoメモリ状態から生成する一時的な差分であり、`eventId`は不透明な重複排除キーです。位置差分はDBに保存・再送せず、位置のTTLは最大60秒、接続断後は次のsnapshotで復元します。写真/trajectory未提供の`participant.arrived`、`participant.phase.changed`、`participant.on_site`、`arrival.replay.requested/started/completed/failed/skipped`は永続差分です。画像、写真URL、元のGPS、参加者資格、内部セッションIDは含みません。映像へ正確な時刻やIDを表示しないでください。

送信JSONはすべて`schemaVersion:1`、3種のversion、`type`を含めます。`renderer.primary.acquire`で主リースを要求し、`renderer.primary.granted`の`data.leaseId`を保存します。他接続には`renderer.primary.conflict`を返します。`arrival.replay.started`、`arrival.replay.completed`、`arrival.replay.failed`にはクライアント生成の一意な`eventId`（8～128文字）、`arrivalId`、`leaseId`を指定します。成功は`renderer.ack`、競合・失効は`renderer.rejected`です。同じeventId・同じ操作の再送は24時間同じ結果となり、異なる内容は拒否します。切断/30秒heartbeat欠落/リース期限切れは再生中を`skipped`に確定し、別端末による自動再生はしません。運営の署名済み明示retryのみが再投入できます。10秒間隔のping/pong（または`renderer.heartbeat`）と30秒応答期限を設け、50分でWSを終了します。Cloud Run設定は3600秒で、クライアント側は切断時に指数バックオフで**新しい資格を取得し**snapshotから再接続します。

最新の量子化済み位置は単一BackendのGoメモリにのみ保持し、PostgreSQLには保存しません。到着・再生イベントと主描画機leaseだけをDBに保存します。複数Backend間で位置は共有されないため、**APIとWebSocketは同じ1インスタンス**へ接続し、Cloud Runの最大インスタンス数を1としてください。旧revisionと並存させると表示が欠落し得るため、受付停止・旧revision停止後に切り替えます。複数インスタンス化には仕様で定めるRedis互換TTLストア/メッセージ基盤への移行と容量・プライバシー承認が必要です。DB migration `006_display.sql`と`deploy/gcp/runtime-grants.sql`を適用してください。

## 非公開写真の設定とライフサイクル

- `TRACE_PHOTO_ENABLED=false`が既定です。`true`にするには`TRACE_PHOTO_BUCKET`と`TRACE_PHOTO_SIGNER_EMAIL`の両方が必要です。
- 保存先設定があれば、新規写真を無効にしても既存写真の本人閲覧・削除と清掃は継続します。写真・アップロード枠が残る間に保存先設定を外すと、起動・清掃コマンドは拒否します。
- bucketは東京`ASIA-NORTHEAST1`、Uniform bucket-level accessとPublic access preventionを有効にし、versioning・soft delete・retention/hold・独立したlifecycleルールを無効にします。起動時に検査し、稼働中に変更しないでください。
- ADCとIAM `signBlob`を使用し、秘密鍵ファイルやローカルファイル保存へのフォールバックはありません。APIと清掃ジョブへ同じ保存先を設定します。必要権限とCORSは[配置手順](docs/google-cloud.md)を参照してください。

写真はJPEG/WebP、5MiB以下、長辺1920px以下、1セッション12枚までです。登録時に撮影時GPSを省略でき、通常GPSから補完しません。保存後にサイズ・SHA-256・実形式・寸法・画像全体の復号・EXIF等の不在を検証するまで、画像URLは返しません。ブラウザの構造的なJFIF/Adobe情報は許可しますが、埋め込みサムネイル・任意メタデータ・アニメーションは拒否します。

WebPはVP8Lの過大割り当てを修正した公式`x/image v0.47.0`を使い、独自デコーダのコピーは保持しません。公式のentropy group上限・疎なindexの再マッピングに加え、プロセス内のWebP全体復号を同時1件に制限します。待機はcontextで取消可能で、画像とALPHの両方の悪意ある入力を境界テストで検証します。512MiB構成での実負荷・接続待ちへの影響はstagingで確認してください。

登録の同じ`clientPhotoId`と同じ内容は同じPhotoMomentへ集約します。同じ冪等性キーで異なる内容は409です。削除済み写真の登録再送は404となり、復活させません。登録の再送でURL期限が切れていた場合は`uploadUrl`を省略するため、`pending`に対して`upload-url`を要求します。再発行は初回を含め5回まで、上限到達は409です。

アップロードURLは最大5分（Memoryの`expiresAt`以下）の新規作成専用PUTです。返却する`uploadHeaders`の`Content-Type`、`x-goog-if-generation-match: 0`、`Cache-Control`を送信し、`Content-Length`はブラウザがBlobサイズから自動設定します。上書き・読み取り・一覧には使えません。`complete`はbodyなしで何度送っても同じ写真の状態へ収束します。未アップロードは409、不正画像は消去後`failed`となり、消去に失敗した間は`deleting`で新しいURLを発行しません。

本人memoryは検証済み`ready`だけを撮影時刻順に返し、画像URLは5分と`deleteAfter`の早い方までです。`locationConfidence`はGPSなし/精度100m超をlow、30m超〜100mをmedium、30m以下をhighとする暫定値で、経路の信頼度ではありません。写真一覧には画像URLを含めません。既存の`photoSummary`はDBの実件数から計算します。
写真が`ready`になると、撮影時刻順の隣り合う写真ごとに`RouteSegment`を同一DBトランザクションで作成します。個別削除要求では`deleting`になった写真をただちに除外して残る前後の写真間を再計算し、画像消去完了・失敗時にも整合させます。写真0枚・1枚なら区間は空のままで到着/終了を妨げません。既存写真を保持するDBへ移行したときは本人memory取得時に欠損区間を再計算します。
到着QRの確定情報は現時点ではブースcheckpointIdのみです。承認済みの会場checkpoint→CampusGraph座標の対応がないため、最後の写真から到着地点への区間は捏造しません。この依存が解決するまでは写真間の経路のみを提供します。

CampusGraphは`internal/route/campus_graph.json`へ固定同梱します（TRACE_F main、2026-10-23取得、JSON記載版`toyosu-graph-2026-10-21.1.draft`）。Graph付属READMEの記述版`toyosu-graph-2026-10-18.2.draft`とは異なるため、JSON自身の版を正として使います。Graphの`coordinateVersion`=`toyosu-2026-08-28.1`、`threeDMapVersion`=`キャンパス全体.blend 2026-10-21`と公開設定の三つが一致しないと写真ストレージを用いるAPI/清掃ジョブの起動を拒否します。既定の`campus-2026-01`等を黙って上書きしません。承認済みの地図・表示機側にも同じ版を事前配布・ヘルス確認してから`TRACE_COORDINATE_VERSION`、`TRACE_CAMPUS_GRAPH_VERSION`、`TRACE_THREE_D_MAP_VERSION`と豊洲のジオフェンスを設定し、受け付け切替・ロールバックを行ってください。Graphの更新では固定JSON、互換版、試験点の測定を併せて更新します。

アンカーはGraphのローカルENU（東=x、北=z）へ変換し、GPS精度円内の通行可能な`gpsSnap=true`両端を持つ辺だけへ投影します。`passable=false`、`restricted=true`、`closed=true`、時刻付き`closures`内、および`openingHours`（曜日0=日曜〜6=土曜、`open`/`close`は`HH:MM`・日本時間）の時間外辺は候補・探索の両方から除外します。現行Graphは`passable`のみを収録し、現場の一時通行止め・利用時間の運用入力は未提供です。運用データが正式に渡されるまでは実際の臨時閉鎖や時間外利用を防げないため公開運用に使用しないでください。階や屋内はGPSで確定できないため`gpsSnap=false`の辺へ吸着しません。

本人専用memoryレスポンスの`segments`は`routeSegmentId`、両端の写真ID、`coordinateVersion`、`campusGraphVersion`、`threeDMapVersion`、`algorithmVersion`=`photo-edges-dijkstra-1`、`confidence`、`lengthM`、`path`を返します。`path`はGraph沿いの表示用ローカル点列`{t,x,z}`で、GPS履歴や生緯度経度は含みません。同程度の複数辺・撮影GPS欠損/精度不足・撮影地点から60秒超過去/10秒超未来の位置情報・経路不在・極端な移動速度は`confidence=low,path=[]`です。推定経路は実歩行の完全再現ではありません。公開イベント/スクリーンへ本人の写真・正確な撮影時刻・経路を流しません。

削除はまず永続的な`deleting`を確定し、画像の消去が成功するまで参照・認証材料を捨てません。写真DELETE成功は204、写真・Memoryの消去失敗は503となり、同じ冪等性キーで再試行します。体験単位削除は従来どおりLive削除後にMemoryを処理し、写真消去失敗時は`memory: pending`です。期限切れはAPI常駐清掃と清掃ジョブで同じ処理を再開します。

署名付きPUTは削除後も期限まで生きるため、アップロード先はゼロバイトで占有し、期限後だけoutboxから消去します。これにより削除後の再送で画像が復活しません。画像バイト・GPS・PhotoMoment参照とそれに紐づくRouteSegmentは削除対象となり、この短期outboxにはランダムなobject keyと期限だけを保持します。ピン・派生特徴・別サムネイルはまだ生成しません。

DBに`004_route_segments.sql`を追加します。適用後に`runtime-grants.sql`を実行し、APIと清掃ジョブを同じschema対応版へ更新してください。旧バイナリは新版schemaを拒否するため、受付停止の移行窓と手動migration確認が必要です。SDKの検証は公式クライアントのHTTP fixture、API実動作は専用PostgreSQLと明示的な一時object emulatorで確認しています。実GCSのIAM・bucket CORS・端末アップロードは、承認済みstagingで別途必要です。

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
APIと清掃ジョブは起動時、APIは`/health/ready`と各`/v1/`リクエスト、定期清掃は実行前に、埋め込まれたmigrationの最終版とDBの連続した適用履歴が一致することを確認します。DBが古い・新しい・履歴に欠番がある場合は受付・清掃を行いません。既存の旧バイナリは新版の検査を持たないため、非互換migrationの前に停止・置換が必要です。APIユーザーにはスキーマ変更権限を付けず、新規テーブルの必要権限を`deploy/gcp/runtime-grants.sql`へ明示的に追加してから配信します。[stagingでの手順](docs/staging-cicd.md#6-運用と復旧)を参照してください。

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

検証対象：既存API導線、時刻型の往復、資格情報分離、複数接続からの同時削除、停止と位置保存の競合、再送記録失敗時のロールバック、保存期限・関連データ削除、DB migrationの初回・追加・再実行・並行実行・失敗時ロールバック・不整合履歴・旧アプリのready拒否、CampusGraph上の既知経路/通行禁止/時間外/複数候補/欠損、Readyと写真削除時の区間再生成、Cloud Runポート・Cloud SQL接続設定。到着の統合テストには運営/参加者資格分離、改変・期限境界・会場、重複・別端末・同時削除、キュー上限・タイムアウト、写真0/1枚・pending15秒、skip・監査を追加しています。

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
