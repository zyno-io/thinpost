---
title: Operator console
description: A tour of the Thinpost operator console, what each screen does, and the privacy rules it follows.
---

# Operator console

The operator console is the web interface for administrators. It is a Vue single-page application bundled into the control image and served from the same origin as the admin API (`/api/v1`), so a deployment needs no separate hosting. The console is a client of the [admin API](/reference/api/admin): anything you can do in it, you can script.

## Signing in

The console signs operators in through your OpenID Connect provider using the authorization-code flow with PKCE. It reads the public parameters (issuer, client ID and scopes) from `GET /api/v1/config`, which holds no client secret.

Register two URLs with your identity provider: `<origin>/auth/callback` as the redirect URI and `<origin>/` as the post-logout redirect. The signed-in user is kept in the browser's local storage, so reloads and new tabs stay signed in until the token expires or you sign out, and other tabs sign out with you. A refresh token is never stored.

Only users whose access token carries the configured administrator role (and scope, when one is required) are administrators. See [Operator authentication](/deploy/operator-auth).

## Applications

The Applications screen lists apps and creates new ones. An app opens into its own area with these tabs, in order:

| Tab | What you do there |
| --- | --- |
| **API keys** | View and set the app's sending limit, daily recipient limit and pending-message limit (below), then list, issue and revoke its keys. A new key's secret appears once in a dialog, is held only in memory and is discarded when you close it. It is never written to browser storage, the URL or logs. |
| **Sending domains** | Request a domain, copy its TXT challenge, verify it, reissue a pending challenge and start a test send from a verified domain. The challenge value is shown only on the page that requested it. DKIM, SPF and DMARC readiness are shown separately because the API does not report them. See [Sender domains](/guide/domains). |
| **Messages** | History with cursor paging, search, message detail and **Send test message** (below). |
| **Webhooks** | Create, edit, enable, disable, test and delete endpoints, view delivery history and replay failed deliveries. See [Webhooks](/guide/webhooks). |
| **Tracking** | Open and click defaults, one-click unsubscribe, the daily event budget, deployment state and the day's recorded and unrecorded events. See [Open and click tracking](/guide/tracking). |
| **Suppressions** | Search, filter, add and remove entries. See [Suppressions](/guide/suppressions). |
| **Inbound** | Receiving domains, per-app limits, received messages and their webhook history. See [Inbound mail](/guide/inbound). |

The app area stays mounted as you move between tabs. The console remembers the last app you used so that the shortcuts `/domains`, `/messages` and `/webhooks` go straight to it. The app picker appears only when none is remembered.

### App limits

The **API keys** tab has three panels that show and set an app's submission limits. Each shows the current setting and when it was loaded, has **Reload limit** and **Change limit** buttons. A failed load or save says why, and a change with an unknown outcome is flagged until you reload.

- **Sending limit.** The submission rate: messages per minute and a burst size.
- **Daily recipient limit.** The number of recipients the app can send to each UTC day. The panel shows how many have been used today (UTC) and the day. Changing the limit does not reset today's count.
- **Pending-message limit.** The number of the app's messages that can be pending at once. The panel shows the pending count, which is a snapshot. When more messages are pending than Thinpost counts, it shows a lower bound ("or more") and says it is approximate.

A limit of **zero blocks all new messages** for the app, and every new app starts at zero for all three. In the dialog, choose **Block all new messages** to set zero (it asks you to acknowledge), or **Allow, up to a limit**. For the daily recipient and pending-message limits, enter a whole number from 1 up to the maximum (1,000,000 recipients per day, 100,000 pending messages); the sending limit takes a rate from 1 to 60,000 messages per minute and a burst from 1 to 10,000. Changes take effect immediately and are audited. Messages already accepted are not affected. The admin API equivalents are `GET` and `PUT` on `/api/v1/apps/{appId}/submission-rate`, `/recipient-quota` and `/pending-quota`.

### Messages

- **List.** Accepted time, message ID, sender domain, first recipient (with a count of others), subject, an overall status (queued through delivered) and delivery counts. Messages accepted before the console recorded this metadata show "Not recorded". A message [submitted on hold](/guide/holding-messages) shows **On hold by app**, then **Cancelled** or **Hold expired** if it was never released.
- **Search.** By message ID, recipient, recipient domain, sender domain or subject. Terms go in a `POST` body and are kept only in memory, never in the URL, router state, document title, browser storage, logs or notifications. They are not restored after a reload. Pasting an exact 32-character ID opens that message.
- **Detail.** Sender domain, recipients, and per-delivery state with the retry count, earliest next attempt and latest outcome. HTML content appears on **Rendered**, **Text** and **HTML source** tabs. The rendered view is sanitized, shown in a `sandbox` iframe with its own policy, and blocks all remote content, with a count of what it blocked. Inline `cid:` images are resolved within size limits. Attachments download as `application/octet-stream`. A message submitted as a complete email over SMTP shows its parsed content, with its attachments inside the original: **Download original (.eml)** saves the message exactly as received, also as `application/octet-stream`. A **Headers** view shows every synthesized header in order. Everything is held in memory and discarded when you leave. A held message also shows its hold: when it expires, or when it was released, cancelled or expired. Operators cannot release or cancel it; only the app can.
- **Enhanced Protection.** The subject of a protected message is masked in lists and excluded from search. Revealing it, showing content, downloading an attachment or the original message and viewing headers each ask for confirmation and are recorded in the audit log. Unprotected messages show none of these dialogs. See [Concepts](/guide/concepts#enhanced-protection).

### Send a test message

**Send test message** on the Messages tab (and **Send test** on a verified domain's row) sends one real message as the app, to check that delivery works end to end. You choose a local part and one of the app's verified sending domains for From, an optional display name, one to five To recipients, a subject and a text body (both prefilled with neutral text), an optional HTML body, and optionally Enhanced Protection and the two options under [Test send options](#test-send-options). There is no Cc, Bcc, reply address, custom header, attachment, tracking choice or hold in a test send.

By default, a test send is a normal submission as far as Thinpost is concerned:

- It goes through the same acceptance path as the app API, so sender-domain verification, the recipient rules, the [suppression list](/guide/suppressions), the submission rate, the daily recipient quota and the pending-message limit all apply, and it counts against them. A new app has a submission rate, daily recipient limit and pending-message limit of zero and refuses test sends until each is set in its panel on the **API keys** tab (see [App limits](#app-limits)).
- The app's [webhooks](/guide/webhooks) receive its events like those of any other message.
- Open and click tracking and the one-click unsubscribe link follow the app's settings. If a recipient clicks unsubscribe, that address is suppressed for the app.
- A paused app refuses it, and the button is disabled with an explanation. It is also disabled when the server has no submission configured, or when you are signed out. It needs no API key.

The console shows the message detail once Thinpost accepts the message. The form is held only in memory. The console never retries the request. If no answer arrives, or Thinpost answers with a server error, the console cannot tell whether the message was queued: it reloads the message list and blocks another test send until that reload succeeds, so check the list before trying again. A test send has no idempotency key, so a second send after a lost answer can deliver twice.

Each test send is audited as `message.test.submitted` with the administrator as the actor, and only the recipient count and the two option flags as detail. The message list and the message detail tag it **Console test**, and add **limits skipped** and **allowlist skipped** when those options were used.

#### Test send options

Both options are off by default, are sent only when ticked, and exist only in the console's test send: the app API and every provider route reject them with a `400`, even when false.

- **Skip sending limits.** The send is not checked against, and is not counted in, the app's sending rate, daily recipient quota or pending-message limit. The rate bucket and the daily quota stay as they were, and the message does not count toward the app's pending-message limit (the Operations backlog still shows it). Everything else still applies: a verified sender domain, DKIM signing, the suppression list, a paused app, the deployment's admission gate, the size and profile limits, the egress policy and Enhanced Protection TLS.
- **Skip recipient allowlist.** Shown only when this deployment restricts recipients to an allowlist. The message is not held by the allowlist, so it can reach people outside it. Submitting with it ticked asks you to confirm, naming the number of recipients. It skips only the allowlist: the egress policy still refuses special-use and infrastructure destinations, and the other checks above still apply.

### Webhook history and replay

Delivery history is paged, newest first, with the latest attempts for a delivery loaded on request. It carries no payloads, secrets or claim tokens. **Replay** is offered only for failed deliveries and requires you to acknowledge that the receiver may see the same event ID again. If a replay response is lost, the console blocks further replays until you reload the history. It never retries a replay itself.

## Operations

The Operations screen is global rather than per app. It shows data returned by the operator API, and anything the API cannot provide is shown as unavailable, never as zero. It covers:

- **Runtime and operations status:** the admission gate, the edge worker's lease, message, delivery and webhook backlogs (messages on hold by their app are counted in the messages row's **Held** column), and a cross-app list of messages that need an operator.
- **Retry policy:** the delivery retry policy, which is the one setting you can edit here. The defaults are 128 attempts, a 30-minute first delay, a three-hour cap and a five-day lifetime. Changes use a revision check and are audited.
- **Sending IPs:** add, name and check the DNS of sending addresses and assign apps. See [Sending IPs](/guide/sending-ips).
- **Feedback loops:** the abuse-report mailbox, the reporter allowlist and 14 days of counters. See [Suppressions](/guide/suppressions#spam-complaints-and-feedback-loops).
- **Inbound mail today:** the day's inbound counters.

## Privacy and safety rules

- Search terms, subjects, recipients, contents, attachments and headers are never stored in the browser. Only these are: the theme preference, the last app ID, the signed-in OIDC user, and the sign-in redirect state in session storage.
- Email HTML is untrusted. The server sends a Content-Security-Policy with no remote image or frame sources and `frame-src 'none'`, and the console relies on it for the sandboxed preview.
- While a dialog is open, the navigation behind it is inert. Leaving the page while a key is being issued or a secret is displayed asks for confirmation.
- The theme menu offers system, light and dark.

## Next

- [Quickstart](/guide/quickstart)
- [Admin API reference](/reference/api/admin)
