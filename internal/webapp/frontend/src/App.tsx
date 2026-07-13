import { useConfig } from "./hooks/useConfig";
import { AppShell, Topbar, VaultHeader } from "./components/shell";
import { Toaster } from "./toast";
import { ModalHost } from "./modal";
import HubApp from "./apps/HubApp";
import VolumeApp from "./apps/VolumeApp";

export default function App() {
  const { data: config } = useConfig();

  return (
    <>
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
    </>
  );
}
