import { mount, type VueWrapper } from '@vue/test-utils';
import { afterEach, describe, expect, it } from 'vitest';
import { nextTick } from 'vue';

import DropdownMenu from '@/shared/components/dropdown-menu.vue';

let wrapper: VueWrapper | undefined;

function mountMenu() {
    wrapper = mount(DropdownMenu, {
        props: { label: 'Test menu' },
        slots: {
            trigger: '<span>open</span>',
            default: `
                <button type="button" class="menu-item" role="menuitemradio" aria-checked="false" id="a">A</button>
                <button type="button" class="menu-item" role="menuitemradio" aria-checked="true" id="b">B</button>
                <button type="button" class="menu-item" role="menuitem" id="c">C</button>`
        },
        attachTo: document.body
    });
    return wrapper;
}

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
});

describe('dropdown menu', () => {
    it('opens on ArrowDown and focuses the checked radio item', async () => {
        const menu = mountMenu();
        await menu.get('.dropdown-trigger').trigger('keydown', { key: 'ArrowDown' });
        await nextTick();
        await nextTick();
        expect(menu.find('[role="menu"]').exists()).toBe(true);
        expect(document.activeElement?.id).toBe('b');
    });

    it('moves with arrows, Home and End', async () => {
        const menu = mountMenu();
        await menu.get('.dropdown-trigger').trigger('keydown', { key: 'ArrowDown' });
        await nextTick();
        await nextTick();
        const popup = menu.get('[role="menu"]');
        await popup.trigger('keydown', { key: 'ArrowDown' });
        expect(document.activeElement?.id).toBe('c');
        await popup.trigger('keydown', { key: 'ArrowDown' });
        expect(document.activeElement?.id).toBe('a');
        await popup.trigger('keydown', { key: 'End' });
        expect(document.activeElement?.id).toBe('c');
        await popup.trigger('keydown', { key: 'Home' });
        expect(document.activeElement?.id).toBe('a');
    });

    it('closes on Escape and returns focus to the trigger', async () => {
        const menu = mountMenu();
        await menu.get('.dropdown-trigger').trigger('keydown', { key: 'ArrowDown' });
        await nextTick();
        await nextTick();
        await menu.get('[role="menu"]').trigger('keydown', { key: 'Escape' });
        await nextTick();
        expect(menu.find('[role="menu"]').exists()).toBe(false);
        expect(document.activeElement).toBe(menu.get('.dropdown-trigger').element);
    });

    it('closes on an outside pointerdown', async () => {
        const menu = mountMenu();
        await menu.get('.dropdown-trigger').trigger('click');
        await nextTick();
        expect(menu.find('[role="menu"]').exists()).toBe(true);
        document.body.dispatchEvent(new Event('pointerdown', { bubbles: true }));
        await nextTick();
        expect(menu.find('[role="menu"]').exists()).toBe(false);
    });

    it('closes when a menu item is clicked', async () => {
        const menu = mountMenu();
        await menu.get('.dropdown-trigger').trigger('click');
        await nextTick();
        await menu.get('#c').trigger('click');
        await nextTick();
        expect(menu.find('[role="menu"]').exists()).toBe(false);
    });
});
