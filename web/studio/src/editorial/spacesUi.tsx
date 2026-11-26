// The editorial spaces in the shell: navigation links, the page for an
// editorial route, and the Studio home of accounts that cannot author.
// Only spaces the server lets the account use are offered (spaces.ts).
import type { ReactNode } from 'react';
import type { EditorialRoute } from '../editorialRoutes';
import { paths } from '../editorialRoutes';
import type { Route } from '../router';
import { canHandleReports, type Spaces } from '../spaces';
import { Link } from '../ui/Link';
import { AdminLessonsPage, LessonPublicationPage } from './AdminPages';
import { AdminCoursePage, AdminCoursesPage } from './CourseAdminPages';
import { CoursePublishPage, CourseReviewPage } from './CourseReviewPages';
import { UserListPage, UserPage } from './UserPages';
import { CataloguePage } from './CataloguePage';
import { CurriculumPage, EditionPage, SectionPage } from './CurriculumPages';
import { NoAccess } from './common';
import { PublishQueuePage, PublishRevisionPage } from './PublishPages';
import { ReportListPage, ReportPage } from './ReportPages';
import { ReviewQueuePage, ReviewRevisionPage } from './ReviewPages';
import { t } from '../i18n';

interface SpaceLink {
  key: string;
  label: string;
  to: string;
  description: string;
  available: (s: Spaces) => boolean;
  routes: EditorialRoute['name'][];
}

export const SPACE_LINKS: SpaceLink[] = [
  { key: 'review', label: t('shell.space.review'), to: paths.reviewQueue, description: t('shell.space.reviewText'), available: (s) => s.reviewer, routes: ['reviewQueue', 'reviewRevision', 'reviewCourse'] },
  { key: 'publish', label: t('shell.space.publish'), to: paths.publishQueue, description: t('shell.space.publishText'), available: (s) => s.publisher, routes: ['publishQueue', 'publishRevision', 'publishCourse'] },
  { key: 'reports', label: t('shell.space.reports'), to: paths.reports, description: t('shell.space.reportsText'), available: canHandleReports, routes: ['reports', 'report'] },
  { key: 'admin', label: t('shell.space.admin'), to: paths.adminLessons, description: t('shell.space.adminText'), available: (s) => s.administrator, routes: ['adminLessons', 'adminCourses', 'adminCourse', 'adminUsers', 'adminUser', 'adminCatalogue', 'adminCurriculum', 'adminSection', 'adminEdition'] },
];

/** Navigation links for the editorial spaces this account can use. */
export function SpaceNav({ spaces, route }: { spaces: Spaces; route: Route }) {
  return (
    <>
      {SPACE_LINKS.filter((l) => l.available(spaces)).map((l) => (
        <Link key={l.key} to={l.to} current={(l.routes as string[]).includes(route.name)}>
          {l.label}
        </Link>
      ))}
    </>
  );
}

/** Which space a route belongs to and whether the account can use it. */
export function routeAccess(route: EditorialRoute, spaces: Spaces): { space: string; allowed: boolean } {
  if (route.name === 'publication') return { space: t('shell.space.publish'), allowed: spaces.publisher || spaces.administrator };
  // Publishers and administrators read any submitted course revision (they never decide on it).
  if (route.name === 'reviewCourse') return { space: t('shell.space.review'), allowed: spaces.reviewer || spaces.publisher || spaces.administrator };
  const link = SPACE_LINKS.find((l) => (l.routes as string[]).includes(route.name))!;
  return { space: link.label, allowed: link.available(spaces) };
}

/** The page for an editorial route, or the "no access" page when the server would refuse the space. */
export function editorialPage(route: EditorialRoute, spaces: Spaces): ReactNode {
  const access = routeAccess(route, spaces);
  if (!access.allowed) return <NoAccess space={access.space} />;
  switch (route.name) {
    case 'reviewQueue':
      return <ReviewQueuePage />;
    case 'reviewRevision':
      return <ReviewRevisionPage key={route.revisionId} revisionId={route.revisionId} />;
    case 'reviewCourse':
      return <CourseReviewPage key={route.revisionId} revisionId={route.revisionId} />;
    case 'publishCourse':
      return <CoursePublishPage key={route.revisionId} revisionId={route.revisionId} />;
    case 'adminCourses':
      return <AdminCoursesPage />;
    case 'adminCourse':
      return <AdminCoursePage key={route.courseId} courseId={route.courseId} />;
    case 'publishQueue':
      return <PublishQueuePage />;
    case 'publishRevision':
      return <PublishRevisionPage key={route.revisionId} revisionId={route.revisionId} />;
    case 'adminLessons':
      return <AdminLessonsPage />;
    case 'adminUsers':
      return <UserListPage />;
    case 'adminUser':
      return <UserPage key={route.userId} userId={route.userId} />;
    case 'adminCatalogue':
      return <CataloguePage />;
    case 'adminCurriculum':
      return <CurriculumPage />;
    case 'adminSection':
      return <SectionPage key={route.sectionId} sectionId={route.sectionId} />;
    case 'adminEdition':
      return <EditionPage key={route.editionId} editionId={route.editionId} />;
    case 'publication':
      return <LessonPublicationPage key={route.ref} lessonRef={route.ref} />;
    case 'reports':
      return <ReportListPage />;
    case 'report':
      return <ReportPage key={route.reportId} reportId={route.reportId} />;
  }
}

/** /studio for accounts without the author permission: the spaces they can use. */
export function StudioHome({ spaces }: { spaces: Spaces }) {
  const available = SPACE_LINKS.filter((l) => l.available(spaces));
  return (
    <main className="page" id="main">
      <h1 tabIndex={-1}>{t('shell.home')}</h1>
      <p>{t('shell.noAuthor')}</p>
      {available.length > 0 ? (
        <ul className="lesson-list" aria-label={t('shell.yourSpaces')}>
          {available.map((l) => (
            <li key={l.key} className="lesson-card">
              <h2>
                <Link to={l.to}>{l.label}</Link>
              </h2>
              <p className="lesson-context">{l.description}</p>
            </li>
          ))}
        </ul>
      ) : (
        <p>{t('shell.noSpace')}</p>
      )}
    </main>
  );
}
