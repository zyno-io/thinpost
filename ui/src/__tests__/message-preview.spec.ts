import { describe, expect, it } from 'vitest';

import * as preview from '@/message-preview';
import { buildPreviewDocument, MAX_PREVIEW_HTML_LENGTH, normalizeContentId, PREVIEW_CSP_BLOCKED, referencedContentIds, verifyPreviewDocument } from '@/message-preview';

function build(html: string, inlineImages: Map<string, string> = new Map()) {
    const result = buildPreviewDocument(html, { inlineImages });
    if (!result.ok) throw new Error(`preview failed: ${result.reason}`);
    return { ...result, doc: new DOMParser().parseFromString(result.srcdoc, 'text/html') };
}

describe('buildPreviewDocument', () => {
    it('puts the blocking CSP meta first and exports no remote-content policy', () => {
        const { doc } = build('<p>hi</p>');
        const first = doc.head.firstElementChild!;
        expect(first.tagName).toBe('META');
        expect(first.getAttribute('http-equiv')).toBe('Content-Security-Policy');
        expect(first.getAttribute('content')).toBe(PREVIEW_CSP_BLOCKED);
        expect(Object.keys(preview).filter(key => key.startsWith('PREVIEW_CSP'))).toEqual(['PREVIEW_CSP_BLOCKED']);
    });

    it('removes active elements, every meta and event attributes', () => {
        const { doc, srcdoc } = build(
            '<html><head><meta http-equiv="refresh" content="0;url=https://beacon.test"><base href="https://evil.test/"><link rel="stylesheet" href="https://t.test/a.css"></head>' +
                '<body onload="x()"><script>fetch("https://beacon.test")</script><iframe src="https://x.test"></iframe><object data="https://x.test"></object>' +
                '<embed src="https://x.test"><template><b>t</b></template><meta http-equiv="refresh" content="0;url=https://beacon.test">' +
                '<img src="data:image/png;base64,AAAA" onerror="x()"><form action="https://x.test"><button formaction="https://y.test">go</button></form></body></html>'
        );
        for (const selector of ['script', 'iframe', 'object', 'embed', 'base', 'link', 'template', 'body meta']) {
            expect(doc.querySelector(selector), selector).toBeNull();
        }
        expect(doc.querySelectorAll('meta')).toHaveLength(4);
        expect(srcdoc).not.toMatch(/onerror|onload|formaction|beacon\.test|refresh/i);
        expect(doc.querySelector('form')?.hasAttribute('action')).toBe(false);
        expect(doc.querySelector('img')?.getAttribute('src')).toBe('data:image/png;base64,AAAA');
    });

    it('turns link hrefs into titles and removes targets', () => {
        const { doc } = build('<a href="https://track.test/x?u=1" target="_blank">go</a><svg><a href="https://svg.test/"><text>s</text></a><image href="https://t.test/i.png"/></svg>');
        const a = doc.querySelector('a')!;
        expect(a.hasAttribute('href')).toBe(false);
        expect(a.hasAttribute('target')).toBe(false);
        expect(a.getAttribute('title')).toBe('https://track.test/x?u=1');
        expect(doc.querySelectorAll('[href]')).toHaveLength(0);
    });

    it('removes and counts remote, protocol-relative, relative, javascript and blob URLs', () => {
        const result = build(
            '<img src="https://t.test/a.png"><img src="//t.test/b.png"><img src="/t/o/x"><img src="javascript:alert(1)"><img src="blob:https://x/1">' +
                '<table background="https://t.test/bg.png"><tr><td>x</td></tr></table><video poster="https://t.test/p.png"></video><img src="data:image/gif;base64,R0lG">'
        );
        expect(result.blockedResources).toBe(7);
        expect(result.doc.querySelectorAll('img[src]')).toHaveLength(1);
        expect(result.srcdoc).not.toContain('t.test');
    });

    it('keeps a data-only srcset and drops one mixing data and remote candidates', () => {
        const kept = build('<img srcset="data:image/png;base64,AAAA 1x">');
        expect(kept.doc.querySelector('img')?.hasAttribute('srcset')).toBe(true);
        const mixed = build('<img srcset="data:image/png;base64,AAAA 1x, https://t.test/b.png 2x">');
        expect(mixed.doc.querySelector('img')?.hasAttribute('srcset')).toBe(false);
        expect(mixed.blockedResources).toBe(1);
    });

    it('resolves cid images case-insensitively, with brackets and percent-encoding; counts unresolved ones', () => {
        const images = new Map([['logo@x', 'data:image/png;base64,AAAA']]);
        const resolved = build('<img src="CID:%3CLogo@X%3E"><img src="cid:logo@x"><img src="cid:missing">', images);
        const srcs = Array.from(resolved.doc.querySelectorAll('img')).map(img => img.getAttribute('src'));
        expect(srcs).toEqual(['data:image/png;base64,AAAA', 'data:image/png;base64,AAAA', null]);
        expect(resolved.unresolvedInline).toBe(1);
        expect(resolved.blockedResources).toBe(0);
    });

    it('yields one head for a full document input', () => {
        const { doc } = build('<!doctype html><html><head><title>T</title></head><body><p>hi</p></body></html>');
        expect(doc.querySelectorAll('head')).toHaveLength(1);
        expect(doc.title).toBe('T');
        expect(doc.body.textContent).toContain('hi');
    });

    it('reports oversize input and an input that fails verification', () => {
        expect(buildPreviewDocument('x'.repeat(MAX_PREVIEW_HTML_LENGTH + 1), { inlineImages: new Map() })).toEqual({ ok: false, reason: 'too-large' });
        expect(buildPreviewDocument('<p>x</p>', { inlineImages: new Map(), verify: () => false })).toEqual({ ok: false, reason: 'unsafe' });
    });
});

describe('verifyPreviewDocument', () => {
    const good = (body: string) => build(body).srcdoc;
    it('accepts a built document', () => {
        expect(verifyPreviewDocument(good('<p>ok</p>'))).toBe(true);
    });
    it('rejects scripts, handlers, remote URLs and a missing CSP meta', () => {
        expect(verifyPreviewDocument(good('<p>ok</p>').replace('<p>', '<script>1</script><p>'))).toBe(false);
        expect(verifyPreviewDocument(good('<p>ok</p>').replace('<p>', '<p onclick="x()">'))).toBe(false);
        expect(verifyPreviewDocument(good('<p>ok</p>').replace('<p>', '<img src="https://t.test/x.png"><p>'))).toBe(false);
        expect(verifyPreviewDocument(good('<p>ok</p>').replace(PREVIEW_CSP_BLOCKED, "default-src *"))).toBe(false);
        expect(verifyPreviewDocument('<!doctype html><html><head></head><body>x</body></html>')).toBe(false);
    });
});

describe('content ids', () => {
    it('normalizes', () => {
        expect(normalizeContentId(' <Logo%40X> ')).toBe('logo@x');
        expect(normalizeContentId('%E0%A4%A')).toBe('%e0%a4%a');
    });
    it('finds referenced cids', () => {
        expect(referencedContentIds('<img src="cid:A"><img src="CID:<b>"><div background="cid:c"></div><img src="https://x/y.png"><a href="cid:z">z</a>').sort()).toEqual([
            'a',
            'b',
            'c'
        ]);
    });
});

describe('URL edge cases', () => {
    it('does not treat Unicode-space-prefixed data: URLs as data URLs', () => {
        const result = build('<img src="\u00a0data:image/png;base64,AAAA"><img src="\ufeffdata:image/png;base64,AAAA">');
        expect(result.doc.querySelectorAll('img[src]').length).toBe(0);
        expect(result.blockedResources).toBe(2);
    });
    it('removes href from non-anchor, non-SVG-resource elements such as MathML', () => {
        const result = build('<math><mi href="data:text/html,x">x</mi></math><p href="data:text/html,y">y</p>');
        expect(result.doc.querySelector('[href]')).toBeNull();
    });
});
