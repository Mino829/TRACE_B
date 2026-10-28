CREATE TABLE display_fallback_control (
 singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
 mode TEXT NOT NULL CHECK (mode IN ('normal','pending','fallback')),
 switch_at TIMESTAMPTZ,
 changed_at TIMESTAMPTZ NOT NULL,
 CHECK ((mode='pending') = (switch_at IS NOT NULL))
);
INSERT INTO display_fallback_control(singleton,mode,changed_at) VALUES (TRUE,'normal',CURRENT_TIMESTAMP);
