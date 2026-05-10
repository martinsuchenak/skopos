import { defineConfig } from 'vite';

export default defineConfig({
  build: {
    outDir: 'dist',
    rollupOptions: {
      input: {
        app: 'src/main.ts',
        style: 'src/style.css',
      },
    },
  },
});
