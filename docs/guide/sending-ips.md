---
title: Sending IPs
description: Name outbound addresses, assign apps to them, set up PTR, HELO and SPF, and roll a new address out safely.
---

# Sending IPs

The edge worker can send from several public addresses so that different apps build separate reputations. A noisy or low-quality app then cannot damage the standing of another. Each address is a **sending IP** in Thinpost.

## What a sending IP is

Three places each own one part of a sending IP:

| Owner | Decides |
| --- | --- |
| **Deployment (Helm values)** | Which addresses exist on the edge: the primary `sourceAddress` and any `additionalSources`. Each entry needs a `hostname` (the name it greets with, its PTR and HELO) and a certificate Secret for that name. |
| **Database (console)** | What each address is called (a lowercase slug used in logs instead of the address), its HELO name, its bounce domain and which apps are assigned to it. |
| **Edge (runtime)** | What it can actually bind. At session start it reports its addresses and bounce domains. A delivery for an address the edge did not report stays queued and is never sent from another address. |

An app with no assignment uses the **primary** address. The primary needs no database row, but may have one that only gives it a name.

The HELO name is also the **SMTP submission host**: the server name the console's SMTP tab gives to the software that sends an app's mail over SMTP, because that name already has a valid certificate. An app uses the HELO name of its assigned sending IP, or of the row that names the primary address. If the primary has no row, the SMTP tab asks for one, so name it here. See [Operator console](/guide/console#smtp). Clients connect to this name on ports 465 and 587 of the sending IP, which reuse the same certificate as port 25.

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
- The **bounce domain** is the HELO name or a domain it belongs to. The console requires this and pre-fills it. The edge accepts every one of its own names as a bounce domain automatically. If you choose a different bounce domain, publish an MX record pointing at this address's name and an SPF record listing the address.
- Several addresses can share one bounce domain. Its SPF record then lists all of them as `ip4:` entries. Add to the existing record; publish only one SPF record per name.

A customer's own SPF record does not need to mention Thinpost.

### The console's DNS check

The console has a **Check DNS** action for each sending IP. It reads public DNS from the control process, stores nothing and is advisory. It reports whether:

- the PTR names include the HELO name,
- the HELO name resolves back to the address,
- the HELO name has an SPF record, and
- the bounce domain's SPF record covers the address through an `ip4:` or `a` entry.

It does not follow `include:` or `redirect=`, so an address listed only through an include shows as not matching.

## Add a sending IP

Do these in order. Each step is safe on its own, and none changes mail flow until the last.

1. **Obtain the address** and record it in your infrastructure as code.
2. **Allow it in the egress policy.** The control plane checks every source address the edge reports against the infrastructure ranges you configure, and refuses the edge's session when one is outside them. Add the new range to the control plane before you roll the edge. The edge applies no destination policy and needs no range.
3. **Route it.** Configure your network so the address reaches the edge interface and TCP 25 passes in both directions. Every address listens, so inbound 25 must be open. A new network also needs its gateway. See [Direct public networking](/deploy/direct-public-networking).
4. **Issue a certificate** for the address's HELO name, in its own Secret in the edge namespace. Create it **before** you change the Helm values: a missing or wrong certificate stops the edge at its next start, and with it all mail.
5. **Publish DNS** as above and set the PTR.
6. **Add the address** to `additionalSources` with its `hostname` and `certificateSecret`, then upgrade the edge release. The edge restarts, so sending pauses briefly and bounce senders retry. See [Helm: Edge](/deploy/helm-edge).
7. **In the console,** choose **Add IP** on the Operations screen, run **Check DNS**, and then assign one low-volume app.
8. **Verify from outside.** Check the greeting, the STARTTLS certificate and an inbound test message on the new address.
9. **Warm it up.** Watch the first app's deliveries, then move more apps one at a time.

A retired bounce domain stays in `additionalBounceDomains` so the edge keeps accepting it. Keep its MX record for at least five days plus your maximum retry age so late bounces still arrive.

### Adding the first additional IP

This order keeps mail flowing on a running install:

1. In the console, name the primary (its HELO name and bounce domain) and run the DNS check.
2. Add the addresses to `additionalSources`, upgrade the edge, then add the IP in the console and assign apps.

Never roll the control plane back with Helm; roll forward with a fix. See [Upgrades](/deploy/upgrades).

## Moving the edge between nodes

The edge is built to float between nodes: it attaches to its networks through Multus and Whereabouts, the journal is a single-writer volume, and the Deployment keeps one replica with the `Recreate` strategy so one pod at a time holds the journal and addresses. Your routers must not tie an address to a single node's MAC address; see [Direct public networking](/deploy/direct-public-networking).

## Next

- [Webhooks](/guide/webhooks)
- [Helm: Edge](/deploy/helm-edge)
