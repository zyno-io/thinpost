---
title: Suppressions
description: How the per-app suppression list works, what adds an address to it automatically, and how spam-complaint feedback loops feed it.
---

# Suppressions

Each app has one **suppression list**: addresses Thinpost will not send to for that app. It protects your reputation by honoring bounces, complaints and unsubscribes without your code having to.

## How the list behaves

- Addresses are lowercased, and matching is case-insensitive on the whole address. Unicode and punycode forms of an internationalized address do not match each other.
- The check runs **per SMTP attempt**, when the delivery is claimed, against the current list. A suppression added while an attempt is in flight does not stop it, and it is not retroactive for mail already sent.
- A suppressed recipient's delivery fails with the local outcome `suppressed` and **no SMTP attempt**. If every recipient of a message is suppressed, the message's status is `suppressed`. A partly suppressed message is accepted and the suppressed recipients drop out.
- Suppressed recipients still count toward the app's daily recipient limit, which is charged when the message is accepted.
- Removing an entry does not resend deliveries already dropped. Your app must resubmit.
- Cc and Bcc recipients are checked like To recipients.

Entries are stored in plaintext in PostgreSQL, so they appear in database backups. No address is logged, audited or put in a URL.

## Where entries come from

Each entry has a **reason** and an **origin**.

| Reason | Added when | Origin |
| --- | --- | --- |
| `hard_bounce` | A recipient's server rejects `RCPT TO` with a 5xx whose enhanced status code is `5.1.1` or `5.1.10` (unknown user). Added in the same transaction that records the result. | `recipient` |
| `spam_complaint` | An accepted feedback-loop report names the delivery (see below). | `recipient` |
| `unsubscribe` | The recipient uses one-click unsubscribe ([tracking](/guide/tracking#one-click-unsubscribe)). | `recipient` |
| `manual` | An administrator or the provider suppression API adds it. | `console` or `api` |

Automatic hard-bounce suppression is conservative. Nothing is added for `MAIL FROM` or `DATA` rejections (IP blocks, content), 4xx replies, a missing or unusual enhanced code (such as `550 5.5.0`), `null_mx`, `domain_not_found`, or an unverified bounce receipt. Only the code and stage are kept, never the reply text. A DSN-based bounce path is not built yet.

A recipient-originated entry **replaces a weaker one**, in this order of strength: `spam_complaint`, `unsubscribe`, `hard_bounce`, `manual`. A later unsubscribe from a different message suppresses the address again. A repeated report changes nothing but a counter, so a replayed old report never restores a suppression an operator removed.

## Managing the list

In the console, open the app and choose **Suppressions**. You can filter by reason, search by address prefix or `@domain` for an exact domain, add up to 100 addresses at a time, and remove entries singly or in bulk. Removing a `spam_complaint` or `unsubscribe` entry, which is the recipient's own request, asks for an extra confirmation. Message detail links to **Find in suppressions** for a suppressed delivery and offers **Suppress recipient** for a failed one.

Searches and removals send the address in a request body, never in the URL, so addresses stay out of access logs. The admin API mirrors this: `GET /apps/{appId}/suppressions`, `POST .../suppressions` to add, `POST .../suppressions/search` and `POST .../suppressions/delete` (by suppression ID).

### Provider suppression APIs

Apps can manage their own list through the provider API they already use, with their app key. Only Postmark and Resend have such APIs; Mailgun and SendGrid do not.

- **Postmark.** `GET /message-streams/outbound/suppressions/dump` (filters: `SuppressionReason`, `Origin`, `fromdate`, `todate`), `POST .../suppressions` and `POST .../suppressions/delete`, at most 50 addresses per call. Only the `outbound` stream exists; any other is 422. Adds have reason `manual`. A `Customer`-origin entry can be removed, but entries created by the recipient side (hard bounce, spam complaint, unsubscribe) cannot, and report `Failed`. Only the console can remove them. A send where every distinct recipient is suppressed returns 422 with `ErrorCode` 406.
- **Resend.** `GET /suppressions` with `limit`, `after`, `before` and `origin` (`bounce`, `complaint`, `manual`).

Routes that put an address in a URL (Postmark `dump?EmailAddress=`, and Resend routes addressing a single entry) are not implemented, because ingress logs would record the address.

## Spam complaints and feedback loops

Mailbox providers can send abuse reports (ARF, RFC 5965) when a recipient marks a message as spam. Thinpost receives them at one global mailbox, `b+<token>@<main HELO name>`, on the edge's existing bounce listener. The name is the HELO name of the sending worker's main IP, as it last reported it. The mailbox can't be created before the worker has connected. If you rename the main IP, rotate the mailbox and register the new address.

Set it up in the console under **Operations, Feedback loops**:

1. Create the feedback mailbox. Rotating it replaces the address, and the old one stops receiving at once.
2. Add the reporters you expect to the **reporter allowlist** (at most 32 domains). It is empty by default, so nothing is accepted until you configure it.
3. Register the mailbox address with each provider's feedback program.

A report counts only when **both** of these hold:

- it has a valid DKIM signature whose `d=` equals or is a subdomain of an allowlisted reporter domain, and
- the original message embedded in the report has a `Message-ID` that belongs to an accepted delivery whose sender domain matches. When the original has a `Return-Path`, it must be one delivery's own bounce address, and an absent `Return-Path` is accepted only when the `Message-ID` identifies a single delivery, since reporters often redact it.

An accepted report records one `spam_complaint` suppression, one `complained` [webhook](/guide/webhooks) event and a feedback record, all in a single transaction, and leaves the delivery `accepted`. The address comes from the delivery's recorded recipient, never from the report. Reports are parsed in memory and never stored or logged. Rejected or ignored reports are acknowledged and only increment a daily counter, shown for 14 days. A closed gate, a failed commit or a temporary DNS failure makes the edge answer 451 so the sender retries.

Provider notes:

- **Yahoo** signs as `arf.mail.yahoo.com` and is registered per sending (DKIM) domain by that domain's owner.
- **Microsoft** registers per sending IP. Its report format and signer are not documented, so confirm both from the first real report and watch the `unauthenticated` and `uncorrelated` counters.
- **Gmail** has no per-message feedback loop; Postmaster Tools reports aggregates only.

## Next

- [Inbound mail](/guide/inbound)
- [Webhooks](/guide/webhooks)
