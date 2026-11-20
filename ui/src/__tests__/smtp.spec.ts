import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { FromForm } from '@/smtp';

import { httpError } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    getRuntime: vi.fn(),
    getAppSmtp: vi.fn(),
    listAppDomains: vi.fn(),
    updateAppSmtpSettings: vi.fn(),
    createAppSmtpCredential: vi.fn(),
    revokeAppSmtpCredential: vi.fn(),
    getAppMessage: vi.fn(),
    getAppMessageBody: vi.fn(),
    getPublicConfig: vi.fn()
}));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const vf = vi.hoisted(() => ({ showConfirm: vi.fn(), showConfirmDestroy: vi.fn(), showToast: vi.fn() }));
vi.mock('@zyno-io/vue-foundation', async importOriginal => {
    const original = await importOriginal<object>();
    return { ...original, ...vf };
});

const { authState } = await import('@/auth');
const { resetRuntimeFeatures } = await import('@/runtime-features');
const { default: AppSmtp } = await import('@/screens/app-smtp.vue');
const { default: MessageDetail } = await import('@/screens/message-detail.vue');
const smtp = await import('@/smtp');
const { readMessageSummary } = await import('@/message-summary');
const { parseRuntimeInfo } = await import('@/tracking');

const APP_ID = '0123456789abcdef0123456789abcdef';
const CRED_ID = 'fedcba9876543210fedcba9876543210';
const PASSWORD = 'tps_one_time_password_value_for_the_test_0';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };
const credential = { id: CRED_ID, name: 'Office printer', username: CRED_ID, active: true, created_at: '2026-10-02T12:00:00Z' };
const policy = { mode: 'off', only_unauthorized: true };

function smtpResponse(overrides: Record<string, unknown> = {}) {
    return {
        settings: { from: policy, signing_ready: [] },
        credentials: [credential],
        connection: {
            host: 'mail.example.com',
            host_source: 'primary',
            primary_unnamed: false,
            ports: [
                { port: 465, security: 'tls' },
                { port: 587, security: 'starttls' }
            ],
            observed_ports: [465],
            observed_at: '2026-11-19T12:00:00Z'
        },
        ...overrides
    };
}

let wrapper: VueWrapper | undefined;

async function mountSmtp() {
    const mounted = await mountAppRoute(`/apps/${APP_ID}/smtp`, { 'app-smtp': AppSmtp });
    wrapper = mounted.wrapper;
    return wrapper;
}

/** Fills in and submits the name dialog, which is rendered outside the screen's own form. */
async function submitName(name: string) {
    const dialog = document.querySelector('[role="dialog"]')!;
    const input = dialog.querySelector<HTMLInputElement>('input[type="text"]')!;
    input.value = name;
    input.dispatchEvent(new Event('input', { bubbles: true }));
    dialog.querySelector('form')!.dispatchEvent(new Event('submit', { cancelable: true }));
    await flushPromises();
}

function button(label: string) {
    const match = wrapper!.findAll('button').find(b => b.text().includes(label));
    if (!match) throw new Error(`button "${label}" not found`);
    return match;
}

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    resetRuntimeFeatures();
    authState.status = 'signed-in';
    api.listApps.mockResolvedValue({ data: { apps: [app] } });
    api.getRuntime.mockResolvedValue({ data: { features: { submission: true, tracking: false, smtp_submission: true } } });
    api.getAppSmtp.mockResolvedValue({ data: smtpResponse() });
    api.listAppDomains.mockResolvedValue({
        data: {
            domains: [
                { name: 'billing.example.com', active: true, created_at: '2026-10-04T12:00:00Z' },
                { name: 'pending.example.com', active: false, created_at: '2026-10-04T12:00:00Z' }
            ]
        }
    });
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
});

describe('SMTP helpers', () => {
    it('reads the SMTP feature flag, defaulting to off for older servers', () => {
        expect(parseRuntimeInfo({ features: { submission: true, tracking: true } }).features.smtp_submission).toBe(false);
        expect(parseRuntimeInfo({ features: { submission: true, tracking: true, smtp_submission: true } }).features.smtp_submission).toBe(true);
    });

    it('rejects malformed SMTP responses', () => {
        expect(() => smtp.parseAppSmtp({})).toThrow();
        expect(() => smtp.parseAppSmtp(smtpResponse({ credentials: [{ id: 1 }] }))).toThrow();
        expect(() => smtp.parseAppSmtp(smtpResponse({ settings: { from: { mode: 'weird', only_unauthorized: true }, signing_ready: null } }))).toThrow();
        expect(smtp.parseAppSmtp(smtpResponse({ settings: { from: policy, signing_ready: null } })).settings.signing_ready).toBeNull();
    });

    it('needs both a username and a password from a created credential', () => {
        expect(smtp.parseIssuedCredential({ credential, username: CRED_ID, password: PASSWORD })).toEqual({ name: 'Office printer', username: CRED_ID, password: PASSWORD });
        expect(smtp.parseIssuedCredential({ credential, username: CRED_ID })).toBeNull();
        expect(smtp.parseIssuedCredential({ credential, password: PASSWORD })).toBeNull();
    });

    it('reports port availability only after the Edge has reported', () => {
        expect(smtp.portAvailability([465], undefined, 465)).toBe('unknown');
        expect(smtp.portAvailability([465], '2026-11-19T12:00:00Z', 465)).toBe('listening');
        expect(smtp.portAvailability([465], '2026-11-19T12:00:00Z', 587)).toBe('not-listening');
    });

    describe('From policy validation', () => {
        const verified = ['billing.example.com'];
        const form = (patch: Partial<FromForm>): FromForm => ({ mode: 'off', domain: '', address: '', onlyUnauthorized: true, ...patch });

        it('accepts the three modes with only their own fields', () => {
            expect(smtp.validateFromForm(form({ domain: 'x' }), verified).policy).toEqual({ mode: 'off', only_unauthorized: true });
            expect(smtp.validateFromForm(form({ mode: 'domain', domain: 'billing.example.com', address: 'ignored@example.org' }), verified).policy).toEqual({
                mode: 'domain',
                domain: 'billing.example.com',
                only_unauthorized: true
            });
            expect(smtp.validateFromForm(form({ mode: 'address', address: ' No-Reply@Billing.Example.com ' }), verified).policy).toEqual({
                mode: 'address',
                address: 'No-Reply@billing.example.com',
                only_unauthorized: true
            });
        });

        it('refuses a domain that is not verified and a missing domain', () => {
            expect(smtp.validateFromForm(form({ mode: 'domain' }), verified).domainError).toContain('Choose');
            expect(smtp.validateFromForm(form({ mode: 'domain', domain: 'other.example.com' }), verified).domainError).toContain('verified');
        });

        it.each([
            ['', 'Enter the address'],
            ['no-reply', 'full address'],
            ['a b@billing.example.com', 'before the @'],
            ['a..b@billing.example.com', 'before the @'],
            ['.a@billing.example.com', 'before the @'],
            ['"a b"@billing.example.com', 'before the @'],
            [`${'a'.repeat(65)}@billing.example.com`, 'before the @'],
            ['é@billing.example.com', 'plain letters'],
            ['a@other.example.com', 'verified sending domain']
        ])('refuses the address %j', (address, message) => {
            const result = smtp.validateFromForm(form({ mode: 'address', address }), verified);
            expect(result.policy).toBeNull();
            expect(result.addressError).toContain(message);
        });

        it('shows what each policy does to an address', () => {
            expect(smtp.fromExamples(form({ mode: 'domain', domain: 'billing.example.com' }), verified)[0]).toEqual({ sent: 'scanner@office.example.net', becomes: 'scanner=office.example.net@billing.example.com' });
            expect(smtp.fromExamples(form({ mode: 'address', address: 'no-reply@billing.example.com', onlyUnauthorized: false }), verified)).toEqual([
                { sent: 'scanner@office.example.net', becomes: 'no-reply@billing.example.com' }
            ]);
            expect(smtp.fromExamples(form({ mode: 'off' }), verified)).toEqual([]);
            // A long target leaves less room: the example is shortened like the server's, with the hash shown as <hash>.
            const longTarget = `${'t'.repeat(63)}.${'u'.repeat(63)}.${'v'.repeat(63)}.${'w'.repeat(40)}.example.com`;
            const [long] = smtp.fromExamples(form({ mode: 'domain', domain: longTarget, onlyUnauthorized: false }), [longTarget]);
            expect(long!.becomes.endsWith(`-<hash>@${longTarget}`)).toBe(true);
            expect(long!.becomes.length - '<hash>'.length + 8).toBeLessThanOrEqual(254);
        });
    });
});

describe('SMTP tab', () => {
    it('is hidden until the server reports SMTP submission and shown after', async () => {
        api.getRuntime.mockResolvedValue({ data: { features: { submission: true, tracking: false, smtp_submission: false } } });
        await mountSmtp();
        expect(wrapper!.find('nav[aria-label="App sections"]').text()).not.toContain('SMTP');
        expect(wrapper!.find('[data-testid="smtp-unavailable"]').exists()).toBe(true);
        expect(api.getAppSmtp).not.toHaveBeenCalled();
        wrapper!.unmount();

        resetRuntimeFeatures();
        api.getRuntime.mockResolvedValue({ data: { features: { submission: true, tracking: false, smtp_submission: true } } });
        await mountSmtp();
        expect(wrapper!.find('nav[aria-label="App sections"]').text()).toContain('SMTP');
        expect(wrapper!.find('[data-testid="smtp-connect"]').exists()).toBe(true);
    });

    it('stays hidden when the server\'s features can\'t be read', async () => {
        api.getRuntime.mockRejectedValue(httpError(503));
        await mountSmtp();
        expect(wrapper!.find('nav[aria-label="App sections"]').text()).not.toContain('SMTP');
        // The screen still works by direct link: the API decides.
        expect(wrapper!.find('[data-testid="smtp-connect"]').exists()).toBe(true);
    });

    it('shows the host, both ports and what the Edge reported', async () => {
        await mountSmtp();
        const connect = wrapper!.find('[data-testid="smtp-connect"]');
        const values = connect.findAll<HTMLInputElement>('input[readonly]').map(i => i.element.value);
        expect(values).toEqual(['mail.example.com', '465', '587']);
        expect(wrapper!.find('[data-testid="smtp-port-465-status"]').text()).toBe('Available');
        expect(wrapper!.find('[data-testid="smtp-port-587-status"]').text()).toBe('Not available right now');
        expect(wrapper!.find('[data-testid="smtp-recommendation"]').text()).toContain('port 465');
        expect(connect.text()).toContain('STARTTLS');
    });

    it('asks for a server name, with a link, when the main sending IP is unnamed', async () => {
        api.getAppSmtp.mockResolvedValue({
            data: smtpResponse({ connection: { host_source: null, primary_unnamed: true, ports: [], observed_ports: [] } })
        });
        await mountSmtp();
        expect(wrapper!.find('[data-testid="smtp-host-unnamed"]').exists()).toBe(true);
        expect(wrapper!.find('[data-testid="smtp-host-operations-link"]').attributes('href')).toBe('/operations');
        expect(wrapper!.find('[data-testid="smtp-port-465-status"]').text()).toBe('Not checked yet');
        expect(wrapper!.find('[data-testid="smtp-connect"]').text()).not.toMatch(/HELO|Edge|primary source/i);
    });

    it('creates a credential, shows its username and password once, and never stores them', async () => {
        api.getAppSmtp.mockResolvedValueOnce({ data: smtpResponse({ credentials: [] }) }).mockResolvedValue({ data: smtpResponse() });
        api.createAppSmtpCredential.mockResolvedValue({ data: { credential, username: CRED_ID, password: PASSWORD } });
        vf.showConfirm.mockResolvedValue(true);
        const setItem = vi.spyOn(Storage.prototype, 'setItem');

        await mountSmtp();
        expect(wrapper!.text()).toContain('No SMTP credentials');
        await button('Create credential').trigger('click');
        await submitName('Office printer');

        expect(api.createAppSmtpCredential).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { name: 'Office printer' } });
        const readonly = Array.from(document.querySelectorAll<HTMLInputElement>('[role="alertdialog"] input[readonly]')).map(i => i.value);
        expect(readonly).toEqual([CRED_ID, PASSWORD]);
        expect(document.querySelector('[role="alertdialog"]')!.textContent).toContain('Password');
        expect(api.getAppSmtp).toHaveBeenCalledTimes(2);
        expect(setItem.mock.calls.flat().join(' ')).not.toContain(PASSWORD);
        expect(JSON.stringify({ ...localStorage, ...sessionStorage })).not.toContain(PASSWORD);

        await document.querySelector<HTMLFormElement>('[role="alertdialog"] form')!.dispatchEvent(new Event('submit', { cancelable: true }));
        await flushPromises();
        expect(vf.showConfirm).toHaveBeenCalled();
        expect(document.body.innerHTML).not.toContain(PASSWORD);
        expect(wrapper!.text()).toContain('Office printer');
        setItem.mockRestore();
    });

    it('explains the credential limit', async () => {
        api.getAppSmtp.mockResolvedValue({ data: smtpResponse({ credentials: Array.from({ length: 10 }, (_, i) => ({ ...credential, id: String(i).padStart(32, '0'), username: String(i).padStart(32, '0') })) }) });
        await mountSmtp();
        expect(button('Create credential').attributes('disabled')).toBeDefined();
        expect(wrapper!.text()).toContain('10 active credentials');
    });

    it('maps a 409 on create to the limit message', async () => {
        api.createAppSmtpCredential.mockRejectedValue(httpError(409));
        await mountSmtp();
        await button('Create credential').trigger('click');
        await submitName('Another');
        expect(document.body.textContent).toContain('already has 10 active credentials');
    });

    it('revokes after confirmation', async () => {
        api.revokeAppSmtpCredential.mockResolvedValue({});
        vf.showConfirmDestroy.mockResolvedValue(true);
        await mountSmtp();
        await button('Revoke').trigger('click');
        await flushPromises();
        expect(vf.showConfirmDestroy).toHaveBeenCalled();
        expect(api.revokeAppSmtpCredential).toHaveBeenCalledWith({ path: { appId: APP_ID, credentialId: CRED_ID } });
        expect(api.getAppSmtp).toHaveBeenCalledTimes(2);
    });

    it('does not revoke when the confirmation is declined', async () => {
        vf.showConfirmDestroy.mockResolvedValue(false);
        await mountSmtp();
        await button('Revoke').trigger('click');
        await flushPromises();
        expect(api.revokeAppSmtpCredential).not.toHaveBeenCalled();
    });

    it('saves a domain policy with examples and warns when mail would not be signed', async () => {
        api.updateAppSmtpSettings.mockResolvedValue({
            data: { from: { mode: 'domain', domain: 'billing.example.com', only_unauthorized: true }, signing_ready: [{ domain: 'billing.example.com', ready: false }] }
        });
        await mountSmtp();
        expect(wrapper!.find('[data-testid="smtp-save"]').attributes('disabled')).toBeDefined();
        await wrapper!.find('[data-testid="smtp-mode-domain"]').setValue(true);
        const select = wrapper!.find<HTMLSelectElement>('#smtp-from-domain');
        expect(select.findAll('option').map(o => o.text())).toEqual(['Choose a domain', 'billing.example.com']);
        await select.setValue('billing.example.com');
        expect(wrapper!.find('[data-testid="smtp-examples"]').text()).toContain('scanner=office.example.net@billing.example.com');
        expect(wrapper!.find('[data-testid="smtp-keep-origin"]').exists()).toBe(false);

        await wrapper!.find('[data-testid="smtp-from"] form').trigger('submit');
        await flushPromises();
        expect(api.updateAppSmtpSettings).toHaveBeenCalledWith({
            path: { appId: APP_ID },
            body: { from: { mode: 'domain', domain: 'billing.example.com', only_unauthorized: true } }
        });
        expect(wrapper!.find('[data-testid="smtp-signing-warning"]').text()).toContain('billing.example.com');
        expect(wrapper!.text()).toContain('Saved');
    });

    it('does not send an invalid address and shows why', async () => {
        await mountSmtp();
        await wrapper!.find('[data-testid="smtp-mode-address"]').setValue(true);
        await wrapper!.find('#smtp-from-address').setValue('no-reply@other.example.com');
        await wrapper!.find('[data-testid="smtp-from"] form').trigger('submit');
        await flushPromises();
        expect(api.updateAppSmtpSettings).not.toHaveBeenCalled();
        expect(wrapper!.find('#smtp-from-address-hint').text()).toContain('verified sending domain');
        expect(wrapper!.find('[data-testid="smtp-save"]').attributes('disabled')).toBeDefined();
    });

    it('shows the server\'s refusal of an unverified domain', async () => {
        api.updateAppSmtpSettings.mockRejectedValue(httpError(409));
        await mountSmtp();
        await wrapper!.find('[data-testid="smtp-mode-address"]').setValue(true);
        await wrapper!.find('#smtp-from-address').setValue('no-reply@billing.example.com');
        await wrapper!.find('[data-testid="smtp-from"] form').trigger('submit');
        await flushPromises();
        expect(wrapper!.find('[data-testid="smtp-from"]').text()).toContain('verified sending domain of this app');
    });

    it('lists nothing to choose when the app has no verified domain', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [{ name: 'pending.example.com', active: false, created_at: '2026-10-04T12:00:00Z' }] } });
        await mountSmtp();
        expect(wrapper!.find('[data-testid="smtp-no-domains"]').text()).toContain('no verified sending domain');
    });

    it('shows a failed load with a retry', async () => {
        api.getAppSmtp.mockRejectedValueOnce(httpError(503)).mockResolvedValue({ data: smtpResponse() });
        await mountSmtp();
        expect(wrapper!.text()).toContain('temporarily unavailable');
        await button('Try again').trigger('click');
        await flushPromises();
        expect(wrapper!.find('[data-testid="smtp-connect"]').exists()).toBe(true);
    });
});

describe('SMTP messages', () => {
    const MESSAGE_ID = 'abababababababababababababababab';
    const summary = {
        id: MESSAGE_ID,
        state: 'prepared',
        status: 'delivered',
        enhanced_protection: false,
        subject_masked: false,
        subject: 'Scan',
        accepted_at: '2026-10-02T12:00:00Z',
        delivery_count: 1,
        accepted_count: 1,
        failed_count: 0,
        uncertain_count: 0
    };

    it('reads the smtp object only when it is well formed', () => {
        expect(readMessageSummary(summary)?.smtp).toBeNull();
        expect(readMessageSummary({ ...summary, smtp: { credential_id: CRED_ID, credential_name: 'Office printer', original_from: 'printer@office.example.net' } })?.smtp).toEqual({
            credentialName: 'Office printer',
            originalFrom: 'printer@office.example.net'
        });
        expect(readMessageSummary({ ...summary, smtp: { credential_id: CRED_ID, credential_name: 'Office printer' } })?.smtp).toEqual({ credentialName: 'Office printer', originalFrom: null });
        expect(readMessageSummary({ ...summary, smtp: 'yes' })?.smtp).toBeNull();
        expect(readMessageSummary({ ...summary, smtp: { credential_name: 3 } })?.smtp).toBeNull();
    });

    async function openDetail(message: object) {
        api.getAppMessage.mockResolvedValue({ data: { message, deliveries: [] } });
        api.getAppMessageBody.mockResolvedValue({ data: { from: 'a@example.com', to: [], subject: 'Scan', enhanced_protection: false, attachments: [] } });
        const mounted = await mountAppRoute(`/apps/${APP_ID}/messages/${MESSAGE_ID}`, { 'app-message': MessageDetail });
        wrapper = mounted.wrapper;
    }

    it('shows the SMTP badge, credential name and original From on the message', async () => {
        await openDetail({ ...summary, smtp: { credential_id: CRED_ID, credential_name: 'Office printer', original_from: 'printer@office.example.net' } });
        expect(wrapper!.find('[data-fact="smtp"]').text()).toContain('SMTP');
        expect(wrapper!.find('[data-fact="smtp"]').text()).toContain('Office printer');
        expect(wrapper!.find('[data-fact="original-from"]').text()).toContain('printer@office.example.net');
    });

    it('shows neither for a message that was not sent over SMTP', async () => {
        await openDetail(summary);
        expect(wrapper!.find('[data-fact="smtp"]').exists()).toBe(false);
        expect(wrapper!.find('[data-fact="original-from"]').exists()).toBe(false);
    });

    it('omits the Original From line when the From was not replaced', async () => {
        await openDetail({ ...summary, smtp: { credential_id: CRED_ID, credential_name: 'Office printer' } });
        expect(wrapper!.find('[data-fact="smtp"]').exists()).toBe(true);
        expect(wrapper!.find('[data-fact="original-from"]').exists()).toBe(false);
    });
});
