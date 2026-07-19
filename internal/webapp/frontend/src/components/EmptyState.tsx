import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { Button } from "@/components/ui/button";

// Onboarding: a signed-in account with no projects shouldn't hit a blank
// sidebar. Explain that access comes from an invite, let them paste one,
// and — since any member can — offer to start a new project. Both inputs
// are RHF+zod forms with inline errors (no toast-on-typo).

const joinSchema = z.object({
  invite: z
    .string()
    .trim()
    .refine((v) => /join\/([0-9a-f]+)/.test(v) || /^[0-9a-f]{8,}$/.test(v), {
      message: "That doesn't look like an invite link.",
    }),
});
const createSchema = z.object({
  name: z.string().trim().min(1, "Give the project a name.").max(60, "Keep it under 60 characters."),
});

export function EmptyState({
  authEnabled,
  onCreate,
}: {
  authEnabled: boolean;
  onCreate: (name: string) => void;
}) {
  const joinForm = useForm<z.infer<typeof joinSchema>>({
    resolver: zodResolver(joinSchema),
    defaultValues: { invite: "" },
  });
  const createForm = useForm<z.infer<typeof createSchema>>({
    resolver: zodResolver(createSchema),
    defaultValues: { name: "" },
  });

  return (
    <div className="onboard">
      <h1>Welcome to BearDrive</h1>
      <p>You're signed in, but you're not part of any project yet.</p>
      {authEnabled && (
        <div className="ob-card">
          <h3>Have an invite link?</h3>
          <p>A teammate can send you a join link. Paste it here:</p>
          <form
            className="ob-row"
            onSubmit={joinForm.handleSubmit(({ invite }) => {
              const m = invite.match(/join\/([0-9a-f]+)/) || invite.match(/^([0-9a-f]{8,})$/);
              location.href = "/join/" + m![1];
            })}
          >
            <input
              id="ob-invite"
              type="text"
              placeholder="https://…/join/…"
              autoComplete="off"
              {...joinForm.register("invite")}
            />
            <Button id="ob-join" variant="primary" type="submit">
              Join
            </Button>
          </form>
          {joinForm.formState.errors.invite && (
            <p className="field-err">{joinForm.formState.errors.invite.message}</p>
          )}
        </div>
      )}
      <div className="ob-card">
        <h3>Or start a new project</h3>
        <p>Create a shared space for your team's files.</p>
        <form className="ob-row" onSubmit={createForm.handleSubmit(({ name }) => onCreate(name))}>
          <input
            id="ob-name"
            type="text"
            placeholder="Project name, e.g. wiki"
            autoComplete="off"
            {...createForm.register("name")}
          />
          <Button id="ob-create" variant="primary" type="submit">
            Create
          </Button>
        </form>
        {createForm.formState.errors.name && (
          <p className="field-err">{createForm.formState.errors.name.message}</p>
        )}
      </div>
    </div>
  );
}
