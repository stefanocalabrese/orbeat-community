import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClientProvider } from "@tanstack/react-query";
import "./index.css";
import App from "./App";
import { AppAuthProvider } from "./auth/AuthProvider";
import { createAppQueryClient } from "./api/client";
import { loadConfig } from "./config";
import { ToastProvider } from "./components/ui/Toast";
import ConfigWarningBanner from "./components/ConfigWarningBanner";

const queryClient = createAppQueryClient();

// Load runtime config (/config.json) before mounting: the OIDC provider needs
// the authority + client_id at mount time, so nothing may render until config
// is populated. loadConfig never rejects (it degrades to the fallback).
void loadConfig().then(() => {
  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      {/* Rendered outside AppAuthProvider/QueryClientProvider on purpose: it
          reads only the module-level configLoadFailed flag loadConfig() just
          settled above, needs no auth or query context, and must still show
          even if a bad oidcAuthority (drawn from the same failed config)
          makes AppAuthProvider itself unable to render anything useful. */}
      <ConfigWarningBanner />
      <AppAuthProvider>
        <QueryClientProvider client={queryClient}>
          {/* Inside QueryClientProvider because useInvalidating (api/queries.ts)
              calls useToast from every mutation hook, and outside App so a
              toast survives a route change: the page that started a mutation is
              often not the page that is on screen when it lands. */}
          <ToastProvider>
            <App />
          </ToastProvider>
        </QueryClientProvider>
      </AppAuthProvider>
    </StrictMode>,
  );
});
