import { toast as sonner } from "sonner";
import { Toaster as SonnerToaster } from "@/components/ui/sonner";

// Transient toast. Same imperative `toast()` API as the original in-house
// version; sonner (via the shadcn wrapper) renders it. Call sites and specs
// are markup-agnostic.

export function toast(msg: string, isErr = false) {
  // Errors stay until dismissed: a failed action the user has to react to
  // should not vanish while they are reading it.
  if (isErr) sonner.error(msg, { duration: Infinity, closeButton: true });
  else sonner(msg);
}

// The hub refused a save that would empty or sharply shrink a file. Keeping
// the saved version is the default: nothing is written unless the user asks.
export function warnShrink(path: string, confirm: () => Promise<void>) {
  const name = path.split("/").pop() || path;
  sonner.warning(`Not saved: this would empty or cut most of ${name}. The saved version was kept.`, {
    duration: Infinity,
    closeButton: true,
    action: { label: "Save anyway", onClick: () => void confirm() },
  });
}

export function Toaster() {
  return <SonnerToaster position="bottom-center" />;
}
