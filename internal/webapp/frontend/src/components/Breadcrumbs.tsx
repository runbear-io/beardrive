// Every ancestor segment is a link to that folder's listing; the last
// segment is the current page.
export function Breadcrumbs({
  path,
  onOpenFolder,
}: {
  path: string;
  onOpenFolder: (dir: string) => void;
}) {
  const parts = path.split("/");
  let acc = "";
  return (
    <>
      {parts.map((seg, i) => {
        acc = acc ? acc + "/" + seg : seg;
        const target = acc;
        const last = i === parts.length - 1;
        return (
          <span key={target}>
            {i > 0 && <span className="crumb-sep">/</span>}
            {last ? (
              <span>{seg}</span>
            ) : (
              <span className="crumb-seg" title={target} onClick={() => onOpenFolder(target)}>
                {seg}
              </span>
            )}
          </span>
        );
      })}
    </>
  );
}
