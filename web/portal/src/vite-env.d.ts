/// <reference types="vite/client" />

interface ImportMetaEnv {
  /**
   * Set by the static demo build (`npm run build:demo`). It swaps the API server for the
   * WebAssembly control plane in web/portal/src/demo/, and puts the demo credentials on the login
   * screen. Absent in every ordinary build, where the portal talks to a real API.
   */
  readonly VITE_DEMO?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
