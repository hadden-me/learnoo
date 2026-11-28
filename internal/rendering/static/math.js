(() => {
  const start = performance.now();
  let errors = 0;
  const nodes = document.querySelectorAll('.math');
  for (const node of nodes) {
    const source = node.textContent;
    try {
      if (!window.katex) throw new Error('Math assets unavailable');
      katex.render(source, node, { displayMode: node.dataset.display === 'true', output: 'htmlAndMathml', trust: false, strict: 'error', throwOnError: true, maxExpand: 200, maxSize: 20, macros: {} });
      node.dataset.rendered = 'true';
    } catch {
      node.textContent = source;
      node.classList.add('math-error');
      node.setAttribute('aria-label', `Formule non rendue : ${source}`);
      errors++;
    }
  }
  window.learnooMetrics = { mathMs: performance.now() - start, formulaCount: nodes.length, errors };
  for (const button of document.querySelectorAll('.video-load')) {
    button.addEventListener('click', () => {
      const id = button.dataset.videoId;
      if (!/^[A-Za-z0-9_-]{11}$/.test(id)) return;
      const iframe = document.createElement('iframe');
      iframe.src = `https://www.youtube-nocookie.com/embed/${id}`;
      iframe.title = 'Vidéo pédagogique YouTube';
      iframe.referrerPolicy = 'strict-origin-when-cross-origin';
      iframe.allow = 'fullscreen';
      // Replace the whole control (button and its notice), then keep
      // keyboard focus on the player that took the button's place.
      // A print-only copy of the link, since print hides the player.
      const link = document.createElement('p');
      link.className = 'video-print-link';
      link.textContent = `https://youtu.be/${id}`;
      (button.closest('.video-control') || button).replaceWith(iframe, link);
      iframe.focus();
    }, { once: true });
  }
})();
