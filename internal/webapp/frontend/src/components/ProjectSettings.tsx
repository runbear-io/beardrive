import { useEffect } from "react";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { api } from "../api/http";
import { modalPrompt } from "../modal";
import { toast } from "../toast";
import { useHubRefresh } from "../hooks/useHub";
import { PROJECT_ICONS, ProjectIcon } from "./shell";
import { projColor } from "./ProjectNav";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Separator } from "@/components/ui/separator";
import { Textarea } from "@/components/ui/textarea";
import type { Org, Project } from "../api/types";

// Settings for the open project (sidebar menu): General edits the name,
// description and icon; About holds the identity facts; the danger zone
// deletes. Install/connect lives on the Installation page.

const MAX_DESC = 280;

// Mirrors the server's rules (projects.go) so a typo never round-trips.
const schema = z.object({
  name: z
    .string()
    .trim()
    .min(1, "Give the project a name.")
    .max(120, "Keep the name under 120 characters."),
  description: z.string().max(MAX_DESC, `Keep the description under ${MAX_DESC} characters.`),
  icon: z.string(),
});
type Values = z.infer<typeof schema>;

export function ProjectSettings({
  project,
  org,
  onDeleted,
}: {
  project: Project;
  org: Org | null;
  onDeleted: () => Promise<void>;
}) {
  const refresh = useHubRefresh();
  // Owner-only, and only as UX: handleProjectUpdate enforces it too. Swap for
  // the project-level permission once BEA-2 lands.
  const mayEdit = org?.role === "owner";

  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: project.name,
      description: project.description ?? "",
      icon: project.icon ?? "",
    },
  });
  // Switching projects (or a refresh bringing new values) re-seeds the form,
  // so the fields never show another project's metadata.
  useEffect(() => {
    form.reset({
      name: project.name,
      description: project.description ?? "",
      icon: project.icon ?? "",
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [project.id, project.name, project.description, project.icon]);

  const icon = form.watch("icon");
  const description = form.watch("description");

  const save = form.handleSubmit(async (values) => {
    // Only the dirty keys travel: PATCH is a partial update, so an untouched
    // field is never sent — and never overwritten by a stale value.
    const dirty = form.formState.dirtyFields;
    const body: Partial<Values> = {};
    if (dirty.name) body.name = values.name.trim();
    if (dirty.description) body.description = values.description;
    if (dirty.icon) body.icon = values.icon;
    if (Object.keys(body).length === 0) return;
    try {
      await api("PATCH", "/api/projects/" + project.id, body);
      toast("Saved.");
      form.reset({ ...values, name: values.name.trim() }); // clean, keeps what was typed
      await refresh(); // nav mark + dashboard header update without a reload
    } catch (e) {
      toast((e as Error).message, true); // form left alone, so nothing is lost
    }
  });

  return (
    <div className="project-settings">
      <h2>{project.name}</h2>

      <Card>
        <CardHeader>
          <CardTitle>General</CardTitle>
          <CardDescription>Name, description and icon for this project.</CardDescription>
        </CardHeader>
        <Separator />
        <CardContent>
          <form className="ps-form" onSubmit={save}>
            <div className="ps-field">
              <Label htmlFor="ps-icon-btn">Icon</Label>
              <div className="ps-icon-row">
                <span
                  className="proj-mark"
                  aria-hidden="true"
                  style={{ background: projColor(project.name) }}
                >
                  <ProjectIcon name={icon} />
                </span>
                <DropdownMenu>
                  <DropdownMenuTrigger asChild>
                    <Button id="ps-icon-btn" type="button" variant="subtle" disabled={!mayEdit}>
                      Change
                    </Button>
                  </DropdownMenuTrigger>
                  <DropdownMenuContent align="start" className="ps-icon-grid">
                    {/* Real menu items, so the grid closes on pick and works
                        from the keyboard like every other menu in the app. */}
                    <DropdownMenuItem
                      className={"ps-icon-cell" + (icon === "" ? " active" : "")}
                      title="Default"
                      aria-label="Default icon"
                      onSelect={() => form.setValue("icon", "", { shouldDirty: true })}
                    >
                      <ProjectIcon />
                    </DropdownMenuItem>
                    {Object.keys(PROJECT_ICONS).map((name) => (
                      <DropdownMenuItem
                        key={name}
                        className={"ps-icon-cell" + (icon === name ? " active" : "")}
                        title={name}
                        aria-label={name}
                        onSelect={() => form.setValue("icon", name, { shouldDirty: true })}
                      >
                        <ProjectIcon name={name} />
                      </DropdownMenuItem>
                    ))}
                  </DropdownMenuContent>
                </DropdownMenu>
              </div>
            </div>

            <div className="ps-field">
              <Label htmlFor="ps-name">Name</Label>
              <Input
                id="ps-name"
                disabled={!mayEdit}
                aria-invalid={!!form.formState.errors.name}
                aria-describedby={form.formState.errors.name ? "ps-name-err" : undefined}
                {...form.register("name")}
              />
              {form.formState.errors.name && (
                <span id="ps-name-err" role="alert" className="field-err">
                  {form.formState.errors.name.message}
                </span>
              )}
            </div>

            <div className="ps-field">
              <Label htmlFor="ps-desc">
                Description <span className="ps-opt">(optional)</span>
              </Label>
              <Textarea
                id="ps-desc"
                rows={2}
                disabled={!mayEdit}
                placeholder="What this project is for."
                aria-invalid={!!form.formState.errors.description}
                aria-describedby={form.formState.errors.description ? "ps-desc-err" : undefined}
                {...form.register("description")}
              />
              <div className="ps-meta">
                {form.formState.errors.description ? (
                  <span id="ps-desc-err" role="alert" className="field-err">
                    {form.formState.errors.description.message}
                  </span>
                ) : (
                  <span />
                )}
                <span className="ps-count">
                  {description.length} / {MAX_DESC}
                </span>
              </div>
            </div>

            {mayEdit && (
              <>
                <Separator />
                <div className="ps-actions">
                  <Button
                    id="ps-save"
                    type="submit"
                    variant="primary"
                    disabled={!form.formState.isDirty || form.formState.isSubmitting}
                  >
                    Save changes
                  </Button>
                </div>
              </>
            )}
          </form>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>About</CardTitle>
        </CardHeader>
        <Separator />
        <CardContent>
          <dl className="ps-facts">
            <dt>Project id</dt>
            <dd>
              <code>{project.id}</code>
            </dd>
            {org && (
              <>
                <dt>Workspace</dt>
                <dd>{org.name}</dd>
              </>
            )}
            {project.created && (
              <>
                <dt>Created</dt>
                <dd>{new Date(project.created).toLocaleDateString()}</dd>
              </>
            )}
          </dl>
        </CardContent>
      </Card>

      {/* Owner-only, and only as UX: handleProjectDelete enforces it too. */}
      {org?.role === "owner" && (
        <Card className="ps-danger">
          <CardHeader>
            <CardTitle>Danger zone</CardTitle>
          </CardHeader>
          <Separator />
          <CardContent>
            <p>
              Deleting removes the project from this hub. Its files stay in storage. This can't be
              undone.
            </p>
            <Button
              variant="danger"
              onClick={async () => {
                const typed = await modalPrompt(
                  `Delete “${project.name}”?`,
                  "This can't be undone. Type the project name to confirm:",
                  "",
                  "Delete project",
                  { match: project.name, danger: true },
                );
                if (typed === null) return;
                try {
                  await api("DELETE", "/api/projects/" + project.id);
                  toast(`Deleted “${project.name}”.`);
                  await onDeleted();
                } catch (e) {
                  toast((e as Error).message, true);
                }
              }}
            >
              Delete project
            </Button>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
