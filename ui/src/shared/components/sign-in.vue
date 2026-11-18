<template>
    <main id="sign-in" class="auth-page" aria-labelledby="sign-in-title">
        <div class="theme-corner"><ThemeMenu /></div>
        <section class="card auth-card">
            <div class="auth-mark" aria-hidden="true"><i class="fa-solid fa-paper-plane" /></div>
            <h1 id="sign-in-title">{{ title }}</h1>

            <p v-if="authState.notice" class="notice" :class="noticeClass" role="alert">
                <i :class="noticeIcon" aria-hidden="true" />
                <span class="notice-body">{{ authState.notice }}</span>
            </p>

            <template v-if="canSignIn">
                <p class="subtitle">
                    Sign-in provider<span v-if="issuerHost"> · <strong>{{ issuerHost }}</strong></span>
                </p>
                <button type="button" class="btn primary lg" :disabled="authState.status === 'redirecting'" @click="signIn">
                    <i class="fa-solid fa-right-to-bracket" aria-hidden="true" />
                    {{ authState.status === 'redirecting' ? 'Redirecting…' : 'Sign in' }}
                </button>
            </template>

            <template v-else>
                <p class="subtitle">
                    The console can't load until Thinpost is reachable and sign-in is set up. Your Thinpost administrator can help if this keeps happening.
                </p>
                <button type="button" class="btn lg" :disabled="isRetrying" @click="retry">
                    <i class="fa-solid fa-rotate" aria-hidden="true" />
                    {{ isRetrying ? 'Checking…' : 'Check again' }}
                </button>
            </template>
        </section>
    </main>
</template>

<script lang="ts" setup>
import { computed, ref } from 'vue';

import { authState, initAuth, signIn } from '@/auth';

import ThemeMenu from './theme-menu.vue';

const isRetrying = ref(false);

const canSignIn = computed(() => authState.status === 'signed-out' || authState.status === 'redirecting');

const title = computed(() => {
    switch (authState.status) {
        case 'unreachable':
            return 'Thinpost is unavailable';
        case 'unconfigured':
            return 'Sign-in isn\'t set up';
        default:
            return 'Sign in to Thinpost';
    }
});

const noticeClass = computed(() => (canSignIn.value ? 'info' : 'warning'));
const noticeIcon = computed(() => (canSignIn.value ? 'fa-solid fa-circle-info' : 'fa-solid fa-triangle-exclamation'));

const issuerHost = computed(() => {
    try {
        return authState.issuer ? new URL(authState.issuer).host : null;
    } catch {
        return null;
    }
});

async function retry() {
    isRetrying.value = true;
    try {
        await initAuth();
    } finally {
        isRetrying.value = false;
    }
}
</script>
