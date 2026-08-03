import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  base: '/app/qvmconsole-manager/',
  plugins: [react()],
  build: {
    outDir: '../webassets/dist',
    emptyOutDir: true,
    sourcemap: false,
  },
  server: {
    port: 5173,
    proxy: {
      '/app/qvmconsole-manager': 'http://127.0.0.1:18990',
    },
  },
});
