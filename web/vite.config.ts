import { defineConfig } from 'vite';
import tailwindcss from '@tailwindcss/vite';

export default defineConfig({
  plugins: [tailwindcss()],
  // Explicit: the rolldown-backed build stopped copying public/ with its
  // defaults, silently dropping theme.js from dist after emptyOutDir.
  publicDir: 'public',
  copyPublicDir: true,
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
