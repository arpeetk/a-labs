import { defineConfig } from "vite"
import react from "@vitejs/plugin-react"

export default defineConfig({
  plugins: [react()],
  // Keep the tracked placeholder so plain `go test ./...` can satisfy the
  // desktop entrypoint's embed pattern before a frontend build has run.
  build: { emptyOutDir: false },
})
