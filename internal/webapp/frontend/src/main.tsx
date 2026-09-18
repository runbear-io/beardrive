import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import App from "./App";
import ErrorBoundary from "./ErrorBoundary";
import "./tw.css";
import "./style.css";

/* staleTime is a default, not a policy: without one every query is stale the
   moment it lands, so any remount refetches bytes the cache already holds.
   Freshness comes from the change stream invalidating what actually changed
   (useProjectEvents), which overrides this regardless of the clock. */
const queryClient = new QueryClient({
  defaultOptions: {
    queries: { retry: 1, refetchOnWindowFocus: false, staleTime: 30_000 },
  },
});

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ErrorBoundary>
      <QueryClientProvider client={queryClient}>
        <App />
      </QueryClientProvider>
    </ErrorBoundary>
  </StrictMode>,
);
