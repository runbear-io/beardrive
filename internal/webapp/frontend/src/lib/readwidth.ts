/* The reading column's width while a reader drags its edge wider or
   narrower (components/WidthHandle.tsx). Held in the tab's memory only: a
   reload or a new tab starts back at the default measure, which is the reset. */

export const READ_DEFAULT = 768; // must match --page-read in style.css
export const READ_MIN = 600;

// `room` is how wide the column could get in the current window. Never below
// READ_MIN, so the floor holds even on a window narrower than it.
export function clampReadWidth(w: number, room: number): number {
  return Math.round(Math.min(Math.max(w, READ_MIN), Math.max(room, READ_MIN)));
}
