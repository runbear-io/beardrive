import { flexRender, type Table as RTable } from "@tanstack/react-table";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

/* The render shell every react-table admin list shares: the .admin-item /
   .ai-* vocabulary the CSS already styles, plus a header cell that is
   sortable from the keyboard. It lives here because three tables (members,
   org shares, project shares) render identically — two of them used to
   carry copies of this markup and drifted apart. */

// SortableHead is a real button inside the th, so sorting is reachable by
// keyboard with the direction announced rather than carried by a glyph
// alone.
export function SortableHead({ header }: { header: any }) {
  const sorted = header.column.getIsSorted();
  if (!header.column.getCanSort()) {
    // A column with no header text and nothing to sort (the actions column)
    // would be a dead tab stop with an empty accessible name.
    return <TableHead>{flexRender(header.column.columnDef.header, header.getContext())}</TableHead>;
  }
  return (
    <TableHead
      data-sort={sorted || undefined}
      aria-sort={sorted === "asc" ? "ascending" : sorted === "desc" ? "descending" : "none"}
    >
      <button type="button" className="th-sort" onClick={header.column.getToggleSortingHandler()}>
        {flexRender(header.column.columnDef.header, header.getContext())}
        {sorted === "asc" ? " ↑" : sorted === "desc" ? " ↓" : ""}
      </button>
    </TableHead>
  );
}

export function AdminTable<T>({ table, className }: { table: RTable<T>; className?: string }) {
  return (
    <div className={"admin-list admin-card-table" + (className ? " " + className : "")}>
      <Table className="admin-table">
        <TableHeader>
          {table.getHeaderGroups().map((hg) => (
            <TableRow key={hg.id}>
              {hg.headers.map((h) => (
                <SortableHead key={h.id} header={h} />
              ))}
            </TableRow>
          ))}
        </TableHeader>
        <TableBody>
          {table.getRowModel().rows.map((r) => (
            <TableRow key={r.id} className="admin-item">
              {r.getVisibleCells().map((c) => (
                <TableCell key={c.id}>{flexRender(c.column.columnDef.cell, c.getContext())}</TableCell>
              ))}
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}
