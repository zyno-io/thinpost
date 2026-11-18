-- Support bounded app-scoped webhook history without scanning unrelated apps
-- or the complete attempt table. This does not alter delivery or retry state.
CREATE INDEX webhook_events_app_time_idx ON webhook_events (app_id, occurred_at DESC, id DESC);
CREATE INDEX webhook_attempts_delivery_time_idx ON webhook_attempts (event_id, subscription_id, started_at DESC, id DESC);
