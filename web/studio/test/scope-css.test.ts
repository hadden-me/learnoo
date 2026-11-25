import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { scopeReaderCss } from '../tools/scope-css';

describe('scopeReaderCss', () => {
  it('nests rules and @media blocks, maps body/main to the scope and drops :root', () => {
    const out = scopeReaderCss(':root { color-scheme: light; }\nbody { margin: 0; }\nh1 { font-size: 2rem; }\n@media (max-width: 40rem) {\n  main { padding: 0; }\n  .formula { padding: 1px; }\n}\n.reader main { margin: 0; }', '.lesson-preview');
    expect(out.startsWith('.lesson-preview {')).toBe(true);
    expect(out).not.toContain(':root');
    expect(out).toContain('& { margin: 0; }');
    expect(out).toContain('h1 { font-size: 2rem; }');
    expect(out).toMatch(/@media \(max-width: 40rem\) \{\s*& \{ padding: 0; \}/);
    // Only whole selectors are mapped; a descendant `main` stays a descendant.
    expect(out).toContain('.reader main { margin: 0; }');
  });

  it('fails on at-rules it cannot scope instead of leaking them', () => {
    expect(() => scopeReaderCss("@font-face { font-family: X; src: url(x.woff2); }", '.p')).toThrow(/@font-face/);
    expect(() => scopeReaderCss('@keyframes spin { to { rotate: 1turn; } }', '.p')).toThrow(/@keyframes/);
    expect(() => scopeReaderCss("@import url('x.css');", '.p')).toThrow(/@import/);
  });

  it('fails on :root or html selectors other than a plain :root rule', () => {
    expect(() => scopeReaderCss(':root, body { color: red; }', '.p')).toThrow(/:root or html/);
    expect(() => scopeReaderCss('html { font-size: 100%; }', '.p')).toThrow(/:root or html/);
    expect(() => scopeReaderCss('html[dir=rtl] p { margin: 0; }', '.p')).toThrow(/:root or html/);
  });

  it('accepts the reader stylesheets as they are today', () => {
    const css = ['../../internal/rendering/static/site.css', '../../internal/reader/static/reader.css'].map((f) => readFileSync(resolve(f), 'utf8')).join('\n');
    const out = scopeReaderCss(css, '.lesson-preview');
    expect(out).toContain('.math-error');
    expect(out).not.toMatch(/(^|\n)\s*body\s*\{/);
  });
});
