CREATE TABLE registration_control (
 singleton BOOLEAN PRIMARY KEY CHECK (singleton),
 closed_at TIMESTAMPTZ
);
INSERT INTO registration_control (singleton) VALUES (TRUE);
