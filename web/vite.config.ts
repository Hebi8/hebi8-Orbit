import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://127.0.0.1:4310',
      '/ws': {
        target: 'ws://127.0.0.1:4310',
        ws: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (!id.includes('node_modules')) return undefined;
          // Language grammars & themes stay as lazily-loaded per-language chunks.
          if (/(shiki|@shikijs)[\\/].*(langs|themes)[\\/]/.test(id)) return undefined;
          if (id.includes('shiki') || id.includes('@shikijs')) return 'shiki';
          if (id.includes('react-diff-view') || id.includes('diff-match-patch') || id.includes('gitdiff-parser')) return 'diff';
          return 'vendor';
        },
      },
    },
  },
});
