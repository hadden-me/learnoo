import { afterEach, describe, expect, it, vi } from 'vitest';

// KaTeX for the preview comes from the fingerprinted URLs the server names in
// index.html, falling back to the unversioned /static/ ones.
async function loadedURLs() {
  vi.resetModules();
  const { loadKatex } = await import('../src/preview');
  void loadKatex();
  return {
    css: document.head.querySelector<HTMLLinkElement>('link[rel="stylesheet"]')?.getAttribute('href'),
    js: document.head.querySelector<HTMLScriptElement>('script')?.getAttribute('src'),
  };
}

describe('loadKatex', () => {
  afterEach(() => {
    document.head.replaceChildren();
  });

  it('uses the fingerprinted URLs from the page', async () => {
    for (const [name, content] of [['learnoo-katex-js', '/assets/0123456789abcdef/vendor/katex/katex.min.js'], ['learnoo-katex-css', '/assets/0123456789abcdef/vendor/katex/katex.min.css']]) {
      const meta = document.createElement('meta');
      meta.name = name;
      meta.content = content;
      document.head.append(meta);
    }
    expect(await loadedURLs()).toEqual({ css: '/assets/0123456789abcdef/vendor/katex/katex.min.css', js: '/assets/0123456789abcdef/vendor/katex/katex.min.js' });
  });

  it('falls back to /static without them, and refuses other origins', async () => {
    expect(await loadedURLs()).toEqual({ css: '/static/vendor/katex/katex.min.css', js: '/static/vendor/katex/katex.min.js' });
    document.head.replaceChildren();
    const meta = document.createElement('meta');
    meta.name = 'learnoo-katex-js';
    meta.content = '//evil.test/katex.js';
    document.head.append(meta);
    expect((await loadedURLs()).js).toBe('/static/vendor/katex/katex.min.js');
  });
});
