// CodeMirror 6 wrapper for markdown editing in the inbox modals
// (docs/design/inbox.md, decision 9). CM6 is designed for strict CSP —
// no eval — so the dashboard's script-src 'self' posture holds. The
// wrapper is deliberately dumb: mount, read, destroy. The document never
// enters Alpine's reactive state (deep-proxying a live EditorView breaks
// it); main.ts keeps one module-level instance per open modal.
import { EditorView, keymap, highlightActiveLine, placeholder } from '@codemirror/view';
import { EditorState } from '@codemirror/state';
import { defaultKeymap, history, historyKeymap } from '@codemirror/commands';
import { markdown } from '@codemirror/lang-markdown';
import { HighlightStyle, syntaxHighlighting } from '@codemirror/language';
import { tags as t } from '@lezer/highlight';

// CM6 ships with NO default syntax colors — without a HighlightStyle the
// editor looks like a bare textarea. The styles below reference CSS
// variables (defined per theme in style.css on .dark/.light), so the
// highlighting follows the dashboard theme without swapping facets.
const mdHighlight = HighlightStyle.define([
  { tag: t.heading1, fontSize: '1.05rem', fontWeight: '700', color: 'var(--md-heading)' },
  { tag: t.heading2, fontSize: '1rem', fontWeight: '700', color: 'var(--md-heading)' },
  { tag: t.heading3, fontSize: '0.95rem', fontWeight: '600', color: 'var(--md-heading)' },
  { tag: [t.heading4, t.heading5, t.heading6], fontWeight: '600', color: 'var(--md-heading)' },
  { tag: t.strong, fontWeight: '700', color: 'var(--md-strong)' },
  { tag: t.emphasis, fontStyle: 'italic' },
  { tag: t.link, textDecoration: 'underline', color: 'var(--md-link)' },
  { tag: t.url, color: 'var(--md-url)' },
  { tag: t.monospace, color: 'var(--md-code)', backgroundColor: 'var(--md-code-bg)', borderRadius: '0.25rem' },
  { tag: t.quote, fontStyle: 'italic', color: 'var(--md-quote)' },
  // Markdown's own punctuation: emphasis marks, list bullets, fences, the
  // ">" of quotes — dimmed so prose stands out.
  { tag: [t.processingInstruction, t.meta, t.contentSeparator], color: 'var(--md-marks)' },
]);

export interface EditorOptions {
  onSubmit?: () => void;
}

export function mountMarkdownEditor(parent: HTMLElement, initial: string, opts: EditorOptions = {}): EditorView {
  return new EditorView({
    state: EditorState.create({
      doc: initial,
      extensions: [
        history(),
        keymap.of([
          // Save straight from the editor.
          { key: 'Mod-Enter', run: () => { opts.onSubmit?.(); return true; } },
          ...defaultKeymap,
          ...historyKeymap,
        ]),
        // Plain markdown(): its default keymap continues list markers and
        // blockquote marks on Enter — the behavior a notes editor wants.
        markdown(),
        EditorView.lineWrapping,
        highlightActiveLine(),
        placeholder('Rough notes in markdown…'),
        syntaxHighlighting(mdHighlight),
      ],
    }),
    parent,
  });
}

export function editorText(view: EditorView | null): string {
  return view ? view.state.doc.toString() : '';
}

export function destroyEditor(view: EditorView | null): null {
  if (view) view.destroy();
  return null;
}
