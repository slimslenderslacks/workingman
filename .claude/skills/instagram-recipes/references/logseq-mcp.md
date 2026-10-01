# Logseq MCP — verified tool surface

Probed against a live Logseq **DB** graph (Logseq's built-in MCP server, the one
registered with `sbx` as the `logseq` static MCP). Everything below was
confirmed by calling the server, not read from docs.

A DB graph is not a markdown-file graph: pages are entities, blocks are nodes,
and nothing here touches `.md` files on disk.

## Tools

| Tool | Arguments | Notes |
| --- | --- | --- |
| `listPages` | `expand?: bool` | Returns every page, including journals, tags and properties. Journals look like `Oct 1st, 2026`. |
| `getPage` | `pageName: string` (name **or** uuid) | Returns `{entity, blocks}`. Errors with `API Error: Page "X" not found` if absent — that error is a normal "does it exist?" answer. |
| `searchBlocks` | `searchTerm: string` | Full-text over block content. Good for dedupe by reel shortcode. |
| `upsertNodes` | `operations: array`, `dry-run?: bool` | The only write. See below. |
| `listTags` | `expand?: bool` | |
| `listProperties` | `expand?: bool` | |

## `upsertNodes`

Its own description says: *"This tool must be called at most once per user
request. Never re-call it unless explicitly asked."* It is a batch API — build
one `operations` array covering all the work.

Each operation:

- `operation` — `"add"` or `"edit"`
- `entityType` — `"block"`, `"page"`, `"tag"`, or `"property"`
- `id` — for `edit`, a real uuid string. For `add`, an arbitrary temp string, so
  later operations can reference the thing you just created.
- `data` — the fields to set.

Clojure keywords are written as plain JSON strings (`"add"`, not `":add"`).

### `data` keys

For the page/block work this skill does, only three matter:

- `title` — a page's name, or a block's content
- `page-id` — which page a block belongs to; required when adding a block.
  Accepts a real page uuid **or** a temp id from an earlier `add`.
- `tags` — list of tag uuid strings

The rest (`property-type`, `property-cardinality`, `property-classes`,
`class-extends`, `class-properties`) are for defining tags and properties.

### Verified behaviors

- **Dry run works and validates.** `"dry-run": true` returns e.g.
  `"Dry run: Added: {:page 1, :block 2}."` or `"Dry run:  Edited: {:block 1}."`
  and commits nothing. Use it as a preflight on every payload.
- **Unknown `data` keys are hard errors**, reported per-operation:
  `API Error: Tool arguments are invalid: [nil nil {:data {:parent-id ["disallowed key"]}}]`
  (the `nil`s are the operations that passed). Useful: the position of the map
  tells you which operation is bad.
- **No block nesting.** `parent-id` is rejected. Every block added through this
  API lands at the page's top level (`level: 1`). Structure a page as a flat
  ordered sequence and carry hierarchy in the text (`## Ingredients`).
- **No ordering control.** Added blocks append to the bottom; there is no
  `order`/`position`/`index` argument. To place something at the top of an
  existing page you must rewrite existing blocks' `title`s in place and append
  the overflow.
- **No delete.** There is no tool to remove a page or a block. Writes are
  effectively one-way — which is why the dry run matters and why the skill
  dedupes before writing.
- **`edit` needs a real uuid.** Temp ids only resolve within the call that
  created them.
- **`page-id` takes a page uuid, and a block uuid slips through.** `getPage`
  returns `entity.uuid` (the page) alongside a `blocks` list whose uuids look
  the same. Passing a block uuid as `page-id` is accepted by validation and
  the dry run still reports the counts you expected, so this one is silent —
  read the value out of `entity`, not out of `blocks`.

### Reading block order

`getPage` returns blocks with:

```json
{"uuid": "...", "title": "glass noodles", "level": 1,
 "parent": {"id": 1063}, "order": "a0", "id": 1066,
 "created-at": 1778609671997, "updated-at": 1778609878308}
```

Sort by **`order`**, lexicographically — the sequence runs
`a0, a1, ... a6, a6G, a6V, a7, a8 ...`, so a plain string sort is correct and
a numeric one is not. The array order in the response is not guaranteed to
match, and `created-at` reflects when a block was typed, not where it sits.

Empty blocks (`"title": ""`) show up in real pages; skip them when counting
entries.

## Which graph?

The server serves whichever graph the Logseq desktop app currently has open.
There is no graph argument on any tool, and no way to switch graphs remotely —
if it's the wrong graph, a human has to switch it in the app.
