---
title: Quickstart
description: Run Thinpost on your own machine with Docker Compose, send a message through the Postmark-compatible API and read it in Mailpit.
---

# Quickstart

This walkthrough runs a complete Thinpost stack on your machine and sends one message through it. Nothing leaves your computer: outbound mail goes to [Mailpit](https://mailpit.axllent.org), a local inbox, instead of the internet.

::: warning For trying it out only
The quickstart stack uses a development identity provider with a one-button sign-in, a throwaway certificate authority, loopback-only database and object-store connections, and a local DNS zone. Each one is refused or unsupported in a real deployment. Use [Deploy](/deploy/) for anything beyond a trial.
:::

## What you need

- Docker with Compose v2 (Docker Desktop on macOS or Windows, or Docker Engine on Linux). Apple Silicon and x86-64 are both supported.
- A checkout of the repository: `git clone https://github.com/zyno-io/thinpost && cd thinpost`.
- `curl`, `jq` and `openssl`, and a browser.
- Ports `8443` (console and API) and `8025` (Mailpit) free on localhost. Both are published on `127.0.0.1` only.

## 1. Start the stack

From the repository root:

```sh
docker compose up -d --build --wait
```

The first start builds the images, which takes several minutes; later starts reuse them and take about two. `--wait` returns once every service is healthy. Compose starts, in dependency order:

1. a one-shot job that generates the development CA, certificates, DKIM key, key rings and the local DNS zone;
2. PostgreSQL and an S3-compatible object store;
3. a one-shot job that prepares the database (create roles, migrate, grant privileges), seeds one app and opens the admission gate;
4. the control plane and the edge worker;
5. a small DNS server for the `quickstart.example` zone, Mailpit, the development identity provider and an HTTPS front door.

The stack seeds one app, **Quickstart**, with the sender domain `quickstart.example` (DKIM selector `mail1`) and the allowed recipient `demo@quickstart.example`. It already has sending limits, so it can submit as soon as you have a key.

::: tip Prove it in one command
`examples/quickstart/smoke.sh` runs every step below for you (sign-in, key, domain verification, send) and checks that the delivered message carries a valid `DKIM-Signature` and shows as delivered. It prints `SMOKE PASSED` at the end.
:::

## 2. Trust the local certificate

The front door serves `https://localhost:8443` with a certificate from its own local CA. Copy that CA's root certificate out so `curl` and SDKs can verify the server:

```sh
docker compose cp caddy:/caddy-root/caddy-root.crt ./quickstart-ca.pem
```

Your browser will still warn once; accept the warning for this trial.

## 3. Sign in to the console

Open `https://localhost:8443` and use the development sign-in button. You land on the Applications screen, signed in as an administrator.

## 4. Verify the domain and issue a key

1. **Open the app.** Choose **Quickstart**.
2. **Request the sender domain.** Under **Sending domains**, request `quickstart.example`. The console shows a TXT record name and value that prove you own the domain.
3. **Publish the record** in the stack's DNS zone, replacing the two placeholders with the values the console shows:

   ```sh
   docker compose run --rm --no-deps --entrypoint sh pki-init /qs/zone-add.sh <name> <value>
   ```

   The name can be the full name the console shows or just the part before `.quickstart.example`.

4. **Verify.** Choose **Verify** in the console. The domain becomes active.
5. **Issue a key.** Under **API keys**, issue a key. Copy it now; it is shown once.

## 5. Send a message

Thinpost speaks several provider dialects. This example uses the Postmark-compatible route. Replace `<key>` with the key you copied:

```sh
curl --cacert quickstart-ca.pem https://localhost:8443/email \
  -H "X-Postmark-Server-Token: <key>" \
  -H "Content-Type: application/json" \
  -d '{
    "From": "hello@quickstart.example",
    "To": "demo@quickstart.example",
    "Subject": "Hello from Thinpost",
    "TextBody": "It works."
  }'
```

You get back a JSON response with a `MessageID`. That means the message is committed and queued, not that it has been delivered.

## 6. See it arrive

Open `http://localhost:8025`. Within a few seconds the message appears in Mailpit. Open it and look at the headers: you should see a `DKIM-Signature` with `d=quickstart.example` and `s=mail1`.

Back in the console, open the app's **Messages** tab. The message shows as delivered. The edge sent it to the local Mailpit server, which the quickstart allows as the only destination; nothing is routed off your machine.

### Send over SMTP

The quickstart edge also accepts authenticated SMTP submission on ports 465 and 587 of its sending address. Those ports exist only on the stack's internal network, so `smoke.sh` sends from a helper container: it creates an SMTP credential, sends one message on each port with `curl`, checks that `Bcc` and the client's `Received` header were removed and that the message carries Thinpost's `Message-ID` and a valid DKIM signature, and revokes the credential. See `examples/quickstart/README.md` for the command.

## 7. Try a provider SDK

The same key works with any supported SDK pointed at `https://localhost:8443`. For example, with the Postmark Node SDK:

```js
import { ServerClient } from "postmark";

const client = new ServerClient("<key>", {
  requestHost: "localhost:8443",
  useHttps: true,
});

await client.sendEmail({
  From: "hello@quickstart.example",
  To: "demo@quickstart.example",
  Subject: "From the SDK",
  TextBody: "Sent with the Postmark SDK.",
});
```

Run Node with `NODE_EXTRA_CA_CERTS=quickstart-ca.pem` so it trusts the local certificate. The options for the other SDKs are in [Sending with SDKs](/guide/sending-with-sdks).

## Clean up

Remove the containers and volumes, including generated keys and the database:

```sh
docker compose down -v
```

If something goes wrong, `docker compose ps -a` and `docker compose logs <service>` show what failed. The control plane and edge restart on failure while their dependencies come up.

## What this stack skips

| In the quickstart | In a real deployment |
| --- | --- |
| Development identity provider | Your own OpenID Connect provider ([Operator authentication](/deploy/operator-auth)) |
| Local CA, one machine | A private CA for the control channel and publicly trusted certificates for SMTP ([Control PKI](/deploy/control-pki)) |
| Database and object store on loopback without TLS | TLS to your PostgreSQL and object store ([Database](/deploy/database), [Object storage](/deploy/object-storage)) |
| Fake DNS zone, Mailpit as the only destination | Real DNS, public sending IPs with reverse DNS ([Sending IPs](/guide/sending-ips)) |
| Single process of each kind | Several control replicas and a journalled edge ([Deploy](/deploy/)) |

## Next

- [Sender domains](/guide/domains): what verification does and the DNS you still need.
- [Webhooks](/guide/webhooks): get delivery events in your own app.
- [Topology and prerequisites](/deploy/): run it for real.
