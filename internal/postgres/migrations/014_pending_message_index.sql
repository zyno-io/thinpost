-- This derived flag is transactional with delivery results. Default true is
-- conservative for unprepared, held, corrupt or newly inserted messages.
ALTER TABLE messages ADD COLUMN pending boolean NOT NULL DEFAULT true;

-- Existing prepared messages leave the pending set only when they have at
-- least one delivery and every delivery is terminal. Migration runs with mail
-- admission and processing stopped, so no old writer can race this backfill.
UPDATE messages m SET pending=false
WHERE m.state='prepared'
  AND EXISTS (SELECT 1 FROM deliveries d WHERE d.message_id=m.id)
  AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.message_id=m.id AND d.state NOT IN ('accepted','failed'));

CREATE INDEX messages_app_pending_v2_idx ON messages (app_id,id) WHERE pending;
DROP INDEX messages_app_pending_idx;
