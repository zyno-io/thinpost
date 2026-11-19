# Roadmap

This is a statement of direction, not a commitment or a schedule. Items are grouped by theme and are in no particular order. Shipped behavior is described in the [documentation](https://zyno-io.github.io/thinpost/); open an issue to discuss anything here.

## Delivery reliability and recovery

- Drained controller hand-over for planned updates, and tested failover across control-plane replicas.
- Stronger recovery around uncertain SMTP attempts: bounded reconciliation, a journal that detects its own rollback, and clearer operator tooling for resolving held deliveries.
- A rehearsed, documented coordinated restore of the database, object store, key rings and edge journal, including recovery inventory of stored objects.
- Better visibility into delivery retries and uncertain outcomes.

## Edge networking and SMTP policy

- Authenticated MX discovery with MTA-STS and DANE, and RFC 8689 REQUIRETLS for onward hops where supported.
- Authenticated DNS policy (DNSSEC) for the MX resolver.
- Per-sending-IP concurrency and rate controls, so address warm-up does not depend on manual app assignment.
- Further hardening and testing of outbound destination restrictions, including DNS rebinding and mixed address families.
- Readiness checks and alerts for the edge journal and worker.

## Credentials, keys and certificates

- Coordinated rollover and revocation for the private CA that secures the control channel, and verified certificate renewal.
- Lifecycle management for shared key rings: the idempotency fingerprint key, the bounce key ring and the object-encryption key ring, including inventory of old key versions.
- Automated DKIM key provisioning and rotation, with sender-domain readiness checks for DKIM, SPF and DMARC and DNSSEC policy.
- Sender-domain removal.
- Optional DNS-provider automation for domain onboarding.

## Provider compatibility and API

- SendGrid webhooks and click events in the provider webhook formats.
- Closer parity with each provider's errors, batch behavior, retries and idempotency where practical.
- Native API follow-ups: a batch send route, and app, key and domain administration through the native API. Sending one message is available as `POST /app/v1/messages`.
- More SDK languages and versions in the contract test suite.
- Suppression events as webhooks, DSN-based hard-bounce suppression and mailto unsubscribe.
- Search over received mail.

## Webhooks

- Defined event and delivery-history retention.
- Additional operational controls for dispatch and recovery of pending results.

## Storage, privacy and retention

- Defined encryption, access, retention and deletion policy for message bodies, attachments and event data, including trimming inbound metadata after content expires.
- Production-grade key rotation for the object-encryption key ring.
- Lifecycle management for idempotency records and stored objects.

## Operations and observability

- A defined set of metrics, alerts, health and readiness checks, audit and rate limits.
- Documented backup, restore, upgrade and rollback procedures for every component.
- Broader interoperability and failure testing: SMTP interoperability, ambiguity handling, worker fencing and concurrent replicas.

## Operator console

- Continued hardening of how stored email HTML and webhook content is displayed.
- More operational data in the console, where the API can provide it.
