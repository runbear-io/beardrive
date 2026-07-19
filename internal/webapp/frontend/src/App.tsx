import { useConfig } from "./hooks/useConfig";
import { TooltipProvider } from "@/components/ui/tooltip";
import { AppShell, Topbar, VaultHeader } from "./components/shell";
import { Toaster } from "./toast";
import { ModalHost } from "./modal";
import HubApp from "./apps/HubApp";
import VolumeApp from "./apps/VolumeApp";

export default function App() {
  const { data: config } = useConfig();

  return (
    <TooltipProvider delayDuration={150}>
      {!config ? (
        <AppShell vault={<VaultHeader name="…" showSignout={false} />} topbar={<Topbar />}>
          <div className="empty">Loading…</div>
        </AppShell>
      ) : config.mode === "hub" ? (
        <HubApp config={config} />
      ) : (
        <VolumeApp config={config} />
      )}
      <Toaster />
      <ModalHost />
    </TooltipProvider>
  );
}
