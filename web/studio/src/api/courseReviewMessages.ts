// French explanations for the course review, publication and recovery codes
// (docs/API.md "Course review and publication (LMS-B04)"). Course draft
// codes fall back to describeCourseError (courseMessages.ts), the rest to
// describeEditorialError. Each says what happened and that nothing changed
// when the server guarantees it.
import { ApiError } from './client';
import { describeCourseError } from './courseMessages';
import { describeEditorialError } from './editorialMessages';

const MESSAGES: Record<string, string> = {
  incomplete: 'Soumission refusée : le cours est incomplet. Les points à compléter sont signalés dans le formulaire. Rien n’a été soumis.',
  ineligible:
    'Refusé : une étape épingle une révision de leçon qui n’est plus publiable (elle doit être approuvée, publiée au moins une fois et non retirée, et la leçon doit être publiée). Rien n’a été modifié.',
  draft_exists:
    'Un brouillon de ce cours est déjà ouvert : il ne peut y avoir qu’une version en cours. Supprimez ce brouillon pour créer l’amendement, ou remplacez la révision épinglée directement dans le brouillon.',
  active_candidate_exists:
    'Le cours a déjà une version en cours (en relecture, ou approuvée en attente de publication). Attendez sa publication, ou demandez à un administrateur de la retirer.',
  not_published: 'Ce cours n’a pas de version publiée : il n’y a rien à amender ou à suspendre.',
  no_newer_revisions: 'Aucune leçon du cours publié n’a de révision plus récente publiable : il n’y a rien à amender.',
  not_amendable: 'Ces remplacements ne peuvent pas former un amendement. Créez une version ordinaire du cours.',
  invalid_step:
    'Remplacement refusé : la révision choisie n’est pas une révision plus récente et publiable de la leçon de cette étape, ou une autre étape épingle une leçon d’un autre auteur qui n’est plus publiable (remplacez-la aussi, ou créez une version ordinaire).',
  not_eligible_reviewer: 'Vous ne pouvez pas décider de cette version du cours. Le serveur a refusé ; rien n’a été écrit.',
  not_in_review: 'Cette version du cours n’est plus en relecture : une décision a déjà été enregistrée (peut-être par un autre relecteur), ou elle a été retirée. Rien n’a été écrit.',
  withdrawn: 'Cette version du cours a été retirée par un administrateur : elle ne peut plus être relue, publiée ni restaurée.',
  comment_required: 'Expliquez les modifications demandées dans un commentaire : il est obligatoire.',
  invalid_comment: 'Commentaire refusé : 5 000 caractères au maximum, en texte valide.',
  reason_required: 'Indiquez un motif : il est obligatoire et sera conservé dans l’historique.',
  invalid_reason: 'Motif refusé : 2 000 caractères au maximum, en texte valide.',
  stale_pointer:
    'La publication de ce cours a changé depuis l’ouverture de la page (action d’une autre personne ou d’un autre onglet). Rien n’a été modifié. Rechargez la page pour voir l’état actuel avant de recommencer.',
  stale_base:
    'Publication refusée : la version publiée du cours a changé depuis la rédaction de cette version (autre publication, suspension ou restauration). Rien n’a été publié. Une version à jour doit être rédigée et relue ; un administrateur peut retirer celle-ci pour libérer le cours.',
  not_approved: 'Seule une version approuvée du cours peut être publiée.',
  already_published: 'Cette version du cours a déjà été publiée. Pour la remettre en ligne, utilisez la restauration.',
  never_published: 'Cette version n’a jamais été publiée : elle ne peut pas être restaurée.',
  already_current: 'Cette version est déjà celle que lisent les élèves.',
  published: 'Cette version est celle que lisent les élèves : elle ne peut pas être retirée. Suspendez le cours en cas d’urgence.',
  not_withdrawable: 'Seule une version en relecture ou approuvée peut être retirée.',
  archived: 'Ce cours est archivé : désarchivez-le d’abord (un administrateur).',
  not_archived: 'Ce cours n’est pas archivé.',
  never_submitted: 'Ce cours n’a jamais été soumis : supprimez-le plutôt que de demander son archivage.',
  unknown_user: 'Le nouveau propriétaire doit être un compte actif ayant la permission auteur.',
  not_draft: 'Cette version du cours n’est plus un brouillon (soumise ou supprimée ailleurs). Rechargez la page.',
};

/** A French sentence for a course workflow failure. */
export function describeCourseWorkflowError(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.code in MESSAGES) return MESSAGES[error.code];
    if (error.code === 'placement_invalid' || error.code === 'version_conflict' || error.code === 'not_found') return describeCourseError(error);
  }
  return describeEditorialError(error);
}
