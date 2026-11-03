// Loaded on first use, so pages that show no Markdown stay small. The text is the admin's
// own, but it's still sanitized: it can be pasted from anywhere and is shown inside the panel.
import { marked } from 'marked';
import DOMPurify from 'dompurify';

export function renderMarkdown(text) {
  return DOMPurify.sanitize(marked.parse(text, { gfm: true, breaks: false }));
}
