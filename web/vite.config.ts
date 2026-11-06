import { defineConfig } from 'vite';
import tailwindcss from '@tailwindcss/vite';

export default defineConfig({
  plugins: [tailwindcss()],
  // Explicit: the rolldown-backed build stopped copying public/ with its
  // defaults, silently dropping theme.js from dist after emptyOutDir.
  publicDir: 'public',
  copyPublicDir: true,
  // One copy of each CodeMirror core package in the bundle: extensions are
  // checked with instanceof, so a second @codemirror/state (a lockfile that
  // kept a nested older copy after an update) breaks every editor with
  // "Unrecognized extension value in extension set".
  resolve: {
    dedupe: ['@codemirror/state', '@codemirror/view', '@codemirror/language', '@lezer/common', '@lezer/highlight'],
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    rollupOptions: {
      input: {
        app: 'src/main.ts',
        style: 'src/style.css',
      },
      output: {
        entryFileNames: '[name].js',
        assetFileNames: '[name][extname]',
      },
    },
  },
});
