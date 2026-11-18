<template>
    <div id="thinpost" class="app-root">
        <main v-if="authState.status === 'initializing'" class="auth-page">
            <Loader label="Loading Thinpost" />
        </main>

        <SignIn v-else-if="!hasShell" />

        <Layout v-else>
            <RouterView />
        </Layout>

        <OverlayContainer />
    </div>
</template>

<script lang="ts" setup>
import { OverlayContainer } from '@zyno-io/vue-foundation';
import { computed } from 'vue';

import { authState } from './auth';
import Layout from './shared/components/layout.vue';
import Loader from './shared/components/loader.vue';
import SignIn from './shared/components/sign-in.vue';

// Keep the shell mounted when a session ends so one-time secrets on screen are not torn down.
const hasShell = computed(() => authState.status === 'signed-in' || authState.status === 'ended');
</script>

<style lang="scss" scoped>
.app-root {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-width: 0;
}
</style>
