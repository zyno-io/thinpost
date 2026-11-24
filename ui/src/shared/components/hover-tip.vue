<template>
    <span
        class="hover-tip"
        tabindex="0"
        :aria-describedby="tipId"
        @mouseenter="show"
        @mouseleave="hideSoon"
        @focus="show"
        @blur="hide"
    >
        <slot />
        <span :id="tipId" class="tip" :class="{ shown }" role="tooltip" :style="tipStyle" @mouseenter="keepOpen" @mouseleave="hideSoon">
            <slot name="tip" />
        </span>
    </span>
</template>

<script lang="ts" setup>
import { nextTick, onBeforeUnmount, ref, useId } from 'vue';

/**
 * A text tip for the content in the default slot, shown on hover and focus and dismissed with Escape.
 * The tip is always in the page, so a screen reader announces it as the trigger's description.
 * It uses fixed positioning so a scrolling table wrapper can't clip it. It sits below the trigger,
 * flips above when there is no room, and stays within the viewport. The pointer can move onto the tip.
 */
const tipId = `tip-${useId()}`;
const shown = ref(false);
const tipStyle = ref<Record<string, string>>({});
let anchor: HTMLElement | null = null;
let hideTimer: ReturnType<typeof setTimeout> | undefined;

const GAP = 6;
const MARGIN = 8;
const HIDE_DELAY_MS = 150;

function place() {
    if (!anchor) return;
    const rect = anchor.getBoundingClientRect();
    if (rect.bottom < 0 || rect.top > window.innerHeight || rect.right < 0 || rect.left > window.innerWidth) {
        hide();
        return;
    }
    const tip = anchor.querySelector<HTMLElement>('.tip');
    const tipRect = tip?.getBoundingClientRect();
    const tipH = tipRect?.height ?? 0;
    const tipW = tipRect?.width ?? 0;
    const below = rect.bottom + GAP;
    const flip = tipH > 0 && below + tipH > window.innerHeight - MARGIN && rect.top - GAP - tipH >= MARGIN;
    const top = flip ? rect.top - GAP - tipH : below;
    const maxLeft = Math.max(MARGIN, window.innerWidth - tipW - MARGIN);
    const left = Math.min(Math.max(MARGIN, rect.left), maxLeft);
    tipStyle.value = { top: `${Math.round(top)}px`, left: `${Math.round(left)}px` };
}

function show(e: Event) {
    clearTimeout(hideTimer);
    anchor = e.currentTarget as HTMLElement;
    place();
    if (shown.value) return;
    shown.value = true;
    // Measure the tip once it is rendered visible, then flip or clamp.
    void nextTick(place);
    window.addEventListener('scroll', place, true);
    window.addEventListener('resize', place);
    document.addEventListener('keydown', onDocumentKeydown);
}

function hide() {
    clearTimeout(hideTimer);
    shown.value = false;
    window.removeEventListener('scroll', place, true);
    window.removeEventListener('resize', place);
    document.removeEventListener('keydown', onDocumentKeydown);
}

// Leaving the trigger waits a moment so the pointer can reach the tip.
function hideSoon() {
    clearTimeout(hideTimer);
    hideTimer = setTimeout(hide, HIDE_DELAY_MS);
}

function keepOpen() {
    clearTimeout(hideTimer);
}

function onDocumentKeydown(e: KeyboardEvent) {
    if (e.key !== 'Escape' || !shown.value) return;
    e.stopPropagation();
    hide();
}

onBeforeUnmount(hide);
</script>

<style lang="scss" scoped>
.hover-tip {
    border-radius: 3px;
    text-decoration: underline dotted var(--text-3);
    text-underline-offset: 3px;
    cursor: default;

    &:focus-visible {
        outline: 2px solid var(--accent);
        outline-offset: 2px;
    }
}

.tip {
    position: fixed;
    z-index: 70;
    display: flex;
    flex-direction: column;
    gap: 2px;
    max-width: 280px;
    padding: 7px 10px;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface);
    box-shadow: var(--shadow-lg);
    color: var(--text);
    font-size: 12px;
    font-weight: 400;
    line-height: 1.4;
    text-align: left;
    text-decoration: none;
    white-space: nowrap;
    visibility: hidden;
    opacity: 0;
    transition: opacity 0.1s;

    &.shown {
        visibility: visible;
        opacity: 1;
    }
}
</style>
