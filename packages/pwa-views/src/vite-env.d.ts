// Minimal ImportMeta type declaration for import.meta.env usage.
// This avoids a hard dev-dependency on vite while still allowing
// packages/pwa-views/src/mocks/init.ts to read VITE_ENABLE_MOCKS.
interface ImportMeta {
  readonly env: ImportMetaEnv
}

interface ImportMetaEnv {
  readonly VITE_ENABLE_MOCKS?: string
  [key: string]: string | boolean | undefined
}
