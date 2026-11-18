-- Schema 36: spam-complaint intake from ARF feedback loops (C1).
--
-- feedback_mailbox holds the single global feedback address token. The token
-- appears in the console and is not a credential: a report is trusted only
-- through its reporter DKIM signature and its correlation to a delivery.
-- token_hash is the lookup the bounce ingest already computes. feedback_reporters
-- is the operator allowlist of reporter DKIM domains (empty means no report is
-- ever accepted). inbound_feedback_daily counts the result of every inbound
-- feedback item per UTC day (counts only; never an address or report content).
--
-- Deploy review (lock and rollout): the pre hook closes the gate before this
-- runs. Three CREATE TABLE statements touch no existing data, take no lock on an
-- existing table and need no drain. Schema-35 pods ignore the new tables and are
-- fenced by the closed gate; schema-36 pods require them. The runtime role gets
-- SELECT, INSERT and UPDATE on feedback_mailbox and inbound_feedback_daily and
-- SELECT, INSERT and DELETE on feedback_reporters; collector and retirement roles
-- get nothing. The complained outcome and the recipient_feedback table already
-- exist from migration 35.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open".
CREATE TABLE feedback_mailbox (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    token text NOT NULL CHECK (token ~ '^[0-9a-f]{32}$'),
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE feedback_reporters (
    domain text PRIMARY KEY CHECK (domain = lower(domain) AND octet_length(domain) BETWEEN 3 AND 253),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE inbound_feedback_daily (
    day date NOT NULL,
    channel text NOT NULL CHECK (channel IN ('arf','one_click')),
    result text NOT NULL CHECK (result IN ('accepted','duplicate','ignored','unauthenticated','unparsable','uncorrelated','not_delivered')),
    count integer NOT NULL CHECK (count BETWEEN 0 AND 1000000000),
    PRIMARY KEY (day, channel, result)
);
