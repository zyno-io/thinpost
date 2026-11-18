import '@fontsource/inter/latin-400.css';
import '@fontsource/inter/latin-500.css';
import '@fontsource/inter/latin-600.css';
import '@fontsource/inter/latin-700.css';
import '@fortawesome/fontawesome-free/css/all.css';
import '@zyno-io/vue-foundation/dist/vue-foundation.css';
import './shared/styles/thinpost.scss';
import './theme';
import './openapi-client';
import { createApp } from 'vue';

import App from './app.vue';
import { initAuth } from './auth';
import router from './router';
import { setupVf } from './vf.setup';

// Started before the router is installed so a pending OIDC callback URL is captured intact.
const authReady = initAuth();

const app = createApp(App);
setupVf(app);
app.use(router);
app.mount('#app');

authReady.then(returnTo => {
    if (returnTo) router.replace(returnTo);
});
