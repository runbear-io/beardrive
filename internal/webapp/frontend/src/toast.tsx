import { toast as sonner } from "sonner";
import { Toaster as SonnerToaster } from "@/components/ui/sonner";

// Transient toast. Same imperative `toast()` API as the original in-house
// version; sonner (via the shadcn wrapper) renders it. Call sites and specs
// are markup-agnostic.

export function toast(msg: string, isErr = false) {
  if (isErr) sonner.error(msg);
  else sonner(msg);
}

export function Toaster() {
  return <SonnerToaster position="bottom-center" />;
}
