UPDATE push_subscriptions
SET next_send_at = next_send_at + INTERVAL '15 minutes', interval_minutes = 30
WHERE interval_minutes = 15;

ALTER TABLE push_subscriptions DROP CONSTRAINT push_subscriptions_interval_minutes_check;
ALTER TABLE push_subscriptions ADD CONSTRAINT push_subscriptions_interval_minutes_check CHECK (interval_minutes = 30);
