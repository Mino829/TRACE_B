CREATE TABLE notification_control (
 singleton BOOLEAN PRIMARY KEY CHECK (singleton),
 stopped_at TIMESTAMPTZ
);
INSERT INTO notification_control (singleton) VALUES (TRUE);
