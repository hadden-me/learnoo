// Client routes under /studio/ (History API, no dependency). The Go server
// answers every /studio path whose last segment has no dot with index.html,
// so routes must never contain a dot in their last segment (API.md).
// Reviewer, publisher and administrator spaces are added here as new route
// names by the next slice; the shell shows only those the account can use.
import { useEffect, useState } from 'react';

export const BASE = '/studio';

export type Route =
  | { name: 'lessons' }
  | { name: 'new' }
  | { name: 'edit'; lessonId: number }
  | { name: 'notFound' };

export function parseRoute(pathname: string): Route {
  const path = pathname.replace(/\/+$/, '');
  if (path === BASE) return { name: 'lessons' };
  if (path === `${BASE}/lessons/new`) return { name: 'new' };
  const edit = /^\/studio\/lessons\/([1-9][0-9]{0,17})\/edit$/.exec(path);
  if (edit) return { name: 'edit', lessonId: Number(edit[1]) };
  return { name: 'notFound' };
}

export function routePath(route: Route): string {
  switch (route.name) {
    case 'lessons':
      return `${BASE}/`;
    case 'new':
      return `${BASE}/lessons/new`;
    case 'edit':
      return `${BASE}/lessons/${route.lessonId}/edit`;
    case 'notFound':
      return `${BASE}/`;
  }
}

const listeners = new Set<() => void>();
/** Called before any page change is rendered (the shell invalidates the account check there). */
const beforeNavigate = new Set<() => void>();
const notify = () => {
  beforeNavigate.forEach((l) => l());
  listeners.forEach((l) => l());
};

export function onNavigate(fn: () => void): () => void {
  beforeNavigate.add(fn);
  return () => beforeNavigate.delete(fn);
}

/** Navigation guards: each returns true when leaving must first be confirmed (unsaved edits). */
type Guard = () => boolean;
const guards = new Set<Guard>();
let confirmLeave: (proceed: () => void) => void = (proceed) => proceed();

export function addNavigationGuard(guard: Guard): () => void {
  guards.add(guard);
  return () => guards.delete(guard);
}

/** The shell installs the dialog that asks before leaving unsaved edits. */
export function setLeaveConfirmation(fn: (proceed: () => void) => void) {
  confirmLeave = fn;
}

export const hasBlockingGuard = () => [...guards].some((g) => g());

export function navigate(path: string, options: { replace?: boolean; force?: boolean } = {}) {
  const go = () => {
    if (options.replace) history.replaceState(null, '', path);
    else history.pushState(null, '', path);
    shown = path;
    notify();
    // Focus the new page's heading so screen readers announce the change.
    requestAnimationFrame(() => document.querySelector<HTMLElement>('main h1')?.focus());
  };
  if (!options.force && hasBlockingGuard()) confirmLeave(go);
  else go();
}

let shown = typeof location === 'undefined' ? '/' : location.pathname;

if (typeof window !== 'undefined') {
  window.addEventListener('popstate', () => {
    const target = location.pathname;
    if (target !== shown && hasBlockingGuard()) {
      // Undo the browser's move, then ask; the edits stay on screen either way.
      history.pushState(null, '', shown);
      confirmLeave(() => {
        history.pushState(null, '', target);
        shown = target;
        notify();
      });
      return;
    }
    shown = target;
    notify();
  });
}

export function useRoute(): Route {
  const [path, setPath] = useState(() => location.pathname);
  useEffect(() => {
    const listener = () => setPath(location.pathname);
    listeners.add(listener);
    return () => {
      listeners.delete(listener);
    };
  }, []);
  return parseRoute(path);
}

/**
 * Changes the address without changing the page (no re-render): used when
 * the first save creates a lesson, so the open editor, its caret and any
 * text typed during the save stay as they are, while a reload or a shared
 * link opens the lesson's own address.
 */
export function replacePath(path: string) {
  history.replaceState(null, '', path);
  shown = path;
}
