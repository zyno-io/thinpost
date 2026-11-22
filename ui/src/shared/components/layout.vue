<template>
    <div class="shell">
        <a class="skip-link" href="#main-content">Skip to content</a>
        <header class="topbar" :inert="isModalOpen">
            <div class="topbar-inner">
                <RouterLink to="/" class="brand" aria-label="Thinpost home">
                    <span class="brand-mark" aria-hidden="true"><i class="fa-solid fa-paper-plane" /></span>
                    <span class="brand-name">Thinpost</span>
                </RouterLink>

                <nav class="nav" aria-label="Primary">
                    <RouterLink
                        v-for="item in NAV"
                        :key="item.id"
                        :to="item.path"
                        class="nav-link"
                        :class="{ active: route.meta.nav === item.id }"
                        :aria-current="route.meta.nav === item.id ? 'page' : undefined"
                    >
                        {{ item.label }}
                    </RouterLink>
                </nav>

                <DropdownMenu class="nav-menu" label="Navigation" align="left" trigger-class="icon-trigger">
                    <template #trigger><i class="fa-solid fa-bars" aria-hidden="true" /></template>
                    <RouterLink
                        v-for="item in NAV"
                        :key="item.id"
                        :to="item.path"
                        class="menu-item"
                        role="menuitem"
                        :aria-current="route.meta.nav === item.id ? 'page' : undefined"
                    >
                        <i :class="item.icon" aria-hidden="true" />
                        {{ item.label }}
                        <i v-if="route.meta.nav === item.id" class="fa-solid fa-check check" aria-hidden="true" />
                    </RouterLink>
                </DropdownMenu>

                <div class="topbar-right">
                    <ThemeMenu />
                    <AccountMenu />
                </div>
            </div>
        </header>

        <main id="main-content" tabindex="-1">
            <div v-if="authState.status === 'ended'" class="notice warning session-ended" role="alert" :inert="isModalOpen">
                <i class="fa-solid fa-lock" aria-hidden="true" />
                <div class="notice-body">
                    <p>{{ authState.notice }}</p>
                    <p>Changes and updates won't work until you sign in again. Signing in takes you away from this page.</p>
                </div>
                <div class="notice-actions">
                    <button type="button" class="btn primary sm" @click="signIn">Sign in</button>
                </div>
            </div>

            <div class="page">
                <slot />
            </div>
        </main>
    </div>
</template>

<script lang="ts" setup>
import { computed } from 'vue';
import { useRoute } from 'vue-router';

import { authState, signIn } from '@/auth';
import { NAV } from '@/nav';
import { openModalCount } from '@/shared/modal-state';

import AccountMenu from './account-menu.vue';
import DropdownMenu from './dropdown-menu.vue';
import ThemeMenu from './theme-menu.vue';

const route = useRoute();

// Keep keyboard focus out of navigation, sign-in and sign-out while a modal (such as a one-time secret) is open.
const isModalOpen = computed(() => openModalCount.value > 0);

</script>

<style lang="scss" scoped>
.shell {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-width: 0;
}

.skip-link {
    position: absolute;
    left: 12px;
    top: -40px;
    z-index: 100;
    padding: 8px 12px;
    border-radius: var(--radius);
    background: var(--accent);
    color: var(--accent-text);

    &:focus {
        top: 10px;
    }
}

.topbar {
    position: sticky;
    top: 0;
    z-index: 40;
    height: var(--header-h);
    border-bottom: 1px solid var(--border);
    background: color-mix(in srgb, var(--surface) 88%, transparent);
    backdrop-filter: saturate(1.4) blur(10px);
}

.topbar-inner {
    display: flex;
    align-items: center;
    gap: 18px;
    height: 100%;
    padding: 0 16px;
}

.brand {
    display: inline-flex;
    align-items: center;
    gap: 9px;
    color: var(--text);
    font-weight: 650;
    font-size: 14px;
    letter-spacing: -0.01em;
    text-decoration: none !important;
}

.brand-mark {
    display: grid;
    place-items: center;
    width: 24px;
    height: 24px;
    border-radius: 7px;
    background: var(--text);
    color: var(--bg);
    font-size: 11px;
}

.nav {
    display: flex;
    gap: 2px;
}

.nav-menu {
    display: none;
}

.nav-link {
    padding: 6px 10px;
    border-radius: var(--radius-sm);
    color: var(--text-2);
    font-size: 13px;
    font-weight: 500;
    text-decoration: none !important;

    &:hover {
        background: var(--surface-2);
        color: var(--text);
    }

    &.active {
        background: var(--surface-3);
        color: var(--text);
    }
}

.topbar-right {
    display: flex;
    align-items: center;
    gap: 6px;
    margin-left: auto;

    :deep(.icon-trigger) {
        justify-content: center;
        width: 32px;
        padding: 0;
    }
}

.nav-menu :deep(.icon-trigger) {
    justify-content: center;
    width: 32px;
    padding: 0;
}


.session-ended {
    width: calc(100% - 40px);
    max-width: 1120px;
    margin: 16px auto 0;
}

main {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-width: 0;

    &:focus {
        outline: none;
    }
}

@media (max-width: 900px) {
    .nav {
        display: none;
    }

    .nav-menu {
        display: block;
    }
}

@media (max-width: 640px) {
    .topbar-inner {
        gap: 10px;
        padding: 0 12px;
    }

    .brand-name {
        display: none;
    }

    .session-ended {
        width: calc(100% - 24px);
    }
}

@media (max-width: 360px) {
    .topbar-inner {
        gap: 8px;
        padding: 0 8px;
    }
}
</style>
