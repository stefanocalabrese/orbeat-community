import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClientProvider } from "@tanstack/react-query";
import "./index.css";
import App from "./App";
import { AppAuthProvider } from "./auth/AuthProvider";
import { createAppQueryClient } from "./api/client";
import { loadConfig } from "./config";

const queryClient = createAppQueryClient();

// Load runtime config (/config.json) before mounting: the OIDC provider needs
// the authority + client_id at mount time, so nothing may render until config
// is populated. loadConfig never rejects (it degrades to the fallback).
void loadConfig().then(() => {
  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      <AppAuthProvider>
        <QueryClientProvider client={queryClient}>
          <App />
        </QueryClientProvider>
      </AppAuthProvider>
    </StrictMode>,
  );
});
