ALTER TABLE deliveries ADD COLUMN route_attempts integer NOT NULL DEFAULT 0 CHECK (route_attempts >= 0);
