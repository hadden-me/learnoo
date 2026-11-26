// French explanations for the server's error codes (docs/API.md). The
// server's own `message` is English and for diagnostics; the validator's
// reason for 422 invalid_content is shown after the explanation because it
// names the faulty part of the document.
import { ApiError } from './client';

const MESSAGES: Record<string, string> = {
  network: 'Le serveur ne répond pas. Vérifiez la connexion puis réessayez ; vos modifications restent dans l’éditeur.',
  unauthenticated: 'Votre session a expiré. Reconnectez-vous ; vos modifications restent dans l’éditeur.',
  account_changed:
    'Un autre compte est connecté dans ce navigateur. Rien n’a été envoyé : vos modifications restent ici. Reconnectez-vous avec votre compte pour continuer.',
  csrf_rejected: 'La session a changé (connexion dans un autre onglet ?). Réessayez.',
  cross_origin_rejected: 'Requête refusée : elle ne vient pas de Learnoo.',
  forbidden: 'Votre compte n’a pas la permission nécessaire pour cette action.',
  not_found: 'Introuvable : cette leçon ou révision n’existe pas ou ne vous appartient pas.',
  version_conflict: 'Ce brouillon a été modifié ailleurs (autre onglet ou appareil) depuis son ouverture. Rien n’a été écrasé.',
  not_draft: 'Cette révision a déjà été soumise : elle ne peut plus être modifiée. Créez une nouvelle version.',
  active_candidate_exists:
    'La leçon a déjà une version en cours : un brouillon, une révision en relecture ou une révision approuvée qui attend sa publication. Une nouvelle version ne pourra être créée qu’après sa publication ou son retrait.',
  archived: 'Cette leçon est archivée.',
  translation_exists: 'Cette leçon a déjà une traduction dans cette langue.',
  retry: 'Conflit temporaire avec une autre opération. Rien n’a été écrit : réessayez.',
  too_large: 'Trop volumineux : 256 Kio maximum pour une leçon, 5 Mio pour une image.',
  unsupported_media_type: 'Format refusé : seules les images PNG, JPEG et WebP sont acceptées (pas de SVG ni de GIF).',
  unsupported_heic:
    'Les photos HEIC/HEIF ne sont pas encore acceptées. Exportez ou partagez la photo au format JPEG (ou enregistrez-la en PNG), puis téléversez ce fichier. Sur beaucoup de téléphones, un réglage de l’appareil photo (format « le plus compatible » ou JPEG) permet aussi d’enregistrer directement les prochaines photos en JPEG.',
  unsupported_cmyk:
    'Les JPEG en CMJN (couleurs pour l’impression) ne sont pas acceptés. Convertissez l’image en RVB (sRVB) dans votre logiciel, puis exportez-la en JPEG ou en PNG. Les photos de téléphone ne sont pas concernées.',
  type_mismatch: 'Le contenu du fichier ne correspond pas à son type déclaré. Ré-exportez l’image en PNG ou JPEG.',
  invalid_content: 'Le document ne respecte pas le format de leçon.',
  language_mismatch: 'La langue du document diffère de celle de la leçon.',
  unknown_subject: 'Matière inconnue.',
  unknown_chapter: 'Ce chapitre n’appartient pas à la matière de la leçon.',
  unknown_media: 'Une image jointe est introuvable ou ne vous est pas accessible.',
  unknown_lesson: 'La leçon source de la traduction est introuvable ou ne vous appartient pas.',
  translation_subject: 'Une traduction doit garder la matière de la leçon source.',
  invalid_image: 'Image illisible : le fichier est corrompu ou tronqué.',
  invalid_dimensions: 'Image trop grande : 4 096 pixels par côté et 16 mégapixels au maximum.',
  image_too_complex:
    'Cette image demande trop de mémoire au serveur pour être traitée (par exemple un grand JPEG progressif sans sous-échantillonnage des couleurs, ou un grand PNG entrelacé en 16 bits). Exportez-la en JPEG standard ou en PNG 8 bits, ou réduisez sa taille, puis réessayez.',
  trailing_data:
    'L’image contient des données après sa fin que Learnoo ne reconnaît pas (fichier modifié ou format de photo non pris en charge). Ré-exportez-la depuis un éditeur d’images ou enregistrez-la en PNG, puis réessayez.',
  upload_timeout: 'Le téléversement a pris trop de temps (15 secondes maximum). Réessayez avec une meilleure connexion ou une image plus légère.',
  busy: 'Trop de téléversements en cours. Réessayez dans quelques secondes.',
  internal: 'Erreur inattendue du serveur. Réessayez plus tard.',
  invalid_request: 'Requête refusée par le serveur (format invalide).',
};

const HEIF_TYPES = ['image/heic', 'image/heif', 'image/heic-sequence', 'image/heif-sequence'];

/**
 * A HEIC/HEIF photo, which the server refuses (`unsupported_heic`). The extension counts only when the browser
 * gives no type (empty or application/octet-stream); a file it identifies as another type is left to the other checks.
 */
export const isHeif = (file: { type: string; name: string }): boolean => {
  const type = file.type.toLowerCase();
  if (HEIF_TYPES.includes(type)) return true;
  return (type === '' || type === 'application/octet-stream') && /\.(heic|heif)$/i.test(file.name);
};

/** A French sentence for the user; includes the validator's reason for invalid content. */
export function describeError(error: unknown): string {
  if (!(error instanceof ApiError)) return 'Erreur inattendue dans le Studio.';
  const base = MESSAGES[error.code] ?? `Erreur du serveur (${error.status || 'réseau'}).`;
  if (error.code === 'invalid_content' && error.message) return `${base} Détail : ${error.message}`;
  if (error.retryAfter !== null && (error.code === 'busy' || error.code === 'rate_limited')) return `${base} (${error.retryAfter} s)`;
  return base;
}
