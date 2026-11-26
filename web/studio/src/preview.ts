// Canonical preview: the HTML comes from POST /api/preview (the production
// renderer, byte for byte the reader's lesson markup, allowlisted and escaped
// by the server). It is placed in the page, styled by the reader's own
// stylesheets (scoped at build time, see vite.config.ts) and its formulas are
// typeset with the KaTeX the application serves, with the reader's options
// (internal/rendering/static/math.js). Same origin only: no iframe, no
// external request. The server names KaTeX's fingerprinted, cacheable URLs
// in index.html (<meta name="learnoo-katex-js|css">); the unversioned
// /static/ URLs (not cached) are the fallback, for the development server.

interface Katex {
  render(source: string, element: HTMLElement, options: Record<string, unknown>): void;
}

declare global {
  interface Window {
    katex?: Katex;
  }
}

/** The URL the server gives for an asset in a meta element, if any and same-origin path. */
function assetURL(name: string, fallback: string): string {
  const url = document.querySelector<HTMLMetaElement>(`meta[name="${name}"]`)?.content ?? '';
  return url.startsWith('/') && !url.startsWith('//') ? url : fallback;
}

let loading: Promise<Katex | null> | null = null;

/** Loads the application's KaTeX once; resolves null (formulas left as source) if unavailable. */
export function loadKatex(): Promise<Katex | null> {
  if (window.katex) return Promise.resolve(window.katex);
  loading ??= new Promise((resolve) => {
    const js = assetURL('learnoo-katex-js', '/static/vendor/katex/katex.min.js');
    const css = assetURL('learnoo-katex-css', '/static/vendor/katex/katex.min.css');
    if (!document.querySelector(`link[href="${css}"]`)) {
      const link = document.createElement('link');
      link.rel = 'stylesheet';
      link.href = css;
      document.head.append(link);
    }
    const script = document.createElement('script');
    script.src = js;
    script.onload = () => resolve(window.katex ?? null);
    script.onerror = () => {
      loading = null;
      resolve(null);
    };
    document.head.append(script);
  });
  return loading;
}

/** Typesets every .math element like the reader; returns the number of formulas that could not be rendered. */
export function typeset(root: HTMLElement, katex: Katex | null): number {
  let errors = 0;
  for (const node of root.querySelectorAll<HTMLElement>('.math')) {
    const source = node.textContent ?? '';
    try {
      if (!katex) throw new Error('Math assets unavailable');
      katex.render(source, node, { displayMode: node.dataset.display === 'true', output: 'htmlAndMathml', trust: false, strict: 'error', throwOnError: true, maxExpand: 200, maxSize: 20, macros: {} });
      node.dataset.rendered = 'true';
    } catch {
      node.textContent = source;
      node.classList.add('math-error');
      node.setAttribute('aria-label', `Formule non rendue : ${source}`);
      errors++;
    }
  }
  return errors;
}

/**
 * The reader's click-to-load video button would embed YouTube, which the
 * Studio's policy does not allow (no frames, no external requests). In the
 * preview it is disabled and says so.
 */
export function disableVideos(root: HTMLElement) {
  for (const button of root.querySelectorAll<HTMLButtonElement>('.video-load')) {
    button.disabled = true;
    button.title = 'Les vidéos ne sont pas chargées dans l’aperçu.';
    const note = document.createElement('p');
    note.className = 'preview-video-note';
    note.textContent = 'Aperçu : la vidéo n’est pas chargée dans le Studio ; l’élève pourra la lancer depuis la leçon publiée.';
    button.after(note);
  }
}
