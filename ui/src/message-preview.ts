/**
 * Builds the document shown in the sandboxed preview iframe from untrusted email HTML.
 *
 * The HTML is parsed with `DOMParser` (an inert document: scripts never run and resources never load),
 * sanitized, stripped of every non-`data:` URL, wrapped with a restrictive CSP `<meta>`, serialized and
 * re-parsed for verification. The result is meant only for `<iframe sandbox="" srcdoc>`; it must never be
 * inserted into the console DOM.
 */

export const PREVIEW_CSP_BLOCKED =
    "default-src 'none'; img-src data:; style-src 'unsafe-inline'; font-src data:; media-src data:; base-uri 'none'; form-action 'none'";

export const MAX_PREVIEW_HTML_LENGTH = 2_000_000;

const REMOVED_ELEMENTS = new Set([
    'script',
    'iframe',
    'frame',
    'frameset',
    'object',
    'embed',
    'applet',
    'portal',
    'base',
    'link',
    'meta',
    'template',
    // SVG animation can rewrite URL attributes after the sanitizer has run.
    'animate',
    'set'
]);

const REMOVED_ATTRIBUTES = new Set(['ping', 'action', 'formaction', 'srcdoc', 'nonce', 'integrity']);
const URL_ATTRIBUTES = new Set(['src', 'srcset', 'background', 'poster', 'data', 'href', 'xlink:href', 'lowsrc', 'dynsrc', 'longdesc']);

const PREVIEW_STYLE =
    ':root{color-scheme:light}body{margin:16px;background:#fff;color:#111;font-family:system-ui,-apple-system,"Segoe UI",sans-serif;overflow-wrap:anywhere}';

export function normalizeContentId(raw: string): string {
    const unbracket = (value: string) => {
        const trimmed = value.trim();
        return trimmed.startsWith('<') && trimmed.endsWith('>') ? trimmed.slice(1, -1) : trimmed;
    };
    let id = unbracket(raw);
    try {
        id = decodeURIComponent(id);
    } catch {
        // keep the undecoded value
    }
    return unbracket(id).toLowerCase();
}

/** Trims only what the URL parser strips (C0 controls and space), unlike String#trim which also strips Unicode spaces. */
function urlTrim(value: string): string {
    // eslint-disable-next-line no-control-regex
    return value.replace(/^[\u0000-\u0020]+|[\u0000-\u0020]+$/g, '');
}

const SVG_RESOURCE_ELEMENTS = new Set(['image', 'use', 'feimage', 'pattern', 'lineargradient', 'radialgradient', 'filter', 'textpath', 'mpath']);

function isDataUrl(value: string): boolean {
    return /^data:/i.test(urlTrim(value));
}

function isCidUrl(value: string): boolean {
    return /^cid:/i.test(urlTrim(value));
}

function cidOf(value: string): string {
    return normalizeContentId(urlTrim(value).slice(4));
}

function parse(html: string): Document {
    return new DOMParser().parseFromString(html, 'text/html');
}

function allElements(doc: Document): Element[] {
    return Array.from(doc.querySelectorAll('*'));
}

/** Normalized content IDs referenced as `cid:` by `<img src>` or any `background` attribute. */
export function referencedContentIds(html: string): string[] {
    if (html.length > MAX_PREVIEW_HTML_LENGTH) return [];
    const ids = new Set<string>();
    for (const el of allElements(parse(html))) {
        const isImg = el.localName === 'img';
        for (const [name, value] of attributesOf(el)) {
            if (((isImg && name === 'src') || name === 'background') && isCidUrl(value)) ids.add(cidOf(value));
        }
    }
    return [...ids];
}

function attributesOf(el: Element): Array<[string, string]> {
    return el.getAttributeNames().map(name => [name.toLowerCase(), el.getAttribute(name) ?? '']);
}

/** Accepts only a single `data:` candidate with an optional descriptor; anything with a second candidate is rejected. */
function srcsetIsAllData(value: string): boolean {
    return /^data:[^\s]*(\s+[0-9.]+[wx])?$/i.test(urlTrim(value));
}

function sanitize(doc: Document, inlineImages: ReadonlyMap<string, string>): { blocked: number; unresolved: number } {
    let blocked = 0;
    let unresolved = 0;

    for (const el of allElements(doc)) {
        if (REMOVED_ELEMENTS.has(el.localName.toLowerCase())) el.remove();
    }

    for (const el of allElements(doc)) {
        const tag = el.localName.toLowerCase();
        const isAnchor = tag === 'a' || tag === 'area';
        for (const original of el.getAttributeNames()) {
            const name = original.toLowerCase();
            const value = el.getAttribute(original) ?? '';
            if (/^on/i.test(name) || REMOVED_ATTRIBUTES.has(name) || (name === 'manifest' && tag === 'html')) {
                el.removeAttribute(original);
                continue;
            }
            if (isAnchor && (name === 'href' || name === 'xlink:href')) {
                if (!el.hasAttribute('title')) el.setAttribute('title', value);
                el.removeAttribute(original);
                continue;
            }
            // href on anything other than an SVG resource element (e.g. MathML <mi href>) would be a navigable link.
            if (!isAnchor && (name === 'href' || name === 'xlink:href') && !SVG_RESOURCE_ELEMENTS.has(tag)) {
                el.removeAttribute(original);
                continue;
            }
            if (isAnchor && name === 'target') {
                el.removeAttribute(original);
                continue;
            }
            if (!URL_ATTRIBUTES.has(name)) continue;
            if (name === 'srcset') {
                if (!srcsetIsAllData(value)) {
                    el.removeAttribute(original);
                    blocked++;
                }
                continue;
            }
            if (isCidUrl(value) && ((tag === 'img' && name === 'src') || name === 'background')) {
                const resolved = inlineImages.get(cidOf(value));
                if (resolved && isDataUrl(resolved)) {
                    el.setAttribute(original, resolved);
                } else {
                    el.removeAttribute(original);
                    unresolved++;
                }
                continue;
            }
            if (isDataUrl(value)) continue;
            el.removeAttribute(original);
            blocked++;
        }
    }
    return { blocked, unresolved };
}

function injectHead(doc: Document) {
    const head = doc.head;
    const make = (tag: string, attrs: Record<string, string>, text?: string) => {
        const el = doc.createElement(tag);
        for (const [key, value] of Object.entries(attrs)) el.setAttribute(key, value);
        if (text) el.textContent = text;
        return el;
    };
    const injected = [
        make('meta', { 'http-equiv': 'Content-Security-Policy', content: PREVIEW_CSP_BLOCKED }),
        make('meta', { name: 'referrer', content: 'no-referrer' }),
        make('meta', { 'http-equiv': 'x-dns-prefetch-control', content: 'off' }),
        make('meta', { name: 'color-scheme', content: 'light' }),
        make('style', {}, PREVIEW_STYLE)
    ];
    for (const el of injected.reverse()) head.insertBefore(el, head.firstChild);
}

/**
 * Re-parses the final string, which is what the iframe will parse, and checks the guarantees the sanitizer
 * is meant to provide. This defeats parser-differential (mutation) tricks.
 */
export function verifyPreviewDocument(srcdoc: string): boolean {
    const doc = parse(srcdoc);
    const metas = Array.from(doc.querySelectorAll('meta'));
    if (metas.length !== 4) return false;
    if (!metas.every(meta => meta.parentElement === doc.head)) return false;
    const first = doc.head.firstElementChild;
    if (first !== metas[0] || first?.getAttribute('http-equiv') !== 'Content-Security-Policy' || first.getAttribute('content') !== PREVIEW_CSP_BLOCKED) {
        return false;
    }
    for (const el of allElements(doc)) {
        const tag = el.localName.toLowerCase();
        if (tag !== 'meta' && REMOVED_ELEMENTS.has(tag)) return false;
        for (const [name, value] of attributesOf(el)) {
            if (/^on/i.test(name)) return false;
            if (REMOVED_ATTRIBUTES.has(name)) return false;
            if ((tag === 'a' || tag === 'area') && (name === 'href' || name === 'xlink:href')) return false;
            if (URL_ATTRIBUTES.has(name)) {
                if (name === 'srcset' ? !srcsetIsAllData(value) : !isDataUrl(value)) return false;
            }
        }
    }
    return true;
}

export type PreviewResult =
    | { ok: true; srcdoc: string; blockedResources: number; unresolvedInline: number }
    | { ok: false; reason: 'too-large' | 'unsafe' };

export function buildPreviewDocument(
    html: string,
    options: { inlineImages: ReadonlyMap<string, string>; /** Test hook; defaults to the real verifier. */ verify?: (srcdoc: string) => boolean }
): PreviewResult {
    if (html.length > MAX_PREVIEW_HTML_LENGTH) return { ok: false, reason: 'too-large' };
    try {
        const doc = parse(html);
        const { blocked, unresolved } = sanitize(doc, options.inlineImages);
        injectHead(doc);
        const srcdoc = '<!doctype html>' + doc.documentElement.outerHTML;
        if (!(options.verify ?? verifyPreviewDocument)(srcdoc)) return { ok: false, reason: 'unsafe' };
        return { ok: true, srcdoc, blockedResources: blocked, unresolvedInline: unresolved };
    } catch {
        return { ok: false, reason: 'unsafe' };
    }
}
