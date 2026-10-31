import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const api = "http://localhost:8080";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: { "/api": api, "/admin": api, "/tenants": api, "/companies": api },
  },
});
