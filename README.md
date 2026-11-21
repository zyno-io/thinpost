# Thinpost

Thinpost is a self-hosted email delivery platform for application email. Send through the Postmark, Resend, SendGrid or Mailgun SDK you already use, and keep delivery, API keys and message content on infrastructure you run.

It is two Go processes: a **control plane** that owns apps, keys, queues, DKIM signing, storage, webhooks and the operator console, and a small **edge worker** that speaks SMTP and has no database or object-storage credentials. State lives in PostgreSQL and an S3-compatible object store.

## Features

- Provider-compatible submission APIs and webhook formats (Postmark, Resend, Mailgun; SendGrid submission)
- Apps as security principals, each with its own API keys, sender domains, quotas and webhooks
- DKIM signing, sending IPs (set on the edge) with per-app choice, and Enhanced Protection (required TLS and audited content reads)
- Signed, retried and replayable webhooks
- Opt-in open and click tracking and one-click unsubscribe
- Suppression lists, spam-complaint feedback loops and automatic hard-bounce handling
- Inbound mail delivered to your app as a webhook
- An operator console and admin API, with generic OpenID Connect sign-in

## Status

Thinpost is pre-1.0. It runs in production for its maintainers. APIs and configuration may change between minor versions. See the [roadmap](ROADMAP.md) for what is still being built.

## Quickstart

Run the whole stack on your machine with Docker Compose and send a first message:

```sh
docker compose up
```

The [quickstart guide](https://zyno-io.github.io/thinpost/guide/quickstart) walks through signing in to the console, creating an app and sending mail to a local inbox.

## Documentation

Full documentation is at **<https://zyno-io.github.io/thinpost/>**: concepts, architecture, deployment on Kubernetes, configuration reference, provider compatibility, operations and security.

The documentation source is in [`docs/`](docs/). To preview it locally, run `npm ci` and `npm run docs:dev` in that directory.

## Contributing

See the [contributing guide](https://zyno-io.github.io/thinpost/contributing/) for the toolchain and test commands. Run the Go tests with `go test ./...` and `go vet ./...`.

## License

[MIT](LICENSE).
