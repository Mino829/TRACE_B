# TRACE Backend

TRACEキャンパス展示のバックエンドMVPです。設計資料のうち、匿名参加セッションと短期リアルタイム位置の安全な基盤を先に実装しています。

## 現在利用できる機能

- MemorySession / LivePresenceSession / ExperienceLinkの分離発行
- 参加者ごとに異なるBearer token認証（DBにはSHA-256ハッシュだけを保存）
- 同意文バージョンの検証と公開設定API
- GPSのgeofence・精度・時刻・不自然なジャンプ検証
- 生緯度経度を保存せず、ローカル座標へ変換・量子化した最新1件だけをSQLiteへ保存
- 位置の60秒TTL、停止時の即時消去、期限データの定期清掃
- `on_site`への追加同意または追跡終了
- MemorySession / LivePresenceSessionの個別削除
- 停止・削除の24時間冪等性
- CORS制限、16KB JSON上限、統一エラー形式、ヘルスチェック

実装済みのエンドポイント:

```text
GET    /health/live
GET    /health/ready
GET    /v1/public/config
POST   /v1/experiences
GET    /v1/experiences/{experienceId}
POST   /v1/live-presence-sessions/{liveSessionId}/position
POST   /v1/live-presence-sessions/{liveSessionId}/on-site
POST   /v1/live-presence-sessions/{liveSessionId}/stop
DELETE /v1/memory-sessions/{memorySessionId}
DELETE /v1/live-presence-sessions/{liveSessionId}
```

## ローカル起動

Go 1.25以降を使用します。

```sh
cp .env.example .env
mkdir -p data
set -a
source .env
set +a
go run ./cmd/server
```

別のターミナルから準備状態を確認します。

```sh
curl http://localhost:8080/health/ready
curl http://localhost:8080/v1/public/config
```

環境変数を読み込まない場合は、カレントディレクトリの`trace.db`を使って起動します。

## Docker起動

```sh
cp .env.example .env
docker compose up --build
```

SQLiteファイルはDocker volume `trace-data`へ保存されます。

## 設定時の注意

`.env.example`の次の値はサンプルです。実地投入前に、測量・現地試験で確定したキャンパス境界と3D座標原点へ必ず変更してください。

- `TRACE_MIN_LATITUDE` / `TRACE_MAX_LATITUDE`
- `TRACE_MIN_LONGITUDE` / `TRACE_MAX_LONGITUDE`
- `TRACE_ORIGIN_LATITUDE` / `TRACE_ORIGIN_LONGITUDE`
- `TRACE_ALLOWED_ORIGINS`
- 各同意文バージョンと本文

本番で複数Backendインスタンスを動かす場合はSQLiteを使わず、PostgreSQL対応を追加してください。

## テスト

```sh
go test ./...
go vet ./...
```

テストでは、セッション作成、同意版拒否、認証、位置量子化、停止後の位置消去、冪等性競合、削除再送を確認します。

## 次の実装範囲

設計上の次段階は、写真の非公開オブジェクト保存、PhotoMoment、経路グラフとA*、到着QR署名、描画WebSocket、運営API、レート制限です。公開設定の`photo.enabled`は、それらが完成するまで`false`を返します。
