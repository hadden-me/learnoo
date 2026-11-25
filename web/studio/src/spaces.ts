// Which Studio spaces the signed-in account can use, discovered from the
// server: each space has a read endpoint whose permission rule is the
// space's own, and a 403 means the account lacks it. This only decides what
// the interface offers; the server enforces every rule on every request,
// and a permission revoked later is refused there whatever the interface
// shows.
import { isApiError, type Api } from './api/client';

export interface Spaces {
  /** GET /api/lessons/mine: `author`. */
  author: boolean;
  /** GET /api/review/queue: a reviewer assignment for at least one subject. */
  reviewer: boolean;
  /** GET /api/publish/queue: `publisher`. */
  publisher: boolean;
  /** GET /api/admin/lessons: `administrator`. */
  administrator: boolean;
}

export const NO_SPACES: Spaces = { author: false, reviewer: false, publisher: false, administrator: false };

const PROBES: Record<keyof Spaces, string> = {
  author: '/api/lessons/mine',
  reviewer: '/api/review/queue',
  publisher: '/api/publish/queue',
  administrator: '/api/admin/lessons',
};

/** Probes every space in parallel. `forbidden` means "not this space"; any other failure is thrown. */
export async function discoverSpaces(api: Api): Promise<Spaces> {
  const keys = Object.keys(PROBES) as (keyof Spaces)[];
  const results = await Promise.all(
    keys.map((key) =>
      api.request('GET', PROBES[key]).then(
        () => true,
        (error) => {
          if (isApiError(error, 'forbidden')) return false;
          throw error;
        },
      ),
    ),
  );
  return Object.fromEntries(keys.map((key, i) => [key, results[i]])) as unknown as Spaces;
}

/** Error reports are read by reviewers (their subjects) and administrators (all): the server's rule. */
export const canHandleReports = (s: Spaces) => s.reviewer || s.administrator;

/** Restoration is allowed to publishers and administrators; withdrawal and unpublication to administrators. */
export const canSeePublication = (s: Spaces) => s.publisher || s.administrator;

export const hasAnySpace = (s: Spaces) => s.author || s.reviewer || s.publisher || s.administrator;
