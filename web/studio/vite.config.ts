import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { defineConfig, type Plugin } from 'vitest/config';
import react from '@vitejs/plugin-react';
import { scopeReaderCss } from './tools/scope-css';

// The Studio's preview shows the server's canonical lesson HTML with the
// reader's own stylesheets. Those are page-wide (body, main, h1, button…),
// so they are nested under .lesson-preview at build time instead of being
// copied: the preview cannot drift from the reader's styles, and they cannot
// restyle the Studio. `body` and `main` rules apply to the preview container
// itself; `:root` rules are dropped.
const READER_CSS = ['../../internal/rendering/static/site.css', '../../internal/reader/static/reader.css'].map((p) => fileURLToPath(new URL(p, import.meta.url)));
const VIRTUAL = 'virtual:reader-lesson.css';

function readerLessonStyles(): Plugin {
  return {
    name: 'learnoo-reader-lesson-css',
    resolveId: (id) => (id === VIRTUAL ? `\0${VIRTUAL}` : null),
    load(id) {
      if (id !== `\0${VIRTUAL}`) return null;
      for (const file of READER_CSS) this.addWatchFile(file);
      return scopeReaderCss(READER_CSS.map((f) => readFileSync(f, 'utf8')).join('\n'), '.lesson-preview');
    },
  };
}

// Development: `npm run dev` proxies the application's API, sign-in and
// static assets to a running Go server (docs/DEVELOPMENT.md), keeping the
// browser's Host so the server's same-origin checks pass.
const app = process.env.LEARNOO_APP_URL ?? 'http://127.0.0.1:8080';
const port = Number(process.env.LEARNOO_STUDIO_PORT ?? 5173);
const proxy = Object.fromEntries(['/api', '/static', '/login', '/logout'].map((p) => [p, { target: app, changeOrigin: false }]));

export default defineConfig({
  // The application server serves the built bundle under /studio/ (docs/DEVELOPMENT.md).
  base: '/studio/',
  plugins: [react(), readerLessonStyles()],
  server: { host: '127.0.0.1', port, strictPort: true, proxy, fs: { allow: ['.', '../design', '../../internal/rendering', '../../internal/reader/static'] } },
  test: { environment: 'jsdom', include: ['test/**/*.test.ts'], setupFiles: ['test/setup.ts'] },
});
