import { act, createElement, useEffect } from 'react';
import { createRoot } from 'react-dom/client';
import { describe, expect, it } from 'vitest';
import { useLoad, type Loaded } from '../src/editorial/common';

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}

describe('useLoad', () => {
  it('ignores a slower response to an older request', async () => {
    const pending = new Map<string, ReturnType<typeof deferred<string>>>([
      ['open', deferred<string>()],
      ['all', deferred<string>()],
    ]);
    const seen: Loaded<string>[] = [];
    function Probe({ filter }: { filter: string }) {
      const [value] = useLoad(() => pending.get(filter)!.promise, [filter]);
      useEffect(() => {
        seen.push(value);
      }, [value]);
      return null;
    }
    const root = createRoot(document.createElement('div'));
    await act(async () => root.render(createElement(Probe, { filter: 'open' })));
    await act(async () => root.render(createElement(Probe, { filter: 'all' })));
    await act(async () => pending.get('all')!.resolve('tous'));
    await act(async () => pending.get('open')!.resolve('ouverts'));
    expect(seen.at(-1)).toEqual({ state: 'ready', data: 'tous' });
    expect(seen.some((v) => v.state === 'ready' && v.data === 'ouverts')).toBe(false);
    await act(async () => root.unmount());
  });
});
