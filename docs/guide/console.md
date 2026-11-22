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
| **Sending domains** | Add a domain, copy its DKIM record (with an option to split it into 255-character strings), verify it, and start a test send from a verified domain. A domain verified before DKIM keys existed shows **No DKIM key yet** and offers **Create DKIM key**. For a verified domain, **Rotate key** makes a new key; **Check record**, **Switch** and **Cancel** take it from there, and the old record's keep-until date is shown after a switch. The record is always available to copy. Under each verified domain, a **bounce domain** panel adds, checks (**Check now**), changes and removes the domain's own return-address domain, shows the CNAME to add (DNS only, not proxied) and says whether it is in use. Where tracking is available and `THINPOST_ADMIN_ORIGIN` is set, a **tracking domain** panel does the same for the domain's own link address (see [Your own tracking domain](/guide/tracking#your-own-tracking-domain)). Where `THINPOST_ADMIN_ORIGIN` is not set, the panel appears only for a domain that already has a tracking domain, shows it as **Not in use** and offers no add or change. SPF and DMARC readiness are not reported. See [Sender domains](/guide/domains). |
| **Messages** | History with cursor paging, search, message detail and **Send test message** (below). |
| **Webhooks** | Create, edit, enable, disable, test and delete endpoints, view delivery history and replay failed deliveries. See [Webhooks](/guide/webhooks). |
| **Tracking** | Open and click defaults, one-click unsubscribe, the daily event budget, deployment state and the day's recorded and unrecorded events, and a read-only summary of each sending domain's own tracking address (managed on Sending domains). See [Open and click tracking](/guide/tracking). |
| **Suppressions** | Search, filter, add and remove entries. See [Suppressions](/guide/suppressions). |
| **Inbound** | Receiving domains, per-app limits, received messages and their webhook history. See [Inbound mail](/guide/inbound). |
| **SMTP** | The settings for sending over SMTP, and the app's SMTP credentials and From address policy (see [SMTP](#smtp)). Shown only when the server reports that SMTP submission is available. |

The app area stays mounted as you move between tabs. The console remembers the last app you used so that the shortcuts `/domains`, `/messages` and `/webhooks` go straight to it. The app picker appears only when none is remembered.

### Rename and delete an app

The pencil button next to the app's name opens **Rename app**. A new name takes effect at once; keys, domains, messages and webhooks are unaffected. App names don't have to be unique. The change is recorded in the audit log with the old and new name.

**Delete app** sits beside **Pause app** in the status panel on the API keys tab. The dialog explains what happens and asks you to type the app's name before it will delete. Deleting removes the app from the console and the API, revokes all of its API keys and SMTP credentials so they are refused from then on, and releases its receiving domains so another app can claim them. Mail that was accepted but not yet sent is not sent: held messages are cancelled, messages waiting to be prepared are ended as failed, and every recipient still queued, held or of unknown outcome is marked failed. Its webhook endpoints are switched off and webhook events still waiting are cancelled. A send already under way may still finish, and anything the operator was already holding for recovery is left as it is. Messages, events and the audit log are kept, not erased, and the app's name can be used again. There is no way to restore a deleted app from the console. Afterwards the console returns to the apps list.

### SMTP

::: tip When the tab is shown
The tab appears when the control plane has the SMTP intake configured (the edge bridge, signing and object storage), which it reports as the `smtp_submission` runtime feature. Mail only flows once an administrator has also turned submission on at the edge, which opens ports 465 and 587. The badges next to the ports show whether Thinpost last saw each one listening, so a tab that is visible with "Not checked yet" or "Not available right now" badges means the edge is not offering submission (yet). A direct link to `/apps/<id>/smtp` on a server without the feature shows a notice instead.
:::

The tab has three panels. The wording is meant for the developers and support staff who set up the sending software, not only for administrators.

- **How to connect.** The server name to enter in the sending software, and ports 465 (SSL/TLS) and 587 (STARTTLS), each with a copy field and a badge saying whether Thinpost last saw that port available. Port 465 is recommended; with 587 the software must require STARTTLS. If the app has no server name yet, the panel says why: the sending worker hasn't connected yet, the name can't be confirmed in DNS, or the app's chosen IP is no longer on the sending worker (choose another on the app's Keys page). The server name is the HELO name of the app's sending IP, or of the main IP when it has none, as the sending worker last reported it (see [Sending IPs](/guide/sending-ips)). Thinpost shows it only after public DNS confirms it both ways: the IP's reverse DNS names the server, and the server's name points at the IP. If it can't confirm that, the panel says the name can't be confirmed yet and asks you to check with your Thinpost administrator, rather than showing a name that might send mail to the wrong place. Reverse DNS is set by whoever runs the IP addresses, so a compromised sending worker cannot make the console show a name it controls.
- **Credentials.** A table of the app's credentials with status, created time, last used (at most a minute stale) and username. **Create credential** asks for a name, then shows the username and password once in a dialog. The password is held only in memory and is never written to browser storage or the URL, and closing the dialog without copying it asks first. **Revoke** asks for confirmation. An app can have 10 active credentials.
- **From address.** What happens to the From address of mail sent over SMTP: accept only addresses on the app's verified sender domains, move other addresses onto a chosen domain (the original address is kept in the name part, as in `scanner=office.example.net@example.com`, so senders stay distinguishable), or always send from one address. Examples show the effect, the form checks the address before saving, and a warning appears when mail from the chosen domain would not be DKIM-signed yet. A change applies to mail sent from then on.

Mail sent over SMTP uses the app's [tracking defaults](/guide/tracking#per-app-defaults-and-per-message-choices) and one-click unsubscribe setting too (see [Tracking and unsubscribe](/guide/smtp#tracking-and-unsubscribe)): a device that should not be tracked belongs in an app of its own with those switched off.

A message sent over SMTP shows an **SMTP** badge in the list and, on its detail page, the credential's name and, when the From policy replaced the sender's address, an **Original From** line.

### App limits

The **API keys** tab has three panels that show and set an app's submission limits. Each shows the current setting and when it was loaded, has **Reload limit** and **Change limit** buttons. A failed load or save says why, and a change with an unknown outcome is flagged until you reload.

- **Sending limit.** The submission rate: messages per minute and a burst size.
- **Daily recipient limit.** The number of recipients the app can send to each UTC day. The panel shows how many have been used today (UTC) and the day. Changing the limit does not reset today's count.
- **Pending-message limit.** The number of the app's messages that can be pending at once. The panel shows the pending count, which is a snapshot. When more messages are pending than Thinpost counts, it shows a lower bound ("or more") and says it is approximate.

A new app starts with **60 messages per minute (burst of 100)**, **10,000 recipients per day** and **1,000 pending messages**, so it can send right away. Apps that existed before these defaults keep the limits they had. A limit of **zero blocks all new messages** for the app. In the dialog, choose **Block all new messages** to set zero (it asks you to acknowledge), or **Allow, up to a limit**. For the daily recipient and pending-message limits, enter a whole number from 1 up to the maximum (1,000,000 recipients per day, 100,000 pending messages); the sending limit takes a rate from 1 to 60,000 messages per minute and a burst from 1 to 10,000. Changes take effect immediately and are audited. Messages already accepted are not affected. The admin API equivalents are `GET` and `PUT` on `/api/v1/apps/{appId}/submission-rate`, `/recipient-quota` and `/pending-quota`.

### Messages

- **List.** Accepted time, message ID, sender domain, first recipient (with a count of others), subject, an overall status (queued through delivered) and delivery counts. Messages accepted before the console recorded this metadata show "Not recorded". A message [submitted on hold](/guide/holding-messages) shows **On hold by app**, then **Cancelled** or **Hold expired** if it was never released.
- **Search.** By message ID, recipient, recipient domain, sender domain or subject. Terms go in a `POST` body and are kept only in memory, never in the URL, router state, document title, browser storage, logs or notifications. They are not restored after a reload. Pasting an exact 32-character ID opens that message.
- **Detail.** Sender domain, recipients, and per-delivery state with the retry count, earliest next attempt and latest outcome. HTML content appears on **Rendered**, **Text** and **HTML source** tabs. The rendered view is sanitized, shown in a `sandbox` iframe with its own policy, and blocks all remote content, with a count of what it blocked. Inline `cid:` images are resolved within size limits. Attachments download as `application/octet-stream`. A message submitted as a complete email over SMTP shows its parsed content, with its attachments inside the original: **Download original (.eml)** saves the message exactly as received, also as `application/octet-stream`. A **Headers** view shows every synthesized header in order. Everything is held in memory and discarded when you leave. A held message also shows its hold: when it expires, or when it was released, cancelled or expired. Operators cannot release or cancel it; only the app can.
- **Enhanced Protection.** The subject of a protected message is masked in lists and excluded from search. Revealing it, showing content, downloading an attachment or the original message and viewing headers each ask for confirmation and are recorded in the audit log. Unprotected messages show none of these dialogs. See [Concepts](/guide/concepts#enhanced-protection).

### Send a test message

**Send test message** on the Messages tab (and **Send test** on a verified domain's row) sends one real message as the app, to check that delivery works end to end. You choose a local part and one of the app's verified sending domains for From, an optional display name, one to five To recipients, a subject and a text body (both prefilled with neutral text), an optional HTML body, and optionally Enhanced Protection and the two options under [Test send options](#test-send-options). There is no Cc, Bcc, reply address, custom header, attachment, tracking choice or hold in a test send.

By default, a test send is a normal submission as far as Thinpost is concerned:

- It goes through the same acceptance path as the app API, so sender-domain verification, the recipient rules, the [suppression list](/guide/suppressions), the submission rate, the daily recipient quota and the pending-message limit all apply, and it counts against them. A new app has working default limits (see [App limits](#app-limits)), so test sends work at once. An app whose limit is zero refuses test sends until it is raised in its panel on the **API keys** tab.
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
- **Sending IPs:** the sending worker's addresses and their HELO names (read-only), which apps use each, whether the sending worker still has it, and a DNS check for each. An app's IP is chosen on its Keys page. See [Sending IPs](/guide/sending-ips).
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
