import type { MouseEvent, ReactNode } from 'react';
import { navigate } from '../router';

/** In-app link: a real href (opens in a new tab with modifiers), client navigation otherwise. */
export function Link({ to, children, current, className }: { to: string; children: ReactNode; current?: boolean; className?: string }) {
  const onClick = (e: MouseEvent<HTMLAnchorElement>) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    navigate(to);
  };
  return (
    <a href={to} onClick={onClick} aria-current={current ? 'page' : undefined} className={className}>
      {children}
    </a>
  );
}
