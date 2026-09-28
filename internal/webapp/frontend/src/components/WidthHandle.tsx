import type { KeyboardEvent, PointerEvent } from "react";
import { READ_DEFAULT, READ_MIN, clampReadWidth } from "../lib/readwidth";
import { FM_RAIL } from "../util";

/* Drag handles on both edges of the reading column (.page.read), the way
   Notion lets you pull a page wider. The column is centered, so an edge that
   moves dx changes the width by 2·dx — which keeps the edge under the cursor.

   While dragging, only the --page-read variable on the .page element is
   written: the value reaches React (onChange) once, on release, so the whole
   file page is not re-rendered on every pointer move. Double-click puts the
   default measure back. */

const RAIL = 268; // frontmatter rail + gap beside the prose — style.css
const STEP = 32; // arrow-key nudge

type Side = "left" | "right";

// How wide the prose column may get: the scroller's content box, minus the
// frontmatter rail when it is laid out beside the prose.
function roomFor(page: HTMLElement): number {
  const box = page.parentElement!;
  const cs = getComputedStyle(box);
  const inner = box.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight);
  const rail = page.querySelector(":scope > .fmpanel") && window.matchMedia(FM_RAIL).matches ? RAIL : 0;
  return inner - rail;
}

export function WidthHandles(props: { width: number | null; onChange: (w: number | null) => void }) {
  const current = props.width ?? READ_DEFAULT;

  const onPointerDown = (side: Side) => (e: PointerEvent<HTMLDivElement>) => {
    if (e.button !== 0) return;
    e.preventDefault();
    const handle = e.currentTarget;
    const page = handle.parentElement!;
    const room = roomFor(page);
    // Start from what is on screen: a narrow window already squeezes the
    // column below its max-width, and dragging must not jump to the latter.
    const start = Math.min(current, room);
    const x0 = e.clientX;
    let next: number | null = null;
    handle.setPointerCapture(e.pointerId);
    handle.classList.add("dragging");
    document.body.classList.add("rw-dragging");
    const move = (ev: globalThis.PointerEvent) => {
      const dx = side === "right" ? ev.clientX - x0 : x0 - ev.clientX;
      next = clampReadWidth(start + 2 * dx, room);
      page.style.setProperty("--page-read", next + "px");
    };
    const up = () => {
      handle.removeEventListener("pointermove", move);
      handle.removeEventListener("pointerup", up);
      handle.removeEventListener("pointercancel", up);
      handle.classList.remove("dragging");
      document.body.classList.remove("rw-dragging");
      // A click that never moved commits nothing — the two clicks of a
      // double-click must not pin the default as a "custom" width.
      if (next !== null) props.onChange(next);
    };
    handle.addEventListener("pointermove", move);
    handle.addEventListener("pointerup", up);
    handle.addEventListener("pointercancel", up);
  };

  const onKeyDown = (side: Side) => (e: KeyboardEvent<HTMLDivElement>) => {
    const wider = side === "right" ? "ArrowRight" : "ArrowLeft";
    const narrower = side === "right" ? "ArrowLeft" : "ArrowRight";
    if (e.key !== wider && e.key !== narrower) return;
    e.preventDefault();
    const room = roomFor(e.currentTarget.parentElement!);
    props.onChange(clampReadWidth(Math.min(current, room) + (e.key === wider ? STEP : -STEP), room));
  };

  return (
    <>
      {(["left", "right"] as const).map((side) => (
        <div
          key={side}
          className={`rw-handle rw-${side}`}
          role="separator"
          aria-orientation="vertical"
          aria-label="Resize the reading width"
          aria-valuenow={current}
          aria-valuemin={READ_MIN}
          tabIndex={0}
          title="Drag to resize · double-click to reset"
          onPointerDown={onPointerDown(side)}
          onKeyDown={onKeyDown(side)}
          onDoubleClick={() => props.onChange(null)}
        />
      ))}
    </>
  );
}
