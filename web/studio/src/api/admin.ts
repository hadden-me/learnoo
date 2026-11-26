// Administration endpoints (docs/API.md "Administration (LMS-B10)" and
// "Curriculum (LMS-B02)"): users, grants, account status, student
// invitations, catalogue display names and order, and the curriculum
// hierarchy. Built on the authoring client (client.ts): same session, CSRF
// handling and typed errors. The server checks the administrator permission
// on every request; nothing here grants or hides access on its own.
import { ApiError, type Api, type CatalogueEntry } from './client';
import { describeEditorialError } from './editorialMessages';
import { t } from '../i18n';

export type GlobalPermission = 'author' | 'publisher' | 'administrator';
export type PermissionName = GlobalPermission | 'reviewer';
export const GLOBAL_PERMISSIONS: GlobalPermission[] = ['author', 'publisher', 'administrator'];

export interface ReviewerSubject {
  id: number;
  code: string;
  nameFr: string | null;
  nameAr: string | null;
  retiredAt: string | null;
}

export interface AdminUser {
  id: number;
  email: string;
  displayName: string;
  createdAt: string;
  disabled: boolean;
  permissions: GlobalPermission[];
  reviewerSubjects: ReviewerSubject[];
}

export interface UserEvent {
  kind: 'permission' | 'status';
  action: 'grant' | 'revoke' | 'disable' | 'enable';
  permission: PermissionName | null;
  subjectId: number | null;
  subjectCode: string | null;
  actorId: number | null;
  actorName: string | null;
  operatorLabel: string | null;
  occurredAt: string;
}

export interface UserFilter {
  q?: string;
  permission?: PermissionName | 'none' | '';
  status?: 'active' | 'disabled' | '';
  after?: number | null;
}

export interface UserChange {
  user: AdminUser;
  changed: boolean;
}

export interface Invitation {
  invitation: { id: number; email: string; createdAt: string; expiresAt: string };
  /** `/invite/<token>`: shown once; the link is built from this page's origin. */
  path: string;
}

export interface AdminSubject extends CatalogueEntry {
  chapters: CatalogueEntry[];
}

export interface Section {
  id: number;
  levelId: number;
  code: string;
  nameFr: string;
  nameAr: string;
  displayOrder: number;
  retiredAt: string | null;
  version: number;
  createdAt: string;
}

export interface Level extends Omit<Section, 'levelId'> {
  sections: Section[];
}

export type EditionState = 'draft' | 'current' | 'superseded' | 'retired';

export interface EditionSummary {
  id: number;
  sectionId: number;
  code: string;
  labelFr: string;
  labelAr: string;
  state: EditionState;
  currentSince: string | null;
  supersededAt: string | null;
  retiredAt: string | null;
  version: number;
  createdAt: string;
}

export interface AssignedChapter {
  chapterId: number;
  code: string;
  position: number;
  retired: boolean;
}

export interface Assignment {
  id: number;
  subjectId: number;
  subjectCode: string;
  selectable: boolean;
  chapters: AssignedChapter[];
}

export interface Edition extends EditionSummary {
  assignments: Assignment[];
}

export interface EntryFields {
  nameFr: string;
  nameAr: string;
  displayOrder: number;
}

/** The query string of a user search; empty values are left out (the server refuses empty parameters). */
export function userQuery(f: UserFilter): string {
  const params = new URLSearchParams();
  const q = f.q?.trim();
  if (q) params.set('q', q);
  if (f.permission) params.set('permission', f.permission);
  if (f.status) params.set('status', f.status);
  if (f.after) params.set('after', String(f.after));
  const s = params.toString();
  return s ? `?${s}` : '';
}

/** The full invitation link: this page's origin and the path the server returned. */
export const invitationLink = (origin: string, path: string) => origin.replace(/\/+$/, '') + path;

const enc = encodeURIComponent;
const CUR = '/api/admin/curriculum';

/** Typed administration calls, sharing the authoring client's session. */
export function adminApi(api: Api) {
  return {
    users: (f: UserFilter = {}) => api.request<{ users: AdminUser[]; nextAfter: number | null }>('GET', `/api/admin/users${userQuery(f)}`),
    user: (id: number) => api.request<{ user: AdminUser; history: UserEvent[] }>('GET', `/api/admin/users/${id}`),
    grant: (id: number, permission: PermissionName, subjectId?: number) =>
      api.request<UserChange>('POST', `/api/admin/users/${id}/grant`, { json: permission === 'reviewer' ? { permission, subjectId } : { permission } }),
    revoke: (id: number, permission: PermissionName, subjectId?: number) =>
      api.request<UserChange>('POST', `/api/admin/users/${id}/revoke`, { json: permission === 'reviewer' ? { permission, subjectId } : { permission } }),
    disable: (id: number) => api.request<UserChange>('POST', `/api/admin/users/${id}/disable`, { json: {} }),
    enable: (id: number) => api.request<UserChange>('POST', `/api/admin/users/${id}/enable`, { json: {} }),
    invite: (email: string, ttlHours?: number) =>
      api.request<Invitation>('POST', '/api/admin/invitations', { json: ttlHours === undefined ? { email } : { email, ttlHours } }),

    catalogue: async () => (await api.request<{ subjects: AdminSubject[] }>('GET', '/api/admin/catalogue')).subjects,
    setSubjectNames: (code: string, names: { nameFr?: string; nameAr?: string }) =>
      api.request<{ subject: CatalogueEntry; changed: boolean }>('PUT', `/api/admin/catalogue/subjects/${enc(code)}/names`, { json: names }),
    setSubjectOrder: (code: string, sortOrder: number) =>
      api.request<{ subject: CatalogueEntry; changed: boolean }>('PUT', `/api/admin/catalogue/subjects/${enc(code)}/order`, { json: { sortOrder } }),
    setChapterNames: (subject: string, chapter: string, names: { nameFr?: string; nameAr?: string }) =>
      api.request<{ chapter: CatalogueEntry; changed: boolean }>('PUT', `/api/admin/catalogue/subjects/${enc(subject)}/chapters/${enc(chapter)}/names`, { json: names }),
    setChapterOrder: (subject: string, chapter: string, sortOrder: number) =>
      api.request<{ chapter: CatalogueEntry; changed: boolean }>('PUT', `/api/admin/catalogue/subjects/${enc(subject)}/chapters/${enc(chapter)}/order`, { json: { sortOrder } }),

    levels: async () => (await api.request<{ levels: Level[] }>('GET', '/api/catalogue/levels')).levels,
    editions: (sectionId: number) => api.request<{ section: Section; editions: EditionSummary[] }>('GET', `/api/catalogue/sections/${sectionId}/editions`),
    edition: (id: number) => api.request<Edition>('GET', `/api/catalogue/editions/${id}`),
    createLevel: (code: string, f: EntryFields) => api.request<Level>('POST', `${CUR}/levels`, { json: { code, ...f } }),
    updateLevel: (id: number, expectedVersion: number, f: EntryFields) => api.request<Level>('PUT', `${CUR}/levels/${id}`, { json: { expectedVersion, ...f } }),
    retireLevel: (id: number, expectedVersion: number) => api.request<Level>('POST', `${CUR}/levels/${id}/retire`, { json: { expectedVersion } }),
    createSection: (levelId: number, code: string, f: EntryFields) => api.request<Section>('POST', `${CUR}/levels/${levelId}/sections`, { json: { code, ...f } }),
    updateSection: (id: number, expectedVersion: number, f: EntryFields) => api.request<Section>('PUT', `${CUR}/sections/${id}`, { json: { expectedVersion, ...f } }),
    retireSection: (id: number, expectedVersion: number) => api.request<Section>('POST', `${CUR}/sections/${id}/retire`, { json: { expectedVersion } }),
    createEdition: (sectionId: number, code: string, labelFr: string, labelAr: string) =>
      api.request<Edition>('POST', `${CUR}/sections/${sectionId}/editions`, { json: { code, labelFr, labelAr } }),
    renameEdition: (id: number, expectedVersion: number, labelFr: string, labelAr: string) =>
      api.request<Edition>('PUT', `${CUR}/editions/${id}`, { json: { expectedVersion, labelFr, labelAr } }),
    addAssignment: (editionId: number, expectedVersion: number, subjectId: number) =>
      api.request<Edition>('POST', `${CUR}/editions/${editionId}/assignments`, { json: { expectedVersion, subjectId } }),
    removeAssignment: (editionId: number, assignmentId: number, expectedVersion: number) =>
      api.request<Edition>('DELETE', `${CUR}/editions/${editionId}/assignments/${assignmentId}?expectedVersion=${expectedVersion}`),
    setChapters: (editionId: number, assignmentId: number, expectedVersion: number, chapterIds: number[]) =>
      api.request<Edition>('PUT', `${CUR}/editions/${editionId}/assignments/${assignmentId}/chapters`, { json: { expectedVersion, chapterIds } }),
    makeCurrent: (editionId: number, expectedVersion: number, expectedCurrentEditionId: number | null) =>
      api.request<{ edition: Edition; supersededEditionId: number | null }>('POST', `${CUR}/editions/${editionId}/make-current`, {
        json: { expectedVersion, expectedCurrentEditionId },
      }),
    retireEdition: (editionId: number, expectedVersion: number) => api.request<Edition>('POST', `${CUR}/editions/${editionId}/retire`, { json: { expectedVersion } }),
  };
}

export type AdminApi = ReturnType<typeof adminApi>;

const MESSAGES: Record<string, string> = {
  self_change: t('admin.error.self_change'),
  last_administrator: t('admin.error.last_administrator'),
  account_exists: t('admin.error.account_exists'),
  invalid_input: t('admin.error.invalid_input'),
  retired_subject: t('admin.error.retired_subject'),
  unknown_subject: t('admin.error.unknown_subject'),
  version_conflict: t('admin.error.version_conflict'),
  edition_not_draft: t('admin.error.edition_not_draft'),
  state_conflict: t('admin.error.state_conflict'),
  code_exists: t('admin.error.code_exists'),
  already_retired: t('admin.error.already_retired'),
  parent_retired: t('admin.error.parent_retired'),
  assignment_exists: t('admin.error.assignment_exists'),
  placement_invalid: t('admin.error.placement_invalid'),
  retry: t('admin.error.retry'),
};

/** A sentence in the interface language for an administration failure (falls back to the editorial and authoring messages). */
export function describeAdminError(error: unknown): string {
  if (error instanceof ApiError && error.code in MESSAGES) {
    const base = MESSAGES[error.code];
    return error.code === 'placement_invalid' && error.message ? t('shell.error.detail', { base, detail: error.message }) : base;
  }
  return describeEditorialError(error);
}

export const PERMISSION_LABELS: Record<PermissionName, string> = {
  author: t('admin.permission.author'),
  publisher: t('admin.permission.publisher'),
  administrator: t('admin.permission.administrator'),
  reviewer: t('admin.permission.reviewer'),
};

export const EDITION_STATE_LABELS: Record<EditionState, string> = {
  draft: t('admin.editionState.draft'),
  current: t('admin.editionState.current'),
  superseded: t('admin.editionState.superseded'),
  retired: t('admin.editionState.retired'),
};

/**
 * Which controls the account page offers. The server refuses self-grants and
 * self-disabling (self_change); revoking one's own permissions stays possible.
 */
export function selfRules(isSelf: boolean) {
  return { canGrant: !isSelf, canDisable: !isSelf, canRevoke: true };
}

/** The notice the Studio home shows after an administrator revoked their own administrator permission. */
export const ADMIN_REVOKED_NOTICE = 'admin-revoked';

/**
 * Revoking one's own administrator permission ends access to every administration page: the page does not
 * reload the account (the server would refuse it) but reloads the Studio home, which rediscovers the spaces
 * the account still has and explains what happened.
 */
export const leavesAdministration = (isSelf: boolean, p: { kind: string; permission?: string }) =>
  isSelf && p.kind === 'revoke' && p.permission === 'administrator';

export const adminRevokedHome = `/studio/?notice=${ADMIN_REVOKED_NOTICE}`;

/** The message for a notice in the Studio home's query string, or null. */
export function noticeFrom(search: string): string | null {
  const notice = new URLSearchParams(search).get('notice');
  return notice === ADMIN_REVOKED_NOTICE
    ? t('admin.notice.adminRevoked')
    : null;
}

/** Moves the item at `index` by `delta` (−1 up, +1 down); out-of-range moves return the list unchanged. */
export function move<T>(list: T[], index: number, delta: -1 | 1): T[] {
  const target = index + delta;
  if (index < 0 || index >= list.length || target < 0 || target >= list.length) return list;
  const next = [...list];
  [next[index], next[target]] = [next[target], next[index]];
  return next;
}

/** The section's current edition ID (expectedCurrentEditionId for make-current), or null. */
export const currentEditionId = (editions: EditionSummary[]) => editions.find((e) => e.state === 'current')?.id ?? null;
