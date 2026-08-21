import { defineConfig } from 'vite'
import { fileURLToPath, URL } from 'node:url'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { VitePWA } from 'vite-plugin-pwa'

export default defineConfig({
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  plugins: [
    react(),
    tailwindcss(),
    VitePWA({
      // Development instances (kyber-dev) build with
      // KYBER_PWA_SELF_DESTROYING=true. The generated worker then
      // unregisters itself and drops its caches instead of precaching the
      // app shell.
      //
      // Why this exists: the production worker precaches index.html, which
      // is exactly right for an installed PWA and exactly wrong for an
      // instance that gets redeployed every few minutes. A browser with the
      // worker installed serves the old index.html from disk and never asks
      // the server, so a fresh deploy is invisible no matter how many times
      // the operator reloads.
      //
      // selfDestroying rather than disable: `disable` merely stops shipping
      // a worker, which leaves any ALREADY-registered worker in place
      // serving stale content forever with no way out but manually clearing
      // site data. The self-destroying worker actively cleans up the
      // installs that are already out there.
      selfDestroying: process.env.KYBER_PWA_SELF_DESTROYING === 'true',
      registerType: 'autoUpdate',
      includeAssets: ['icon-192.png', 'icon-512.png', 'icon-maskable-512.png', 'apple-touch-icon.png'],
      manifest: {
        name: 'Kyber',
        short_name: 'Kyber',
        start_url: '/',
        display: 'standalone',
        background_color: '#0a0e13',
        theme_color: '#0a0e13',
        icons: [
          {
            src: '/icon-192.png',
            sizes: '192x192',
            type: 'image/png',
          },
          {
            src: '/icon-512.png',
            sizes: '512x512',
            type: 'image/png',
          },
          {
            src: '/icon-maskable-512.png',
            sizes: '512x512',
            type: 'image/png',
            purpose: 'maskable',
          },
        ],
      },
      workbox: {
        globPatterns: ['**/*.{js,css,html,ico,png,svg}'],
        // Pair with registerType: 'autoUpdate' above. Without these a new
        // SW installs but sits in 'waiting', and the old SW keeps serving
        // cached assets until every tab is closed.
        clientsClaim: true,
        skipWaiting: true,
        runtimeCaching: [
          {
            urlPattern: /^.*\/api\/v1\/.*/,
            handler: 'NetworkFirst',
            options: {
              cacheName: 'api-cache',
              networkTimeoutSeconds: 10,
            },
          },
        ],
      },
    }),
  ],
  build: {
    outDir: 'dist',
    sourcemap: false,
  },
  server: {
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
        configure: (proxy) => {
          proxy.on('proxyReq', (proxyReq, req) => {
            // Browser-session mutations enforce an exact same-origin check.
            // In development the browser addresses Vite on :5173 while the
            // proxied request addresses the control plane on :8080, so make
            // the forwarded Origin agree with the forwarded Host. This is
            // confined to Vite's local development proxy; production keeps
            // the control plane's strict Origin validation unchanged.
            if (req.headers.origin) {
              proxyReq.setHeader('Origin', 'http://localhost:8080')
            }
          })
        },
      },
      '/ws': {
        target: 'ws://localhost:8080',
        ws: true,
      },
    },
  },
})
