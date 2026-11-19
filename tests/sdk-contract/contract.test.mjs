import assert from 'node:assert/strict';
import { createHmac } from 'node:crypto';
import { spawn } from 'node:child_process';
import net from 'node:net';
import { once } from 'node:events';
import { createInterface } from 'node:readline';
import { after, before, test } from 'node:test';

let fixture;
let base;
let originalConnect;
let FormData;
let Mailgun;
let postmark;
let Resend;
let sendgrid;

before(async () => {
  fixture = spawn(process.env.THINPOST_SDK_FIXTURE_BINARY ?? './fixture-server', [], {
    stdio: ['ignore', 'pipe', 'inherit'],
  });
  const lines = createInterface({ input: fixture.stdout });
  const [line] = await Promise.race([
    once(lines, 'line'),
    once(fixture, 'exit').then(() => { throw new Error('fixture exited before starting'); }),
  ]);
  base = new URL(line);
  assert.equal(base.protocol, 'http:');
  assert.equal(base.hostname, '127.0.0.1');
  const port = Number(base.port);
  assert.ok(port > 0);
  process.env.NO_PROXY = '127.0.0.1,localhost';
  process.env.no_proxy = '127.0.0.1,localhost';
  // Catch ordinary SDK connections outside the fixture, including accidental
  // base-URL regressions. This process-level guard is not an OS sandbox.
  originalConnect = net.Socket.prototype.connect;
  net.Socket.prototype.connect = function (...args) {
    const first = Array.isArray(args[0]) ? args[0][0] : args[0];
    const options = typeof first === 'object' ? first : { port: first, host: args[1] };
    if (options.host !== '127.0.0.1' || Number(options.port) !== port) {
      throw new Error('SDK attempted a non-fixture network connection');
    }
    return originalConnect.apply(this, args);
  };
  ({ default: FormData } = await import('form-data'));
  ({ default: Mailgun } = await import('mailgun.js'));
  ({ default: postmark } = await import('postmark'));
  ({ Resend } = await import('resend'));
  ({ default: sendgrid } = await import('@sendgrid/mail'));
});

after(async () => {
  if (originalConnect) net.Socket.prototype.connect = originalConnect;
  if (fixture?.pid && fixture.exitCode === null && fixture.signalCode === null) {
    const exited = once(fixture, 'exit');
    fixture.kill('SIGTERM');
    await exited;
  }
});

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

// webhook renders the webhook the fixture's recorded message would produce,
// using the same format package as production.
async function webhook(record, format, event) {
  const response = await fetch(`${base.origin}/fixture/webhook?${new URLSearchParams({ record, format, case: event })}`);
  assert.equal(response.status, 200);
  return response.json();
}

async function records() {
  const response = await fetch(new URL('/fixture/records', base));
  assert.equal(response.status, 200);
  return response.json();
}

function assertMessage(record, adapter, subject) {
  assert.ok(record, `missing fixture record for ${subject}`);
  assert.equal(record.adapter, adapter);
  assert.equal(record.message.Subject, subject);
  assert.match(record.message.From, /sender@example\.com/);
  assert.equal(record.message.To.length, 2);
  assert.match(record.message.To[0], /recipient@example\.net/);
  assert.match(record.message.To[1], /recipient2@example\.net/);
  assert.equal(record.message.Text, 'fixture body');
  assert.match(record.message.ReplyTo[0], /reply@example\.com/);
  assert.equal(record.message.Cc?.length, 1);
  assert.match(record.message.Cc[0], /copy@example\.com/);
  assert.equal(record.message.Bcc?.length, 1);
  assert.match(record.message.Bcc[0], /blind@example\.com/);
}

function bySubject(all, subject) {
  const matching = all.filter(item => item.message.Subject === subject);
  assert.equal(matching.length, 1);
  return matching[0];
}

function assertAttachment(record) {
  assert.equal(record.message.Attachments.length, 1);
  assert.equal(record.message.Attachments[0].filename, 'fixture.txt');
  assert.equal(Buffer.from(record.message.Attachments[0].content, 'base64').toString(), 'fixture attachment');
}

test('Postmark SDK sends single and batch messages', async () => {
  const client = new postmark.ServerClient('fixture-key', {
    useHttps: false,
    requestHost: base.host,
    fetch: (input, init) => {
      const headers = new Headers(init.headers);
      headers.set('X-Thinpost-Enhanced-Protection', 'true');
      return fetch(input, { ...init, headers });
    },
  });
  const message = { From: 'sender@example.com', To: 'recipient@example.net, recipient2@example.net', Subject: 'postmark single', TextBody: 'fixture body', HtmlBody: '<a href="https://example.net">fixture</a>', ReplyTo: 'reply@example.com', Cc: 'copy@example.com', Bcc: 'blind@example.com', TrackOpens: true, TrackLinks: 'HtmlOnly', Attachments: [{ Name: 'fixture.txt', Content: Buffer.from('fixture attachment').toString('base64'), ContentType: 'text/plain' }] };
  const result = await client.sendEmail(message);
  assert.equal(result.ErrorCode, 0);
  assert.match(result.MessageID, UUID);
  const batch = await client.sendEmailBatch([{ ...message, Subject: 'postmark batch one' }, { ...message, Subject: 'postmark batch two' }]);
  assert.equal(batch.length, 2);
  assert.ok(batch.every(item => item.ErrorCode === 0 && UUID.test(item.MessageID)));
  const all = await records();
  const selected = ['postmark single', 'postmark batch one', 'postmark batch two'].map(subject => bySubject(all, subject));
  for (const item of selected) assertMessage(item, 'postmark', item.message.Subject);
  for (const item of selected) assertAttachment(item);
  assert.ok(selected.every(item => item.message.EnhancedProtection));
  assert.ok(selected.every(item => item.message.Tracking?.opens === true && item.message.Tracking?.clicks === true));
});

test('Postmark SDK reads the suppression dump with and without a filter', async () => {
  // getSuppressions sends count and offset and camelCase filter names.
  const client = new postmark.ServerClient('fixture-key', { useHttps: false, requestHost: base.host });
  const all = await client.getSuppressions('outbound');
  assert.deepEqual(all.Suppressions.map(item => item.EmailAddress).sort(), ['bounced@example.net', 'manual@example.net']);
  const bounced = await client.getSuppressions('outbound', { suppressionReason: 'HardBounce' });
  assert.equal(bounced.Suppressions.length, 1);
  assert.equal(bounced.Suppressions[0].EmailAddress, 'bounced@example.net');
  assert.equal(bounced.Suppressions[0].SuppressionReason, 'HardBounce');
  assert.equal(bounced.Suppressions[0].Origin, 'Recipient');
  const customer = await client.getSuppressions('outbound', { origin: 'Customer', fromDate: '2026-01-01', toDate: '2026-12-31' });
  assert.deepEqual(customer.Suppressions.map(item => item.EmailAddress), ['manual@example.net']);
});

test('Resend SDK sends single and batch request shapes', async () => {
  const client = new Resend('fixture-key', { baseUrl: base.origin });
  const message = { from: 'sender@example.com', to: ['recipient@example.net', 'recipient2@example.net'], subject: 'resend single', text: 'fixture body', html: '<a href="https://example.net">fixture</a>', replyTo: 'reply@example.com', cc: 'copy@example.com', bcc: 'blind@example.com', attachments: [{ filename: 'fixture.txt', content: Buffer.from('fixture attachment') }] };
  const trackingHeaders = { 'X-Thinpost-Enhanced-Protection': 'true', 'X-Thinpost-Track-Opens': 'true', 'X-Thinpost-Track-Clicks': 'true' };
  const single = await client.emails.send(message, { idempotencyKey: 'single-key', headers: trackingHeaders });
  assert.equal(single.error, null);
  assert.match(single.data.id, UUID);
  const batch = await client.batch.send([{ ...message, subject: 'resend batch one' }, { ...message, subject: 'resend batch two' }], { idempotencyKey: 'batch-key', headers: trackingHeaders });
  assert.equal(batch.error, null);
  assert.equal(batch.data.data.length, 2);
  const all = await records();
  const selected = ['resend single', 'resend batch one', 'resend batch two'].map(subject => bySubject(all, subject));
  for (const item of selected) assertMessage(item, 'resend', item.message.Subject);
  for (const item of selected) assertAttachment(item);
  assert.equal(selected[0].idempotencyKey, 'single-key');
  assert.equal(selected[1].idempotencyKey, 'batch-key');
  assert.equal(selected[2].idempotencyKey, 'batch-key');
  assert.ok(selected[1].batch && selected[2].batch);
  assert.ok(selected.every(item => item.message.EnhancedProtection));
  assert.ok(selected.every(item => item.message.Tracking?.opens === true && item.message.Tracking?.clicks === true));
  assert.deepEqual(batch.data.data.map(item => item.id.replaceAll('-', '')), [selected[1].id, selected[2].id]);
});

test('invalid Resend attachment content fails without submitting a message', async () => {
  const client = new Resend('fixture-key', { baseUrl: base.origin });
  const beforeCount = (await records()).length;
  const result = await client.emails.send({ from: 'sender@example.com', to: 'recipient@example.net', subject: 'unsupported', text: 'fixture body', attachments: [{ filename: 'fixture.txt', content: 'synthetic' }] });
  assert.equal(result.data, null);
  assert.equal(result.error.statusCode, 400);
  assert.equal((await records()).length, beforeCount);
});

test('fixture rejects non-loopback connections', () => {
  assert.throws(() => net.connect({ host: '203.0.113.1', port: 443 }), /non-fixture network connection/);
});

test('SDKs submit on hold with X-Thinpost-Hold and see the confirmation header', async () => {
  // Postmark: the SDK's fetch hook adds the request header and exposes the response header.
  let postmarkHold = null;
  const postmarkClient = new postmark.ServerClient('fixture-key', {
    useHttps: false,
    requestHost: base.host,
    fetch: async (input, init) => {
      const headers = new Headers(init.headers);
      headers.set('X-Thinpost-Hold', 'true');
      const response = await fetch(input, { ...init, headers });
      postmarkHold = response.headers.get('X-Thinpost-Hold');
      return response;
    },
  });
  const single = await postmarkClient.sendEmail({ From: 'sender@example.com', To: 'recipient@example.net', Subject: 'hold postmark', TextBody: 'fixture body' });
  assert.equal(single.ErrorCode, 0);
  assert.match(single.MessageID, UUID);
  assert.equal(postmarkHold, 'held');
  postmarkHold = null;
  const batch = await postmarkClient.sendEmailBatch([{ From: 'sender@example.com', To: 'recipient@example.net', Subject: 'hold postmark batch', TextBody: 'fixture body' }]);
  assert.ok(batch.every(item => item.ErrorCode === 0));
  assert.equal(postmarkHold, 'held');
  // Resend: per-request headers.
  const resendClient = new Resend('fixture-key', { baseUrl: base.origin });
  const message = { from: 'sender@example.com', to: ['recipient@example.net'], subject: 'hold resend', text: 'fixture body' };
  const sent = await resendClient.emails.send(message, { headers: { 'X-Thinpost-Hold': 'true' } });
  assert.equal(sent.error, null);
  assert.match(sent.data.id, UUID);
  const sentBatch = await resendClient.batch.send([{ ...message, subject: 'hold resend batch' }], { headers: { 'X-Thinpost-Hold': 'true' } });
  assert.equal(sentBatch.error, null);
  // An invalid value is a 400 and submits nothing.
  const beforeInvalid = (await records()).length;
  const invalid = await resendClient.emails.send({ ...message, subject: 'hold invalid' }, { headers: { 'X-Thinpost-Hold': 'yes' } });
  assert.equal(invalid.data, null);
  assert.equal(invalid.error.statusCode, 400);
  assert.equal((await records()).length, beforeInvalid);
  // SendGrid: a client default header, cleared afterwards so other tests are unaffected.
  sendgrid.setApiKey('fixture-key');
  sendgrid.client.setDefaultRequest('baseUrl', base.origin);
  sendgrid.client.setDefaultHeader('X-Thinpost-Hold', 'true');
  let gridResponse;
  try {
    [gridResponse] = await sendgrid.send({ from: 'sender@example.com', to: 'recipient@example.net', subject: 'hold sendgrid', text: 'fixture body' });
  } finally {
    sendgrid.client.setDefaultHeader('X-Thinpost-Hold', 'false');
  }
  assert.equal(gridResponse.statusCode, 202);
  assert.equal(gridResponse.headers['x-thinpost-hold'], 'held');
  const all = await records();
  for (const subject of ['hold postmark', 'hold postmark batch', 'hold resend', 'hold resend batch', 'hold sendgrid']) {
    assert.equal(bySubject(all, subject).hold, true, subject);
  }
  // Without the header nothing is held.
  assert.ok(all.filter(item => !item.message.Subject.startsWith('hold ')).every(item => item.hold === undefined));
});

test('SendGrid SDK sends a supported message', async () => {
  sendgrid.setApiKey('fixture-key');
  sendgrid.client.setDefaultRequest('baseUrl', base.origin);
  sendgrid.client.setDefaultHeader('X-Thinpost-Enhanced-Protection', 'true');
  const [response] = await sendgrid.send({ from: 'sender@example.com', to: ['recipient@example.net', 'recipient2@example.net'], subject: 'sendgrid single', text: 'fixture body', html: '<a href="https://example.net">fixture</a>', replyTo: 'reply@example.com', cc: 'copy@example.com', bcc: 'blind@example.com', trackingSettings: { openTracking: { enable: true }, clickTracking: { enable: true, enableText: false } }, attachments: [{ filename: 'fixture.txt', content: Buffer.from('fixture attachment').toString('base64'), type: 'text/plain' }] });
  assert.equal(response.statusCode, 202);
  const all = await records();
  const message = bySubject(all, 'sendgrid single');
  assertMessage(message, 'sendgrid', 'sendgrid single');
  assertAttachment(message);
  assert.equal(message.message.EnhancedProtection, true);
  assert.deepEqual(message.message.Tracking, { opens: true, clicks: true });
});

test('SendGrid SDK sends multiple personalizations atomically', async () => {
  const [response] = await sendgrid.send({
    from: 'sender@example.com',
    subject: 'grid global',
    text: 'fixture body',
    personalizations: [
      { to: 'one@example.net', subject: 'grid first' },
      { to: 'two@example.net', from: 'other@example.com', subject: 'grid second' },
    ],
  });
  assert.equal(response.statusCode, 202);
  const all = await records();
  const first = bySubject(all, 'grid first');
  const second = bySubject(all, 'grid second');
  assert.equal(first.adapter, 'sendgrid');
  assert.equal(second.adapter, 'sendgrid');
  assert.ok(first.batch && second.batch && first.message.EnhancedProtection && second.message.EnhancedProtection);
  assert.deepEqual(first.message.To, ['<one@example.net>']);
  assert.deepEqual(second.message.To, ['<two@example.net>']);
  assert.match(second.message.From, /other@example\.com/);
  const [multiple] = await sendgrid.sendMultiple({ from: 'sender@example.com', to: ['three@example.net', 'four@example.net'], subject: 'grid sendMultiple', text: 'fixture body' });
  assert.equal(multiple.statusCode, 202);
  const split = (await records()).filter(item => item.message.Subject === 'grid sendMultiple');
  assert.equal(split.length, 2);
  assert.deepEqual(split.map(item => item.message.To[0]), ['<three@example.net>', '<four@example.net>']);
  assert.ok(split.every(item => item.batch && item.adapter === 'sendgrid'));
});

test('Mailgun SDK sends a supported message and native TLS policy', async () => {
  const client = new Mailgun(FormData).client({ username: 'api', key: 'fixture-key', url: base.origin });
  const result = await client.messages.create('example.com', { from: 'sender@example.com', to: ['recipient@example.net', 'recipient2@example.net'], subject: 'mailgun single', text: 'fixture body', html: '<a href="https://example.net">fixture</a>', 'h:Reply-To': 'reply@example.com', cc: 'copy@example.com', bcc: 'blind@example.com', 'o:require-tls': 'yes', 'o:tracking-opens': 'yes', 'o:tracking-clicks': 'htmlonly', attachment: { data: Buffer.from('fixture attachment'), filename: 'fixture.txt', contentType: 'text/plain' } });
  assert.match(result.id, /^<[0-9a-f]{32}@example\.com>$/);
  const all = await records();
  const message = bySubject(all, 'mailgun single');
  assertMessage(message, 'mailgun', 'mailgun single');
  assertAttachment(message);
  assert.equal(message.sendingDomain, 'example.com');
  assert.equal(message.message.EnhancedProtection, true);
  assert.deepEqual(message.message.Tracking, { opens: true, clicks: true });
});

test('SDKs send Reply-To as a custom header', async () => {
  const before = (await records()).length;
  const postmarkClient = new postmark.ServerClient('fixture-key', { useHttps: false, requestHost: base.host });
  await postmarkClient.sendEmail({ From: 'sender@example.com', To: 'recipient@example.net', Subject: 'header reply postmark', TextBody: 'fixture body', Headers: [{ Name: 'Reply-To', Value: 'reply@example.com' }] });
  const resendClient = new Resend('fixture-key', { baseUrl: base.origin });
  const sent = await resendClient.emails.send({ from: 'sender@example.com', to: ['recipient@example.net'], subject: 'header reply resend', text: 'fixture body', headers: { 'Reply-To': 'reply@example.com' } });
  assert.equal(sent.error, null);
  sendgrid.setApiKey('fixture-key');
  sendgrid.client.setDefaultRequest('baseUrl', base.origin);
  await sendgrid.send({ from: 'sender@example.com', to: 'recipient@example.net', subject: 'header reply sendgrid', text: 'fixture body', headers: { 'Reply-To': 'reply@example.com' } });
  const mailgunClient = new Mailgun(FormData).client({ username: 'api', key: 'fixture-key', url: base.origin });
  await mailgunClient.messages.create('example.com', { from: 'sender@example.com', to: ['recipient@example.net'], subject: 'header reply mailgun', text: 'fixture body', 'h:reply-to': 'reply@example.com' });
  const all = await records();
  assert.equal(all.length, before + 4);
  for (const name of ['postmark', 'resend', 'sendgrid', 'mailgun']) {
    const item = bySubject(all, `header reply ${name}`);
    assert.match(item.message.ReplyTo[0], /reply@example\.com/);
  }
});

test('other SDKs report unsupported or unsafe fields without submitting', async () => {
  const beforeCount = (await records()).length;
  const postmarkClient = new postmark.ServerClient('fixture-key', { useHttps: false, requestHost: base.host });
  await assert.rejects(postmarkClient.sendEmail({ From: 'sender@example.com', To: 'recipient@example.net', Headers: [{ Name: 'Bcc', Value: 'other@example.net' }], Subject: 'unsupported', TextBody: 'fixture body' }), error => error.statusCode === 400);
  await assert.rejects(postmarkClient.sendEmail({ From: 'sender@example.com', To: 'recipient@example.net', Subject: 'unsupported text tracking', TextBody: 'fixture body', HtmlBody: '<b>fixture</b>', TrackLinks: 'HtmlAndText' }), error => error.statusCode === 400);
  await assert.rejects(sendgrid.send({ from: 'sender@example.com', to: 'recipient@example.net', subject: 'unsupported', text: 'fixture body', categories: ['unsupported'] }), error => error.code === 400);
  await assert.rejects(sendgrid.send({ from: 'sender@example.com', to: 'recipient@example.net', subject: 'unsupported text tracking', html: '<b>fixture</b>', trackingSettings: { clickTracking: { enable: true, enableText: true } } }), error => error.code === 400);
  const mailgunClient = new Mailgun(FormData).client({ username: 'api', key: 'fixture-key', url: base.origin });
  await assert.rejects(mailgunClient.messages.create('example.com', { from: 'sender@example.com', to: ['recipient@example.net'], subject: 'unsafe', text: 'fixture body', 'o:skip-verification': 'yes' }), error => error.status === 400);
  await assert.rejects(mailgunClient.messages.create('example.com', { from: 'sender@example.com', to: ['recipient@example.net'], subject: 'unsupported text tracking', html: '<b>fixture</b>', 'o:tracking-clicks': 'yes' }), error => error.status === 400);
  assert.equal((await records()).length, beforeCount);
});

test('Resend webhooks verify with the SDK and carry the id the send returned', async () => {
  const client = new Resend('fixture-key', { baseUrl: base.origin });
  const message = { from: 'sender@example.com', to: ['recipient@example.net'], subject: 'resend webhook', text: 'fixture body' };
  const single = await client.emails.send(message);
  assert.equal(single.error, null);
  const batch = await client.batch.send([{ ...message, subject: 'resend webhook batch one' }, { ...message, subject: 'resend webhook batch two' }]);
  assert.equal(batch.error, null);
  const ids = [single.data.id, ...batch.data.data.map(item => item.id)];
  assert.equal(ids.length, 3);
  for (const id of ids) {
    for (const event of ['accepted', 'bounced', 'opened']) {
      const hook = await webhook(id, 'resend', event);
      const verify = body => client.webhooks.verify({ payload: body, headers: { id: hook.headers['svix-id'], timestamp: hook.headers['svix-timestamp'], signature: hook.headers['svix-signature'] }, webhookSecret: hook.secret });
      const payload = verify(hook.body);
      assert.equal(payload.data.email_id, id);
      assert.equal(payload.type, { accepted: 'email.delivered', bounced: 'email.bounced', opened: 'email.opened' }[event]);
      assert.deepEqual(payload.data.to, ['recipient@example.net']);
      assert.throws(() => verify(hook.body.replace('email_id', 'email_ie')));
    }
  }
});

test('Postmark webhooks carry the MessageID the send returned', async () => {
  const client = new postmark.ServerClient('fixture-key', { useHttps: false, requestHost: base.host });
  const message = { From: 'sender@example.com', To: 'recipient@example.net', Subject: 'postmark webhook', TextBody: 'fixture body' };
  const single = await client.sendEmail(message);
  const batch = await client.sendEmailBatch([{ ...message, Subject: 'postmark webhook batch one' }, { ...message, Subject: 'postmark webhook batch two' }]);
  const ids = [single.MessageID, ...batch.map(item => item.MessageID)];
  assert.equal(ids.length, 3);
  const expected = { accepted: ['Delivery', 'Recipient'], bounced: ['Bounce', 'Email'], opened: ['Open', 'Recipient'] };
  for (const id of ids) {
    for (const [event, [recordType, recipientKey]] of Object.entries(expected)) {
      const hook = await webhook(id, 'postmark', event);
      const payload = JSON.parse(hook.body);
      assert.equal(payload.RecordType, recordType);
      assert.equal(payload.MessageID, id);
      assert.equal(payload[recipientKey], 'recipient@example.net');
      assert.ok(hook.headers['x-pm-webhook-trace-id']);
      assert.equal(hook.headers['svix-signature'], undefined);
    }
  }
});

test('Mailgun webhooks verify with the documented snippet and carry the message id', async () => {
  const client = new Mailgun(FormData).client({ username: 'api', key: 'fixture-key', url: base.origin });
  const result = await client.messages.create('example.com', { from: 'sender@example.com', to: ['recipient@example.net'], subject: 'mailgun webhook', text: 'fixture body' });
  for (const event of ['accepted', 'bounced', 'opened']) {
    const hook = await webhook(result.id, 'mailgun', event);
    const payload = JSON.parse(hook.body);
    const { timestamp, token, signature } = payload.signature;
    assert.equal(token.length, 50);
    assert.equal(createHmac('sha256', hook.secret).update(timestamp.concat(token)).digest('hex'), signature);
    assert.equal(payload['event-data'].message.headers['message-id'], result.id.replace(/^<|>$/g, ''));
    assert.equal(payload['event-data'].recipient, 'recipient@example.net');
  }
});
