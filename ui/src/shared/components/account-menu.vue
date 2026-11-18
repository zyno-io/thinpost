<template>
    <DropdownMenu label="Account" trigger-class="icon-trigger">
        <template #trigger>
            <i class="fa-regular fa-circle-user" aria-hidden="true" />
        </template>
        <div class="menu-meta">
            <strong v-if="authState.displayName">{{ authState.displayName }}</strong>
            <span v-if="issuerHost">Signed in via {{ issuerHost }}</span>
            <span v-else-if="!authState.displayName">Signed in</span>
        </div>
        <div class="menu-separator" role="separator" />
        <button v-if="authState.status === 'signed-in'" type="button" class="menu-item" role="menuitem" @click="signOut">
            <i class="fa-solid fa-arrow-right-from-bracket" aria-hidden="true" />
            Sign out
        </button>
        <button v-else-if="authState.status === 'ended'" type="button" class="menu-item" role="menuitem" @click="signIn">
            <i class="fa-solid fa-right-to-bracket" aria-hidden="true" />
            Sign in again
        </button>
    </DropdownMenu>
</template>

<script lang="ts" setup>
import { computed } from 'vue';

import { authState, signIn, signOut } from '@/auth';

import DropdownMenu from './dropdown-menu.vue';

const issuerHost = computed(() => {
    try {
        return authState.issuer ? new URL(authState.issuer).host : null;
    } catch {
        return null;
    }
});
</script>
