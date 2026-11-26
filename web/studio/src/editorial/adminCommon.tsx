// Shared parts of the administration pages (LMS-B10): the administration
// sub-navigation, the administration calls bound to the shell's client,
// load-failure and result banners.
import { useMemo, type ReactNode } from 'react';
import { adminApi, describeAdminError } from '../api/admin';
import { useStudio } from '../context';
import { paths } from '../editorialRoutes';
import { useRoute } from '../router';
import { Link } from '../ui/Link';

export function useAdmin() {
  const { api } = useStudio();
  return useMemo(() => adminApi(api), [api]);
}

const SECTIONS: { label: string; to: string; routes: string[] }[] = [
  { label: 'Leçons', to: paths.adminLessons, routes: ['adminLessons'] },
  { label: 'Cours', to: paths.adminCourses, routes: ['adminCourses', 'adminCourse'] },
  { label: 'Utilisateurs', to: paths.adminUsers, routes: ['adminUsers', 'adminUser'] },
  { label: 'Catalogue', to: paths.adminCatalogue, routes: ['adminCatalogue'] },
  { label: 'Programmes', to: paths.adminCurriculum, routes: ['adminCurriculum', 'adminSection', 'adminEdition'] },
];

/** The administration sub-navigation, shown on every administration page. */
export function AdminNav() {
  const route = useRoute();
  return (
    <nav aria-label="Administration" className="admin-nav">
      {SECTIONS.map((s) => (
        <Link key={s.to} to={s.to} current={s.routes.includes(route.name)}>
          {s.label}
        </Link>
      ))}
    </nav>
  );
}

/** A failed load, with its explanation and a retry button. */
export function LoadFailed({ message, onRetry }: { message: string; onRetry: () => void }) {
  return (
    <div className="banner error" role="alert">
      <p>{message}</p>
      <button type="button" onClick={onRetry}>
        Réessayer
      </button>
    </div>
  );
}

/** The page frame of an administration page: sub-navigation, then the page. */
export function AdminPage({ title, children, intro }: { title: string; intro?: ReactNode; children: ReactNode }) {
  return (
    <main className="page" id="main">
      <AdminNav />
      <h1 tabIndex={-1}>{title}</h1>
      {intro}
      {children}
    </main>
  );
}

export type Outcome = { kind: 'success' | 'error'; message: string } | null;

/** The result of the last action: a success (status) or an error (alert). */
export function OutcomeBanner({ outcome, onReload }: { outcome: Outcome; onReload?: () => void }) {
  if (!outcome) return null;
  return outcome.kind === 'success' ? (
    <p className="banner success" role="status">
      {outcome.message}
    </p>
  ) : (
    <div className="banner error" role="alert">
      <p>{outcome.message}</p>
      {onReload && (
        <button type="button" onClick={onReload}>
          Recharger
        </button>
      )}
    </div>
  );
}

export const failure = (e: unknown): Outcome => ({ kind: 'error', message: describeAdminError(e) });
