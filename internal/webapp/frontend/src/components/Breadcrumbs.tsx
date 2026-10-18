// Every ancestor segment is a link to that folder's listing; the last
// segment is the current page. The whole path stays on screen: the first and
// last segments keep their full names, and a long segment between them is
// cut to an ellipsis (its full path is the hover text).
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
    <span className="crumbs" title={path}>
      {parts.map((seg, i) => {
        acc = acc ? acc + "/" + seg : seg;
        const target = acc;
        const last = i === parts.length - 1;
        const mid = i > 0 && !last;
        return (
          <span key={target} className="crumb">
            {i > 0 && (
              <span className="crumb-sep" aria-hidden="true">
                ›
              </span>
            )}
            {last ? (
              <span className="crumb-last">{seg}</span>
            ) : (
              <span
                className={"crumb-seg" + (mid ? " crumb-mid" : "")}
                title={target}
                onClick={() => onOpenFolder(target)}
              >
                {seg}
              </span>
            )}
          </span>
        );
      })}
    </span>
  );
}
