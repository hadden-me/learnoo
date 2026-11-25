// Nests the reader's page-wide stylesheets under one selector for the Studio
// preview (see vite.config.ts). Only constructs whose nesting is understood
// are accepted; anything else fails the build rather than leaking styles into
// the Studio or silently changing the preview:
// - `@media` blocks are nested as they are;
// - `body` and `main` rules apply to the scope element itself (`&`);
// - plain `:root { … }` rules are dropped (page-level settings such as
//   color-scheme);
// - other at-rules (`@font-face`, `@keyframes`, `@import`, `@layer`…), and
//   `:root` or `html` anywhere else, throw.

const ALLOWED_AT_RULES = new Set(['media']);

export function scopeReaderCss(css: string, scope: string): string {
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  for (const match of source.matchAll(/@([A-Za-z-]+)/g)) {
    if (!ALLOWED_AT_RULES.has(match[1].toLowerCase())) throw new Error(`scopeReaderCss: unsupported at-rule @${match[1]} in the reader stylesheet; extend tools/scope-css.ts deliberately`);
  }
  const rules = source
    .replace(/(^|[\n}])\s*:root\s*\{[^}]*\}/g, '$1')
    .replace(/(^|[\n,{}])(\s*)(body|main)(\s*[{,])/g, '$1$2&$4');
  if (/(^|[^\w-]):root\b/.test(rules) || /(^|[\s,{}>+~(])html(?![\w-])/.test(rules)) {
    throw new Error('scopeReaderCss: :root or html used in a selector the preview cannot scope; extend tools/scope-css.ts deliberately');
  }
  return `${scope} {\n${rules}\n}\n`;
}
