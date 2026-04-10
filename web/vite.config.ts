import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// WSL2 + /mnt/* (9p drvfs) 不支持 inotify，--watch 模式必须用 chokidar polling。
const watchOptions = process.argv.includes('--watch')
  ? { chokidar: { usePolling: true, interval: 1000 } }
  : undefined;

export default defineConfig({
  plugins: [react()],
  build: {
    lib: {
      entry: 'src/index.ts',
      formats: ['es'],
      fileName: 'index',
    },
    outDir: 'dist',
    rollupOptions: {
      // React 由核心前端提供，插件不打包
      external: ['react', 'react-dom', 'react/jsx-runtime'],
    },
    watch: watchOptions,
  },
});
