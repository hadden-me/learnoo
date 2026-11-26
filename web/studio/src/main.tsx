import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import '../../design/tokens.css';
import 'virtual:reader-lesson.css';
import './styles.css';
import { App } from './App';
import { applyToDocument } from './i18n';

// The server set <html lang dir> from the locale cookie; this only completes dir for the development server (French).
applyToDocument();

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
