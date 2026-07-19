import { useRef } from "react";
import { Button } from "@/components/ui/button";
import { toast } from "../toast";

// Onboarding: a signed-in account with no projects shouldn't hit a blank
// sidebar. Explain that access comes from an invite, let them paste one,
// and — since any member can — offer to start a new project.
export function EmptyState({
  authEnabled,
  onCreate,
}: {
  authEnabled: boolean;
  onCreate: (name: string) => void;
}) {
  const invite = useRef<HTMLInputElement>(null);
  const name = useRef<HTMLInputElement>(null);

  const join = () => {
    const v = invite.current!.value.trim();
    const m = v.match(/join\/([0-9a-f]+)/) || v.match(/^([0-9a-f]{8,})$/);
    if (!m) {
      toast("That doesn't look like an invite link.", true);
      return;
    }
    location.href = "/join/" + m[1];
  };

  return (
    <div className="onboard">
      <h1>Welcome to BearDrive</h1>
      <p>You're signed in, but you're not part of any project yet.</p>
      {authEnabled && (
        <div className="ob-card">
          <h3>Have an invite link?</h3>
          <p>A teammate can send you a join link. Paste it here:</p>
          <div className="ob-row">
            <input id="ob-invite" type="text" placeholder="https://…/join/…" autoComplete="off" ref={invite} />
            <Button id="ob-join" variant="primary" onClick={join}>
              Join
            </Button>
          </div>
        </div>
      )}
      <div className="ob-card">
        <h3>Or start a new project</h3>
        <p>Create a shared space for your team's files.</p>
        <div className="ob-row">
          <input id="ob-name" type="text" placeholder="Project name, e.g. wiki" autoComplete="off" ref={name} />
          <Button id="ob-create" variant="primary" onClick={() => onCreate(name.current!.value.trim())}>
            Create
          </Button>
        </div>
      </div>
    </div>
  );
}
