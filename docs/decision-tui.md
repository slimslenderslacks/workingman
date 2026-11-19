# decision-tui — answering decision requests on the host

`decision-tui` is a terminal UI for answering *decision requests*: questions
that something (an agent, a script, you) wants a human to settle by picking one
of a fixed set of choices. A producer drops a JSON request file into an inbox
directory; the TUI lists the pending requests, renders the selected one's
markdown summary, and lets you pick one of **the request's own choices** (there
are no free-text answers). The answer is written as JSON to an outbox directory,
where the producer picks it up.

It runs directly **on the host**, not in a sandbox, and is **standalone**: the
orch daemon does not start it, write requests for it or read its answers yet.
The command lives in `cmd/decision-tui`; the file formats and I/O are in
`internal/decision`.

## Build and run

```sh
task decision-tui              # builds ./decision-tui in the repo root
# or by hand:
go build ./cmd/decision-tui
./decision-tui                 # default directories
./decision-tui --inbox /tmp/decisions/in --outbox /tmp/decisions/out
```

`task build` builds it along with the other binaries.

| Flag | Default | Meaning |
|---|---|---|
| `--inbox DIR` | `~/.workingman/decisions/inbox` | directory watched for request files |
| `--outbox DIR` | `~/.workingman/decisions/outbox` | directory answers are written to |

Both directories (and missing parents) are created on start. Positional
arguments are rejected.

To try it, copy the [example request](../examples/decisions/example-request.json)
into the inbox while the TUI is running:

```sh
cp examples/decisions/example-request.json ~/.workingman/decisions/inbox/
```

## Screens and key bindings

**List screen** — the pending requests, oldest `created_at` first (requests
without `created_at` come after those with one; ties are broken by file name),
each with its `source` and `created_at` if set. Below it, an *Errors* section
lists inbox files that could not be read or parsed.

| Key | Action |
|---|---|
| `↑` / `k`, `↓` / `j` | move the cursor |
| `home` / `g`, `end` / `G` | first / last request |
| `enter` | open the selected request |
| `q`, `ctrl+c` | quit |

**Detail screen** — the title, the id / source / created metadata, the summary
rendered as markdown (glamour, light or dark style picked from the terminal
background at start-up) in a scrollable pane, and the choices.

| Key | Action |
|---|---|
| `↑` / `k`, `↓` / `j` | move between choices |
| `enter` | answer with the highlighted choice |
| `pgup` / `pgdn`, `ctrl+u` / `ctrl+d`, mouse wheel | scroll the summary |
| `esc` | back to the list without answering |
| `q`, `ctrl+c` | quit (nothing is answered) |

New, changed and removed request files show up live (fsnotify). If the request
you have open changes, its summary is re-rendered; if it is removed or becomes
malformed, you are returned to the list with a message on the status line.

## Request format

A request is one JSON object in a `*.json` file in the inbox. The file name is
only used to track the file; correlation with the answer is by `id`. Example:
[`examples/decisions/example-request.json`](../examples/decisions/example-request.json).

```json
{
  "id": "workingman-db-choice",
  "title": "Choose the state store for the orch daemon",
  "summary": "The daemon currently keeps its state in ...\n\n## Options\n\n- **SQLite** — ...",
  "choices": [
    {"id": "sqlite", "label": "SQLite"},
    {"id": "bbolt", "label": "bbolt"},
    {"id": "json", "label": "Keep the JSON files"}
  ],
  "created_at": "2026-11-19T10:00:00Z",
  "source": "decision-tui-docs example"
}
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `id` | string | yes | Correlates the answer; becomes the answer's file name `<id>.json`, so it must be filename-safe (see below). |
| `title` | string | yes | One line, shown in the list. Must not be blank. |
| `summary` | string | no | Markdown describing the decision. May be empty. |
| `choices` | array of `{"id", "label"}` | yes | At least one. The only answers the user can give. |
| `choices[].id` | string | yes | Returned as `choice_id`. Not blank; unique within the request. |
| `choices[].label` | string | yes | Shown to the user and returned as `choice_label`. Not blank. |
| `created_at` | string (RFC 3339) | no | Used for ordering and shown in the UI. |
| `source` | string | no | Free-form origin (e.g. agent or project name), shown in the UI. |

Unknown fields are ignored. Validation rules (`decision.Request.Validate`):

- `id` is present, at most 200 bytes, has no leading or trailing whitespace,
  does not start with `.`, does not contain `..`, `/`, `\` or `:`, and has no
  control characters.
- `title` is not empty or whitespace-only.
- `choices` has at least one entry; every choice has a non-blank `id` and
  `label`; no two choices share an `id`.
- Every field has the right JSON type (`created_at` must parse as RFC 3339).

Ids should be unique across pending requests: two requests with the same `id`
are both listed, but answering one overwrites the other's answer file.

## Answer format

When you pick a choice, the TUI writes `<outbox>/<id>.json`. Example:
[`examples/decisions/example-answer.json`](../examples/decisions/example-answer.json).

```json
{
  "id": "workingman-db-choice",
  "choice_id": "sqlite",
  "choice_label": "SQLite",
  "answered_at": "2026-11-19T10:05:42.123456789Z"
}
```

| Field | Type | Notes |
|---|---|---|
| `id` | string | The request's `id`. |
| `choice_id` | string | The `id` of the chosen choice — always one of the request's choices. |
| `choice_label` | string | That choice's `label`. |
| `answered_at` | string (RFC 3339, UTC, nanosecond precision) | When the user answered. |

The answer is written **atomically**: the JSON goes to a hidden temp file
`.<id>.<random>.tmp` in the outbox, which is fsynced, closed, made mode `0644`
and renamed to `<id>.json`. A consumer therefore never sees a partial answer;
it should ignore hidden and `*.tmp` files in the outbox. An existing answer
with the same id is replaced.

## The `answered/` archive

After the answer is written, the request file is moved to
`<inbox>/answered/<same file name>` (the directory is created if needed; an
older archived file with the same name is replaced), so it does not reappear
in the list or on the next start. Subdirectories of the inbox are never
scanned. Order matters for failures:

- If the answer cannot be written, nothing changes: the request stays pending
  and the error is shown on the status line.
- If the answer was written but the request cannot be archived, the TUI drops
  it from the list and reports the error, but the file is still in the inbox
  and will be listed again on the next start.

## Writing requests (producers)

Write requests atomically so the TUI never reads half a file: write to a temp
or hidden file **in the inbox directory** (same filesystem, so the rename is
atomic), then rename it to its final `*.json` name.

```sh
tmp=$(mktemp ~/.workingman/decisions/inbox/.req.XXXXXX)
cat > "$tmp" <<'JSON'
{"id": "deploy-now", "title": "Deploy now?", "summary": "",
 "choices": [{"id": "yes", "label": "Deploy"}, {"id": "no", "label": "Wait"}]}
JSON
mv "$tmp" ~/.workingman/decisions/inbox/deploy-now.json
```

In Go, `os.CreateTemp(inbox, ".req-*.tmp")` followed by `os.Rename` does the
same. Then wait for `<outbox>/<id>.json` to appear (rename into the outbox
means it is complete when it shows up).

### Which inbox files are read

Only regular files whose name (`decision.IsCandidateFile`):

- ends in `.json`,
- does not start with `.` (hidden) and does not end in `~`,
- does not contain `.tmp`, `.part`, `.partial`, `.swp` or `.crdownload`
  anywhere in the name (case-insensitive) — so `req.tmp.json` is ignored too.

Everything else, including subdirectories such as `answered/`, is ignored
silently.

### Malformed and partially written files

A candidate file that cannot be read, is not valid JSON or fails validation is
**not** answerable and never crashes the TUI. It is listed in the *Errors*
section of the list screen with the parse error (startup scan errors included)
and flagged on the status line when it appears while running.

While running, events for a file are debounced (100 ms of quiet), and a file
that fails to parse is re-read up to three more times (150 ms apart) before it
is reported, so a non-atomic producer that writes in a few chunks usually gets
through without an error. Fixing the file (writing a valid version) clears its
error and adds the request; deleting it clears the error. The same broken
content is reported only once.
