// RFC 4180-ish delimited text, parsed so the viewer can show a table instead
// of a wall of monospace. Pure so `npm test` (node's runner) can import it
// without React — same reason as lib/sniff.ts.
//
// The contract that matters: NEVER throw. `null` means "this isn't a table",
// and the caller falls back to the plain-text preview. A viewer that
// white-screens on a malformed CSV is worse than the wall it replaced.

export type Csv = {
  rows: string[][];
  truncated: number; // rows past the cap — counted, not kept
};

// Rows kept in the DOM. Not a final number; it only has to be stated on
// screen. Virtualization is deliberately out of scope — lower this if 5k
// rows measures badly.
export const CSV_ROWS = 5000;

export function parseDelimited(text: string, delim: string, cap = CSV_ROWS): Csv | null {
  const rows: string[][] = [];
  let row: string[] = [];
  let cell = "";
  let quoted = false;
  let truncated = 0;

  const endRow = () => {
    row.push(cell);
    cell = "";
    if (rows.length < cap) rows.push(row);
    else truncated++;
    row = [];
  };

  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (quoted) {
      // Inside quotes a doubled quote is a literal one and a newline is
      // content, not a row break.
      if (c !== '"') cell += c;
      else if (text[i + 1] === '"') {
        cell += '"';
        i++;
      } else quoted = false;
      continue;
    }
    if (c === '"' && cell === "") quoted = true;
    else if (c === delim) {
      row.push(cell);
      cell = "";
    } else if (c === "\n") endRow();
    else if (c === "\r" && text[i + 1] === "\n") continue; // CRLF
    else cell += c;
  }

  if (quoted) return null; // unterminated quote: not something to guess at
  if (cell !== "" || row.length) endRow(); // last row, no trailing newline
  if (!rows.length) return null;
  // A file with no delimiter is not a table, it is text.
  if (rows[0].length < 2) return null;
  return { rows, truncated };
}
