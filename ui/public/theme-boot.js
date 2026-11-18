// Applies the stored theme before first paint. Loaded as an external classic script so the
// console works under a strict Content-Security-Policy (script-src 'self').
(function () {
    var stored = null;
    try {
        stored = localStorage.getItem('thinpost:theme');
    } catch (e) {}
    var mode = stored === 'light' || stored === 'dark' ? stored : 'system';
    var isDark = mode === 'dark' || (mode === 'system' && window.matchMedia('(prefers-color-scheme: dark)').matches);
    if (isDark) document.documentElement.classList.add('dark');
})();
