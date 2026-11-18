-- A separate backlog limit bounds accepted messages that still require
-- preparation, delivery, recovery, or operator disposition. New and existing
-- apps default to zero until an operator explicitly configures the limit.
ALTER TABLE apps ADD COLUMN pending_message_limit integer NOT NULL DEFAULT 0
    CHECK (pending_message_limit BETWEEN 0 AND 100000);

CREATE INDEX messages_app_pending_idx ON messages (app_id, id) WHERE state <> 'prepared';
CREATE INDEX deliveries_unfinished_message_idx ON deliveries (message_id)
    WHERE state NOT IN ('accepted', 'failed');
