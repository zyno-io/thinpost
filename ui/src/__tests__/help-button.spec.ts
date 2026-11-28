import { flushPromises, mount } from '@vue/test-utils';
import { installVf } from '@zyno-io/vue-foundation';
import { afterEach, describe, expect, it } from 'vitest';

import HelpButton from '@/shared/components/help-button.vue';
import { openModalCount } from '@/shared/modal-state';

describe('help button', () => {
    afterEach(() => {
        document.body.innerHTML = '';
        openModalCount.value = 0;
    });

    function mountButton() {
        return mount(HelpButton, {
            props: { topic: 'Widgets' },
            slots: { default: '<p>Widgets explained.</p>' },
            global: { plugins: [installVf] },
            attachTo: document.body
        });
    }

    it('shows only the button until pressed, then the explanation in a labeled dialog', async () => {
        const wrapper = mountButton();
        const button = wrapper.get('button[aria-label="About Widgets"]');
        expect(button.attributes('aria-haspopup')).toBe('dialog');
        expect(document.body.textContent).not.toContain('Widgets explained.');
        expect(openModalCount.value).toBe(0);

        await button.trigger('click');
        await flushPromises();
        const dialog = document.querySelector('[role="dialog"]')!;
        expect(dialog.textContent).toContain('Widgets explained.');
        expect(dialog.getAttribute('aria-labelledby')).toBeTruthy();
        expect(openModalCount.value).toBe(1);
    });

    it('closes with the Close button and returns focus to the help button', async () => {
        const wrapper = mountButton();
        const button = wrapper.get('button[aria-label="About Widgets"]');
        await button.trigger('click');
        await flushPromises();
        document.querySelector<HTMLButtonElement>('[role="dialog"] button[type="submit"]')!.click();
        await flushPromises();
        expect(document.querySelector('[role="dialog"]')).toBeNull();
        expect(openModalCount.value).toBe(0);
        expect(document.activeElement).toBe(button.element);
    });

    it('closes on Escape and returns focus to the help button', async () => {
        const wrapper = mountButton();
        const button = wrapper.get('button[aria-label="About Widgets"]');
        await button.trigger('click');
        await flushPromises();
        document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
        await flushPromises();
        expect(document.querySelector('[role="dialog"]')).toBeNull();
        expect(document.activeElement).toBe(button.element);
    });
});
