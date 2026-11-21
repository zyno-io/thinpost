---
title: Sending IPs
description: See the edge's outbound addresses, choose which one each app sends from, set up PTR, HELO, MX and SPF, and roll a new address out safely.
---

# Sending IPs

The edge worker can send from several public addresses so that different apps build separate reputations. A noisy or low-quality app then cannot damage the standing of another. Each address is a **sending IP** in Thinpost. The edge's configuration defines the addresses and their names. The console lists them, and the only thing you choose there is which IP each app sends from.

## What a sending IP is

Three places each own one part of a sending IP:

| Owner | Decides |
| --- | --- |
| **Deployment (Helm values)** | Which addresses exist on the edge: the `addresses` list, whose first entry is the main IP. Each entry needs a `hostname` (the name it greets with, its PTR and HELO) and a certificate Secret for that name. |
| **Database (console)** | A read-only copy of what the edge last reported: each address and its HELO name, so the console can list them. Rows are created from the edge's report, never by hand. The only thing the console stores is which app uses which IP. |
| **Edge (runtime)** | What it can actually bind. At session start it reports its addresses and the name each one greets with. Control sends every delivery with that name as the HELO name and the return-path domain. An edge that reports no names is sent no mail. A delivery for an address the edge did not report stays queued and is never sent from another address. |

There is nothing to name, edit or delete in the console. A new address appears in the list when the edge reports it, a changed HELO name is updated from the next report, and an address the edge stops reporting disappears unless an app still uses it. To add or change an IP, change the edge's configuration (see below) or ask your Thinpost administrator.

An app with no choice uses the **main IP**, the first address the edge reports. Every reported address has a row, including the main IP, so every delivery is sent from an explicitly named address. Changing the order of the edge's addresses changes the main IP, and so where every app with no choice sends from.

The HELO name is also the **SMTP submission host**: the server name the console's SMTP tab gives to the software that sends an app's mail over SMTP, because that name already has a valid certificate. An app uses the HELO name of its chosen sending IP, or of the main IP. Until the edge has connected, while an app's IP is not on the edge, or while public DNS doesn't confirm the name both ways (the IP's reverse DNS names it and it points at the IP), the SMTP tab says there is no server name yet. See [Operator console](/guide/console#smtp). Clients connect to this name on ports 465 and 587 of the sending IP, which reuse the same certificate as port 25.

### Choosing an IP for an app

You can choose an app's IP when you create the app, or later on the app's **Keys** page. Only an IP the edge is reporting right now can be chosen (otherwise the change is refused), and going back to the main IP is always allowed. The change applies to mail sent from then on, including messages already waiting.

### When an IP disappears from the edge

If the edge's configuration drops an address that apps still use, the row stays, marked as not on the sending worker, and those apps' mail waits. It is never sent from another address. The console shows how many apps are waiting. Move them to another IP and the mail flows again. Move apps off an IP before you remove it from the edge.

::: warning Documentation addresses are refused
Thinpost rejects documentation, private, loopback, link-local and other special-purpose ranges as sending sources, so the examples below use `<your public IPv4>` rather than a sample address. The address must be a routable public IPv4 address that you control.
:::

Every address the edge binds also **listens** for SMTP on port 25. It greets with its own name and certificate. Bounces and inbound mail may arrive on any of them. The addresses share one receiver, so connection and memory limits hold for the whole process however many addresses you add.

Sending is serial per edge. A slow receiver on one address delays mail on the others. Per-address concurrency and rate caps do not exist, and you warm an address by hand (see below).

## DNS for a sending IP

None of these records are published by Thinpost. Set them at the address owner (reverse DNS) and in your zone.

| What | Where | Value |
| --- | --- | --- |
| Reverse DNS (PTR) | the address owner or hosting provider | `smtp1.example.com` |
| A record for the HELO name | your zone | `smtp1.example.com. A <your public IPv4>` |
| SPF for the HELO name | your zone | `smtp1.example.com. TXT "v=spf1 a -all"` |
| MX for the HELO name | your zone | `smtp1.example.com. MX 10 smtp1.example.com.` |

- The **HELO name** is the PTR of the address. The edge greets with it and control sends it as the outbound HELO, so the banner, the HELO and the PTR agree.
- `v=spf1 a -all` uses the name's own A record, so it follows the address. Do not use `ptr`, which RFC 7208 deprecates.
- The **MX** record makes bounces and inbound mail for the address arrive at the address itself.
- The **bounce domain** is the HELO name. Every message leaves with a return path like `b+<token>@<HELO name>`, using the name the edge reported for the address the message is sent from. The edge accepts bounces at every one of its own names, so the MX record above is all bounces need. There is no separate bounce domain to set. An app can also use a bounce domain under its own sending domain, which points at this name: see [Your own bounce domain](/guide/domains#your-own-bounce-domain).
- The edge accepts a bounce addressed to `b+<token>@<any name>` as long as that name's DNS points at the edge, so bounces for mail sent under an old HELO name keep arriving after you rename it, until you remove that name's DNS. Keep the old name's A and MX records for your longest retry window, then drop them. Rename in this order: certificate first, then DNS and PTR, then the Helm values.

A customer's own SPF record does not need to mention Thinpost.

### The console's DNS check

The console has a **Check DNS** action for each sending IP. It reads public DNS from the control process, stores nothing and is advisory. Every check is about the HELO name, which is also the bounce domain. It reports whether:

- **reverse:** the PTR names include the HELO name,
- **forward:** the HELO name resolves back to the address,
- **mx:** the HELO name's MX records lead to the address, so bounces reach it (a name with no MX record counts as its own mail server), and
- **spf:** the HELO name's SPF record covers the address through an `ip4:` or `a` entry.

When a check fails, the console lists what to ask whoever manages your DNS to change, for that IP. It does not follow `include:` or `redirect=`, so an address listed only through an include shows as not matching.

## Add a sending IP

An address is added in the edge's configuration, not in the console.

Do these in order. Each step is safe on its own, and none changes mail flow until the last.

1. **Obtain the address** and record it in your infrastructure as code.
2. **Allow it in the egress policy.** The control plane checks every source address the edge reports against the infrastructure ranges you configure, and refuses the edge's session when one is outside them. Add the new range to the control plane before you roll the edge. The edge applies no destination policy and needs no range.
3. **Route it.** Configure your network so the address reaches the edge interface and TCP 25 passes in both directions. Every address listens, so inbound 25 must be open. A new network also needs its gateway. See [Direct public networking](/deploy/direct-public-networking).
4. **Issue a certificate** for the address's HELO name, in its own Secret in the edge namespace. Create it **before** you change the Helm values: a missing or wrong certificate stops the edge at its next start, and with it all mail.
5. **Publish DNS** as above and set the PTR.
6. **Add the address** to the end of `addresses` with its `hostname` and `certificateSecret`, then upgrade the edge release. The edge restarts, so sending pauses briefly and bounce senders retry. See [Helm: Edge](/deploy/helm-edge).
7. **In the console,** open **Operations → Sending IPs**. The new address appears once the restarted edge connects. Run **Check DNS**, and then choose it for one low-volume app.
8. **Verify from outside.** Check the greeting, the STARTTLS certificate and an inbound test message on the new address.
9. **Warm it up.** Watch the first app's deliveries, then move more apps one at a time.

### Adding the first additional IP

This order keeps mail flowing on a running install:

1. In the console, run the DNS check on the main IP.
2. Add the addresses to `addresses`, after the main IP, and upgrade the edge. When it reconnects, the new IPs appear in the console, and you can choose them for apps.

Never roll the control plane back with Helm; roll forward with a fix. See [Upgrades](/deploy/upgrades).

## Moving the edge between nodes

The edge is built to float between nodes: it attaches to its networks through Multus and Whereabouts, the journal is a single-writer volume, and the Deployment keeps one replica with the `Recreate` strategy so one pod at a time holds the journal and addresses. Your routers must not tie an address to a single node's MAC address; see [Direct public networking](/deploy/direct-public-networking).

## Next

- [Webhooks](/guide/webhooks)
- [Helm: Edge](/deploy/helm-edge)
