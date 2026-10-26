---
title: Data queries
description: Query a project's JSON files and Markdown frontmatter as data — filter, pick fields, sort, group and count — over the hub API or MCP, without reading files one by one.
---

A project's files stay the source of truth. When a project admin turns on
**Data** in the project's Settings, the hub also indexes every JSON file in the
project, and the YAML frontmatter of every Markdown file, so that an app or an
agent can ask questions of them as data:

- "Konny's placements, 15 fields each, most recently changed first."
- "How many placements per client and stage?"
- "Contacts per client per week, in Seoul time."
- "Everything awaiting a reply with a DR over 50."

The index follows the files. A write through the hub is in it before the write's
request returns. A device's sync is in it as soon as the hub has folded the push.
Turning Data off drops the index.

## Asking

```http
POST /api/p/<project-id>/data/query
Content-Type: application/json

{
  "pattern": "{client}/placements/**/*.json",
  "where":   {"$client": "konny", "stage": "Awaiting reply", "dr": {"gt": 50}},
  "select":  ["$path", "domain", "dr", "next_action.text"],
  "order":   ["-$changed"],
  "limit":   200
}
```

| Key | Meaning |
|---|---|
| `pattern` | Which files. `*` matches within one path segment, `**` any number of segments, and `{name}` captures a segment as the field `$name`. |
| `each` | Optional. A JSON pointer to an array in each file (`/sends`, `/items`). Each element becomes a row, and fields then read the element. |
| `where` | Optional. `{field: value}` tests equality. `{field: {op: value}}` tests with `gt` `gte` `lt` `lte` `ne` `in` `nin` `exists` `contains`. Combine filters with `and`, `or` and `not`. |
| `select` | The fields to return. Default: `["$path"]`. |
| `order` | Fields to sort by. Prefix a field with `-` for descending order. |
| `limit` | Rows per page, from 1 to 1000. Default: 100. |
| `cursor` | The `next` value from the previous page. |
| `group` | Group rows by these fields and count each group. |
| `count` | Without `group`: return only the number of matching rows. |

A **field** is a dot path into the document (`next_action.due`), a capture
(`$client`), or one of these system fields:

| Field | Value |
|---|---|
| `$path` | The file's path. |
| `$sha` | The file's current version. Pass it to MCP `edit` as the version you read. |
| `$changed` | When the file last changed. |
| `$changed_by` | The account that made that change. |
| `$url` | The file's page on the hub. |
| `$item` | With `each`, the element's JSON pointer, for example `/items/0`. |
| `$doc` | The whole document, or the whole element. |

Two functions are available:
- `lower(field)` lowercases a string, so that `Closed` and `closed` count as one value.
- `week(field, Asia/Seoul)` gives the ISO week (`2026-W43`) of an ISO 8601 date or timestamp in a time zone. Without a zone it uses UTC.

Comparisons are **typed**. `{"dr": {"gt": 50}}` is false for a `dr` that is
`null`, missing, or the string `"70"`; it is never an error. Sorting puts
numbers in numeric order, then strings in byte order, then nulls.

## The answer

```json
{"rows": [{"$path": "konny/placements/listicle/a.com.json", "domain": "a.com", "dr": 62}],
 "next": "…", "errors": 1, "as_of": "2026-10-26T09:12:03.000000000Z", "complete": true}
```

- `groups` replaces `rows` when the query groups. `count` replaces `rows` when the query only counts.
- `errors` is how many matching files could not be read as data: invalid JSON, invalid frontmatter, or a file over 2 MiB. They are counted, never silently left out.
- `as_of` is when the index last caught up with the project.
- `complete` is false while the index is behind the project: during the first build after turning Data on, or after a pass that could not fetch every file (the next change retries them).

Every answer has an **ETag**. Send it back as `If-None-Match`, and the hub
answers `304 Not Modified` while nothing you can see has changed. The ETag
covers only the files that match the pattern and that you may read, so a
change elsewhere, or behind a folder you are denied, does not invalidate it.
A cursor belongs to the version it came from. If the data changes between
pages, the next page answers `409`; start again from the first page.

`POST /api/p/<project-id>/data/describe` with `{"pattern": …, "each": …, "field": …}`
lists the fields one level below `field` (the top level by default). For each
field it shows how often the field appears, its types, and its five most
common values, across a sample of up to 500 documents.

## Permissions and limits

- Reading needs read access to the project. Folder rules apply row by row: a file in a folder you are denied is not in your rows, counts or groups.
- A query that returns `$doc` counts as a read of each file in [read heat](/guides/what-agents-read/). A query that returns other fields does not.
- One page has at most 1,000 rows. A grouped query has at most 1,000 groups. An answer is at most 4 MiB, and a query may run for at most 2 seconds. A query over a limit is refused with an error; the answer is never cut short.
- Each account may send 600 queries per minute.

## From a server: apps and keys

A server that reads a project's data, such as a dashboard or another product's
backend, should not sign in as a person. An organization owner creates an
**app** for it under the organization's settings (**Apps**). The app has an id
of the form `app:<id>`. The owner then mints the app's keys.

```http
POST /api/orgs/<org>/apps                   {"name": "SearchD"}   → {"id", "principal": "app:…"}
POST /api/orgs/<org>/apps/<app>/keys                              → {"key": "bda_…"}   (shown once)
DELETE /api/orgs/<org>/apps/<app>/keys/<key>
```

- **An app sees only what it is granted.** Add its id under a project's Settings → People, or name it in a folder rule, as you would an email address. An app never inherits a project's default access, and it can be granted read or write, never admin.
- **A key opens a few routes only.** Send it as `Authorization: Bearer bda_…`. It reaches `data/query`, `data/describe` and `history` on projects the app was granted. Every other route answers `403`.
- **A key is stored as a digest.** It can be revoked at any time. An app can hold several keys at once, so you can replace one without a gap. The Apps list shows when each key was last used.
- **Deleting an app** revokes every key it holds.
- **An app's reads do not count** toward read heat.

## From an agent

The MCP door has the same two operations as tools: `query` and `describe`.
They take the same JSON as the REST API, plus a `project`, which is the
project's name or id.
