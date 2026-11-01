-- 到着後通知の同意方針は新規参加時だけ固定する。既存参加者・旧writerは延長しない。
ALTER TABLE memory_sessions ADD COLUMN push_after_arrival BOOLEAN NOT NULL DEFAULT FALSE;
