import type { Range } from "@/lib/fuzzy";

/* Paints `text` with the matched ranges emphasised. Ranges are in the
   coordinates of a longer label that `text` starts at `offset` inside, so one
   set of ranges can paint a file name and its folder separately; whatever
   falls outside `text` is ignored. The text is always painted whole and in
   order: a highlight never adds, drops or repeats a character. */
export function Marked({
  text,
  ranges,
  offset = 0,
  className,
  tag: Tag = "b",
}: {
  text: string;
  ranges: readonly Range[];
  offset?: number;
  className?: string;
  tag?: "b" | "mark";
}) {
  const out: React.ReactNode[] = [];
  let at = 0;
  for (const [start, end] of ranges) {
    const a = Math.max(start - offset, at),
      b = Math.min(end - offset, text.length);
    if (b <= a) continue;
    if (a > at) out.push(text.slice(at, a));
    out.push(<Tag key={a}>{text.slice(a, b)}</Tag>);
    at = b;
  }
  if (at < text.length) out.push(text.slice(at));
  return <span className={className}>{out}</span>;
}
