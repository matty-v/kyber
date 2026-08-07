import { defineConfig } from 'tsup'
import { fileURLToPath } from 'node:url'
import { resolve, dirname } from 'node:path'
import { copyFileSync, mkdirSync } from 'node:fs'

const __dirname = dirname(fileURLToPath(import.meta.url))

export default defineConfig({
  entry: ['src/index.ts'],
  format: ['esm'],
  dts: true,
  splitting: false,
  sourcemap: true,
  clean: true,
  // React + router are peer deps; CSS is handled separately (Tailwind v4
  // syntax isn't resolvable by esbuild alone, so we copy it raw).
  external: ['react', 'react-dom', 'react-router-dom', /\.css$/],
  injectStyle: false,
  // Copy src/index.css → dist/index.css so the exports map entry
  // "./styles.css": "./dist/index.css" resolves at consume time.
  // The CSS uses Tailwind v4 @import syntax — consumers process it with
  // @tailwindcss/vite (or any PostCSS-Tailwind pipeline); tsup just ships
  // the raw source.
  async onSuccess() {
    const distDir = resolve(__dirname, 'dist')
    mkdirSync(distDir, { recursive: true })
    copyFileSync(resolve(__dirname, 'src/index.css'), resolve(distDir, 'index.css'))
  },
  esbuildOptions(options) {
    options.alias = { '@': resolve(__dirname, 'src') }
  },
})
