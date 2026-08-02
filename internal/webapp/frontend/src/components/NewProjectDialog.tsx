import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import type { StartTemplate } from "../api/types";

// The "I already have a folder" pick. It creates the same empty project the
// "Empty project" pick does — the browser cannot reach your disk, so this can
// only change what you are told next, never what is created. Create therefore
// stays enabled: disabling it would leave the dialog a dead end AND produce no
// project id, which is the one thing the paste prompt actually needs.
export const EXISTING = "__existing__";

// The create-project dialog: a name, plus where the project starts from.
//
// Not a modalPrompt() variant on purpose — that API exists for one-field
// prompts, and teaching it about choices makes every other caller pay for the
// shape. This is a local useState over the Dialog we already have.
//
// The options come from /api/config, so a hub that ships another template
// needs no change here. "Empty project" is the synthetic first-class option
// (value "") and stays preselected: creating a project without picking
// anything must behave exactly as it did before templates existed.
export function NewProjectDialog({
  templates,
  onCreate,
  onClose,
}: {
  templates: StartTemplate[];
  onCreate: (name: string, template: string) => Promise<void>;
  onClose: () => void;
}) {
  const [name, setName] = useState("");
  // "" is an empty project; EXISTING is also an empty project — same artifact,
  // different intent, and the intent is what the next screen needs to know.
  const [template, setTemplate] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    if (busy) return;
    if (!name.trim()) {
      setErr("Give it a name.");
      return;
    }
    setBusy(true);
    try {
      await onCreate(name.trim(), template);
    } finally {
      setBusy(false);
    }
  };

  // Recommended first, then the rest, then the two that seed nothing. The
  // divider before them is doing real work: everything above answers "what
  // should we put in it", everything below answers "nothing".
  const options = [
    ...templates.map((t) => ({ value: t.name, title: t.title, blurb: t.blurb, rule: false })),
    {
      value: EXISTING,
      title: "I already have a folder",
      blurb: "nothing is seeded — connect it and your files stay as they are",
      rule: true,
    },
    { value: "", title: "Empty project", blurb: "just the folder", rule: false },
  ];

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="modal" showCloseButton={false}>
        <DialogTitle asChild>
          <h3>New project</h3>
        </DialogTitle>
        <label className="modal-label" htmlFor="modal-input">
          Name
        </label>
        <input
          className="modal-input"
          type="text"
          autoComplete="off"
          id="modal-input"
          autoFocus
          value={name}
          aria-invalid={!!err}
          aria-describedby={err ? "modal-input-err" : undefined}
          onChange={(e) => {
            setName(e.currentTarget.value);
            if (err) setErr("");
          }}
          onKeyDown={(e) => e.key === "Enter" && submit()}
        />
        {err && (
          <span id="modal-input-err" role="alert" className="field-err">
            {err}
          </span>
        )}

        {options.length > 1 && (
          <fieldset className="start-points">
            <legend className="modal-label">Starting point</legend>
            {options.map((o, i) => (
              <label
                key={o.value}
                className={
                  "start-point" + (template === o.value ? " on" : "") + (o.rule ? " sp-rule" : "")
                }
              >
                <input
                  type="radio"
                  name="template"
                  value={o.value}
                  checked={template === o.value}
                  onChange={() => setTemplate(o.value)}
                />
                <span className="sp-text">
                  <span className="sp-title">
                    {o.title}
                    {i === 0 && <span className="sp-rec">Recommended</span>}
                  </span>
                  <span className="sp-blurb">{o.blurb}</span>
                </span>
              </label>
            ))}
          </fieldset>
        )}

        <div className="modal-actions">
          <Button variant="subtle" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" onClick={submit} disabled={busy}>
            Create
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
