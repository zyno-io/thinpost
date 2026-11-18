ALTER TABLE sender_domains
    ADD COLUMN verification_hash bytea CHECK (verification_hash IS NULL OR octet_length(verification_hash) = 32),
    ADD COLUMN created_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN verified_at timestamptz;
ALTER TABLE sender_domains ALTER COLUMN active SET DEFAULT false;
-- Existing rows without ownership evidence require re-verification.
UPDATE sender_domains SET active=false WHERE verified_at IS NULL;
ALTER TABLE sender_domains ADD CONSTRAINT sender_domains_active_requires_verification CHECK (NOT active OR verified_at IS NOT NULL);
