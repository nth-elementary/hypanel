import { EditorView, basicSetup } from 'codemirror';
import { css } from '@codemirror/lang-css';
import { html } from '@codemirror/lang-html';
import { javascript } from '@codemirror/lang-javascript';
import { json } from '@codemirror/lang-json';
import { markdown } from '@codemirror/lang-markdown';
import { php } from '@codemirror/lang-php';
import { python } from '@codemirror/lang-python';
import { sql } from '@codemirror/lang-sql';
import { yaml } from '@codemirror/lang-yaml';
import { StreamLanguage } from '@codemirror/language';
import { nginx } from '@codemirror/legacy-modes/mode/nginx';
import { shell } from '@codemirror/legacy-modes/mode/shell';
import { oneDark } from '@codemirror/theme-one-dark';

function language(filename) {
  const name = filename.toLowerCase();
  const ext = name.split('.').pop();
  if (name.includes('nginx') || name.endsWith('.conf') || name.endsWith('.conf.disabled')) return StreamLanguage.define(nginx);
  switch (ext) {
    case 'css': case 'scss': return css();
    case 'html': case 'htm': return html();
    case 'js': case 'mjs': case 'cjs': case 'ts': case 'jsx': case 'tsx': return javascript({ jsx: true, typescript: ext.includes('ts') });
    case 'json': return json();
    case 'md': case 'markdown': return markdown();
    case 'php': return php();
    case 'py': return python();
    case 'sql': return sql();
    case 'yml': case 'yaml': return yaml();
    case 'sh': case 'bash': case 'env': return StreamLanguage.define(shell);
    default: return [];
  }
}

function isDark() {
  const set = document.documentElement.dataset.theme;
  return set ? set === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
}

export function createEditor(parent, doc, filename, onChange) {
  return new EditorView({
    doc,
    parent,
    extensions: [
      basicSetup,
      language(filename),
      isDark() ? oneDark : [],
      EditorView.lineWrapping,
      EditorView.updateListener.of((u) => {
        if (u.docChanged) onChange(u.state.doc.toString());
      }),
    ],
  });
}
