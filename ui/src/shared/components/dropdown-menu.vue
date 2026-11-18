<template>
    <div ref="root" class="dropdown" @keydown.esc.stop="close(true)">
        <button class="dropdown-trigger"
            :id="buttonId"
            ref="trigger"
            type="button"
            :class="triggerClass"
            aria-haspopup="menu"
            :aria-expanded="open"
            :aria-controls="menuId"
            :aria-label="label"
            @click="toggle"
            @keydown.down.prevent="openAndFocus(0)"
            @keydown.up.prevent="openAndFocus(-1)"
        >
            <slot name="trigger" />
        </button>
        <Transition name="dropdown">
            <div
                v-if="open"
                :id="menuId"
                ref="menu"
                class="dropdown-menu"
                :class="[`align-${align}`]"
                :style="menuStyle"
                role="menu"
                :aria-labelledby="buttonId"
                @keydown.down.prevent="move(1)"
                @keydown.up.prevent="move(-1)"
                @keydown.home.prevent="focusAt(0)"
                @keydown.end.prevent="focusAt(-1)"
                @keydown.tab="close(false)"
                @click="onMenuClick"
            >
                <slot :close="() => close(true)" />
            </div>
        </Transition>
    </div>
</template>

<script lang="ts" setup>
import { nextTick, onBeforeUnmount, ref, useId } from 'vue';

const props = withDefaults(
    defineProps<{
        label: string;
        align?: 'left' | 'right';
        triggerClass?: string | string[];
    }>(),
    { align: 'right', triggerClass: undefined }
);

const open = ref(false);
// Fixed positioning lets the menu escape scroll containers (for example a table wrapper) instead of being clipped.
const menuStyle = ref<Record<string, string>>({});

function place() {
    const rect = trigger.value?.getBoundingClientRect();
    if (!rect) return;
    const top = `${Math.round(rect.bottom + 6)}px`;
    menuStyle.value =
        props.align === 'right'
            ? { top, right: `${Math.max(8, Math.round(window.innerWidth - rect.right))}px`, left: 'auto' }
            : { top, left: `${Math.max(8, Math.round(rect.left))}px`, right: 'auto' };
}

function onViewportChange() {
    if (open.value) close(false);
}
const root = ref<HTMLElement>();
const trigger = ref<HTMLButtonElement>();
const menu = ref<HTMLElement>();
const id = useId();
const buttonId = `${id}-trigger`;
const menuId = `${id}-menu`;

function items(): HTMLElement[] {
    return Array.from(menu.value?.querySelectorAll<HTMLElement>('[role^="menuitem"]:not([aria-disabled="true"])') ?? []);
}

function focusAt(index: number) {
    const list = items();
    if (!list.length) return;
    list[(index + list.length) % list.length]?.focus();
}

function move(delta: number) {
    const list = items();
    const current = list.indexOf(document.activeElement as HTMLElement);
    focusAt(current < 0 ? (delta > 0 ? 0 : -1) : current + delta);
}

async function openAndFocus(index: number) {
    place();
    open.value = true;
    await nextTick();
    const list = items();
    const checked = list.findIndex(item => item.getAttribute('aria-checked') === 'true');
    focusAt(checked >= 0 && index === 0 ? checked : index);
}

function toggle() {
    if (open.value) close(false);
    else void openAndFocus(0);
}

function close(returnFocus: boolean) {
    if (!open.value) return;
    open.value = false;
    if (returnFocus) trigger.value?.focus();
}

function onMenuClick(e: MouseEvent) {
    const target = e.target as HTMLElement;
    if (target.closest('[role^="menuitem"]')) close(true);
}

function onDocumentPointer(e: PointerEvent) {
    if (open.value && root.value && !root.value.contains(e.target as Node)) close(false);
}

document.addEventListener('pointerdown', onDocumentPointer);
window.addEventListener('resize', onViewportChange);
window.addEventListener('scroll', onViewportChange, true);
onBeforeUnmount(() => {
    document.removeEventListener('pointerdown', onDocumentPointer);
    window.removeEventListener('resize', onViewportChange);
    window.removeEventListener('scroll', onViewportChange, true);
});
</script>

<style lang="scss" scoped>
.dropdown {
    position: relative;
}

.dropdown-trigger {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    height: 32px;
    padding: 0 8px;
    border: 1px solid transparent;
    border-radius: var(--radius);
    background: transparent;
    color: var(--text-2);
    font: inherit;
    font-size: 13px;
    cursor: pointer;

    &:hover,
    &[aria-expanded='true'] {
        background: var(--surface-2);
        color: var(--text);
    }
}

.dropdown-menu {
    position: fixed;
    z-index: 60;
    min-width: 200px;
    padding: 5px;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface);
    box-shadow: var(--shadow-lg);

    :deep(.menu-item) {
        display: flex;
        align-items: center;
        gap: 10px;
        width: 100%;
        padding: 7px 9px;
        border: 0;
        border-radius: 6px;
        background: none;
        color: var(--text);
        font: inherit;
        font-size: 13px;
        text-align: left;
        text-decoration: none;
        cursor: pointer;

        > i:first-child {
            width: 14px;
            color: var(--text-3);
            text-align: center;
        }

        .check {
            margin-left: auto;
            color: var(--accent);
            font-size: 11px;
        }

        &:hover,
        &:focus-visible {
            outline: none;
            background: var(--surface-2);
        }

        &[aria-disabled='true'] {
            color: var(--text-3);
            cursor: not-allowed;
            opacity: 0.55;

            &:hover {
                background: none;
            }
        }
    }

    :deep(.menu-label) {
        padding: 6px 9px 4px;
        color: var(--text-3);
        font-size: 11px;
        font-weight: 600;
        letter-spacing: 0.04em;
        text-transform: uppercase;
    }

    :deep(.menu-separator) {
        height: 1px;
        margin: 5px 0;
        background: var(--border);
    }

    :deep(.menu-meta) {
        padding: 6px 9px 8px;
        color: var(--text-3);
        font-size: 12px;

        strong {
            display: block;
            color: var(--text);
            font-size: 13px;
            font-weight: 600;
        }
    }
}

.dropdown-enter-active,
.dropdown-leave-active {
    transition:
        opacity 0.1s,
        transform 0.1s;
}

.dropdown-enter-from,
.dropdown-leave-to {
    opacity: 0;
    transform: translateY(-3px);
}
</style>
