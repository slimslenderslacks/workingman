---
name: instagram-recipes
description: Extract a recipe from one or more short cooking videos — Instagram reels/posts, Facebook videos, fb.watch links — and file each as its own Logseq page, then link it from the "Instagram Recipe Intake" index page (newest first). Pulls the title, a 2-3 sentence description, ingredients, numbered steps, and the source URL from the caption, the comments, or the voiceover audio. Use when the user shares instagram.com/reel/..., instagram.com/p/..., fb.watch/... or facebook.com video links and wants them saved as recipes, or says "save this recipe", "add these reels to logseq", or runs /instagram-recipes.
---

# Instagram Recipe Intake

Turn short cooking videos into clean, cookable Logseq pages.

One video in, one Logseq page out, plus a dated reference at the top of the
**Instagram Recipe Intake** index page.

Instagram is the common case and names the skill, but the whole ladder below
works unchanged on **Facebook** (`fb.watch/...`, `facebook.com/<id>/videos/...`)
— same Meta backend, same `yt-dlp` support. Everything lands in the same
intake queue regardless of platform; note the platform on the page when it
isn't Instagram.

**Hard rule: never invent an ingredient, a quantity, or a step.** A recipe page
that looks complete but was half-guessed is worse than one that admits a gap.
If the source doesn't say how much garlic, write `garlic (amount not given)`.
See [Gaps](#gaps-and-honesty).

## When to Use

- User pastes one or more `instagram.com/reel/...`, `instagram.com/p/...`,
  `fb.watch/...` or `facebook.com/<id>/videos/...` links and wants them saved,
  filed, or "added to logseq"
- User runs `/instagram-recipes`
- User asks what's in their recipe intake queue

---

## Phase 0 — Preflight

**1. Confirm the Logseq MCP is present.** This skill cannot run without it.
Look for the `logseq` MCP tools: `listPages`, `getPage`, `upsertNodes`,
`searchBlocks`, `listTags`, `listProperties`.

If they are missing:
- **In a task sandbox** — the sandbox was created without the MCP. Say so and
  stop; the task needs `logseq` in its `static_mcps`. If the reels also need
  comment extraction, it needs `playwright` too.
- **On the host** — Logseq must be running with the MCP server enabled
  (Settings → Features → MCP server). Tell the user, don't try to start it.

**2. Confirm the graph.** `listPages` and check it looks like the cooking graph
(recipe pages, food words). The MCP serves whatever graph Logseq currently has
open — there is no graph-selection argument. If it's clearly the wrong graph,
stop and ask the user to switch graphs in Logseq first. Writing recipes into a
work graph is annoying to undo: there is no delete tool.

**3. Note today's journal title format.** From `listPages`, journal pages look
like `Oct 1st, 2026`. Use that exact format for date references. Confirm it
rather than assuming — don't hand-roll `2026-10-01` if the graph says otherwise.

---

## Phase 1 — Harvest the source

For each URL, work down this ladder and stop at the first rung that yields a
complete recipe.

### Rung 1 — WebFetch the caption

```
WebFetch(url, "Return the account handle and the COMPLETE caption text
verbatim, every line, including the full ingredient list and every
instruction step. Do not summarize, abbreviate, or replace any part of the
caption with a description of it. Then say whether the caption contains a
full recipe or points elsewhere (comments, bio link, substack, 'recipe below').")
```

Most food reels put the whole recipe in the caption, so this often finishes the
job in one call.

**Known pitfall:** WebFetch answers through a small model, and it sometimes
elides the very thing you asked for — returning
`"...[followed by complete ingredients and 11-step instructions]"` instead of
the steps. Treat any bracketed summary, ellipsis, or "etc." as a **failed**
fetch, not a thin caption.

**The fix is a blunter prompt, not a different rung.** The 15-minute cache
holds the fetched *page*, and the prompt is re-run against it, so asking again
in stricter terms costs one call and usually works:

```
WebFetch(url, "Output ONLY the raw caption text character-for-character as it
appears. Reproduce every ingredient line and every numbered step in full. Do
NOT write any commentary, summary, assessment, or bracketed note such as
'[followed by instructions]'. If you cannot reproduce the full text, output
exactly: CANNOT_REPRODUCE")
```

This recovered a full 11-step recipe that the softer prompt had collapsed into
one bracketed phrase. Only on `CANNOT_REPRODUCE`, or a second elided answer,
move to Rung 2.

Also treat as incomplete: "recipe in comments", "full recipe on my substack",
"link in bio", a caption that's pure vibes with no quantities.

**On Facebook, skip WebFetch and use `yt-dlp --dump-json`** — one call returns
`uploader`, `title`, `description`, `duration`, `webpage_url`, and the
`automatic_captions` languages. It is faster and more literal than WebFetch,
and it resolves an `fb.watch` shortlink to its canonical
`facebook.com/<page-id>/videos/<video-id>/` form, which is what belongs on the
page — shortlinks are shares and shouldn't be persisted. Facebook descriptions
are usually much thinner than Instagram captions (often just a filename), so
expect to continue down the ladder.

### Rung 2 — Read the comments

Creators very often drop the full recipe as their own first or pinned comment.

**Try WebFetch first — it does surface comments.** They are absent from the raw
HTML (`curl` will never see them), but WebFetch renders the page, and a
comment-targeted prompt returns the visible thread with handles:

```
WebFetch(url, "List every visible COMMENT on this post, with the commenter's
handle, verbatim. I am looking for a comment by the author (<handle>)
containing the recipe — ingredients and method. If no comments are present in
the content, reply exactly: NO_COMMENTS_VISIBLE")
```

Name the author's handle explicitly in the prompt — it's the one comment that
matters, and the rest of the thread is "looks delicious 😍". An author comment
that turns out to be only hashtags is a real answer: the recipe isn't there.

This reaches the first screen of comments, which is where a pinned or
author-posted recipe lives. Escalate to a browser only when you need to scroll
further or the page demands a login:

- **In a task sandbox:** the `playwright` MCP.
- **On the host:** the `claude-in-chrome` skill, which drives the user's own
  logged-in Chrome. Invoke that skill before using any `mcp__claude-in-chrome__*`
  tool. If the user has declined the extension, don't re-ask — treat it as
  unavailable for the session and carry on down the ladder.

Don't log in, don't dismiss consent walls by clicking through anything that
looks like an account action, and don't try to defeat a login requirement. If
the page demands auth and no logged-in browser is available, move on to
Rung 3 and Rung 4 — a narrated reel is still fully recoverable from its audio.

### Rung 3 — Follow an off-platform recipe link

If the caption points at a substack/blog/site, `WebFetch` that URL and take the
recipe from there. Record **both** URLs on the page: Instagram as the source
reel, the site as where the recipe text came from.

Finding the URL often takes a `WebSearch` first — captions say "recipe on my
substack" without linking it (`WebSearch("<handle> substack <dish name>")`).

**Expect a paywall.** "Recipe on my substack" is usually a conversion funnel,
and the post will show an intro plus the first ingredient before cutting off.
Record the URL either way, so the user can decide whether to subscribe. A
truncated post is not the end of the line, though: try Rung 4 before writing
the reel off, since the narration often gives the method the paywall is
charging for. Never reconstruct the rest from a generic version of the dish.

### Rung 4 — Transcribe the voiceover

Plenty of cooking reels carry the whole method in narration while the caption
says nothing. This is very much reachable, and it is often the rung that saves
a post the earlier rungs wrote off.

Nothing needs to be installed permanently. Pull the audio with a standalone
`yt-dlp` in the scratchpad, then transcribe locally:

```bash
cd "$SCRATCH"
curl -sL -o yt-dlp https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_macos
chmod +x yt-dlp

# cheap first: Facebook usually has auto-captions, Instagram usually doesn't
./yt-dlp --list-subs "<url>"
./yt-dlp --write-auto-subs --sub-langs en_US --skip-download -o clip "<url>"

# otherwise take the audio
./yt-dlp -f bestaudio/best -o 'clip.%(ext)s' "<url>"

# nix gives ffmpeg + whisper.cpp ephemerally, no profile change
nix shell nixpkgs#ffmpeg --command \
  ffmpeg -y -loglevel error -i clip.m4a -ar 16000 -ac 1 -c:a pcm_s16le clip.wav
curl -sL -o ggml-small.en.bin \
  https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-small.en.bin
nix shell nixpkgs#whisper-cpp --command whisper-cli -m ggml-small.en.bin -f clip.wav -nt
```

Notes that matter:

- **Platform auto-captions are a starting point, not the answer.** Take them
  when they exist (they're free), but they mangle exactly the words a recipe
  turns on. A real example: Facebook's captions gave `Ridiculio` and
  `flirtacell`, which Whisper resolved to **radicchio** and **fleur de sel**.
  Run Whisper too and prefer it wherever the two disagree on an ingredient.
- When a single word still looks wrong, re-run with beam search
  (`-bo 5 -bs 5`) and see whether the two decodings agree. Agreement is worth
  recording; persistent disagreement belongs in **Notes** as an uncertainty,
  not silently resolved in your favour.
- `whisper-cli` needs 16 kHz mono WAV — it will not take the `.m4a` directly.
- `small.en` (465 MB) over `base.en`: ingredient names and quantities are
  exactly what a smaller model garbles. A 90-second reel transcribes in
  seconds either way.
- `-nt` drops timestamps, which is what you want for prose.
- If `nix` isn't on the machine, ask before `brew install`-ing anything —
  that's a lasting change to the user's system, unlike the above.
- Everything (binary, audio, model) lives in the scratchpad. Don't put any of
  it in the repo.

A voiceover is speech, so expect no quantities at all — narration runs to
"a ton of arugula" and "cook until soft". Write it down that way. The texture
and timing cues the cook gives out loud (`until it's more porridge than soup`)
are the real content; capture those faithfully and mark every amount as not
given.

Record in **Notes** that the method came from the voiceover rather than any
written source, so the user knows why the quantities are missing.

### Rung 5 — Give up honestly

If no rung produced a recipe, **do not write a page**. Collect the misses and
report them at the end with the reason (`recipe is behind a substack paywall`,
`author never posted it`, `post unavailable`, `silent reel, no narration`).
Never substitute a generic internet recipe for the real one — the user wants
*that creator's* version.

If the method exists only as on-screen text rather than speech, Rung 4 won't
reach it; say so plainly rather than guessing from the caption's adjectives.

---

## Phase 2 — Shape the recipe

From the harvested text, produce five things:

| Field | Rules |
| --- | --- |
| **Title** | The dish, in title case. Use the creator's name for it when they give one (`Spicy Squash Rigatoni with Walnut Breadcrumbs`). Strip emoji, hashtags, and hype (`THE BEST EVER!!`). This becomes the Logseq page name, so keep it distinctive and human. |
| **Description** | 2-3 sentences, your own words. What the dish is, what makes it worth cooking, and any defining technique or timing. No hashtags, no `@` handles, no "this is a good one!". |
| **Ingredients** | One per line, quantity first, in the order the recipe uses them. Keep the creator's units. Preserve sub-groupings (`for the sauce:` / `for the breadcrumbs:`) as their own heading lines. Drop brand spam where it's incidental (`Farm Boy™ Tomato Paste` → `tomato paste`) but keep a brand when it's genuinely the point (`gochujang`, `Calabrian chili paste`). |
| **Instructions** | Simple numbered steps. Split run-on caption paragraphs into one action per step. Keep temperatures, times, and visual cues (`until fork tender`, `9-10 minutes`) — those are the recipe. Aim for 4-12 steps; if you're at 20, you're over-splitting. |
| **Source URL** | The **canonical** URL. Instagram: `https://www.instagram.com/reel/<shortcode>/` — strip `?utm_source=`, `?igsh=`, and especially `&stkn=`, since share tokens are scoped and expire. Facebook: the `facebook.com/<page-id>/videos/<video-id>/` form that `yt-dlp` reports as `webpage_url`, never the `fb.watch` shortlink. |

### Dedupe before you write

For each shortcode, `searchBlocks` for it (e.g. `Dd688TQvO58`). A hit means the
reel is already filed. Skip it, and say which ones you skipped. Also check the
proposed page title against `listPages` — if a *different* recipe already owns
that title, disambiguate with the handle: `Spaghetti Pomodoro (max.baroni)`.

---

## Phase 3 — The page template

Logseq DB graphs store a page as a **flat, ordered list of blocks**. There is no
nesting available through this MCP (`parent-id` is rejected as a disallowed
key), so the template is a flat sequence, and the headings are just blocks whose
text happens to be a markdown heading.

Blocks, in order:

```
1.  <the 2-3 sentence description>
2.  **Source:** [@<handle>](https://www.instagram.com/<handle>/) — [reel](<canonical url>)
    (Facebook: `**Source:** [<Page name> on Facebook](<canonical url>)`)
3.  **Captured:** [[Oct 1st, 2026]]
4.  ## Ingredients
5.  <one block per ingredient>
    ...
6.  ## Instructions
7.  **1.** <first step>
    **2.** <second step>
    ...
8.  ## Notes          (only if there is something to say)
9.  <one block per note>
```

**Why `**1.**` and not `1.`** — each block renders as its own markdown
fragment, so a block starting with `1.` becomes a one-item ordered list and
every step renders as "1.". Bolding the number instead gives correct, stable
numbering. This is not a style preference; `1.` is visibly broken.

A sub-grouped ingredient list gets its group labels as their own blocks:

```
## Ingredients
**for the spicy squash sauce:**
1 butternut squash, roasted + peeled
3 shallots, sliced thin
...
**for the walnut breadcrumbs:**
1/2 cup walnut pieces
...
```

The **Notes** section is where honesty lives: `Caption gave no quantity for the
olive oil.` / `Full recipe came from the creator's pinned comment.` /
`Creator's substack has a longer version: <url>`.

---

## Phase 4 — The index page

Every recipe gets a reference on **Instagram Recipe Intake**, **newest at the
top**.

Entry block format — one line, one block:

```
[[Spicy Squash Rigatoni with Walnut Breadcrumbs]] — @gabe_roberge_ · [[Oct 1st, 2026]]
```

### If the page does not exist yet

Create it with an intro block, then the entries newest-first:

```
1.  Recipes pulled from Instagram reels and other short cooking videos, newest first. Each entry links to its own page.
2.  [[Newest Recipe]] — @handle · [[Oct 1st, 2026]]
3.  [[Older Recipe]] — @handle · [[Oct 1st, 2026]]
```

### If it already exists — the shift

`upsertNodes` appends new blocks to the **bottom** and offers no ordering
argument, so "newest at top" has to be done by rewriting block text in place:

1. `getPage "Instagram Recipe Intake"`.
2. Sort the returned blocks by their **`order`** field, lexicographically
   (`a0` < `a1` < ... < `a6` < `a6G` < `a6V` < `a7`). Do **not** trust the array
   order you got back.
3. Drop the intro block; what's left is the existing entries, top to bottom.
4. Build the desired list: `[new entries, newest first] + [existing entries]`.
5. Emit `edit` ops rewriting existing block *i*'s `title` to desired entry *i*,
   for every existing entry block.
6. Emit `add` ops for the leftover tail (as many as you added), which land at
   the bottom.

Net effect: *k* new entries appear at the top, everything else slides down by
*k*, and nothing is lost. Cost is one edit per existing entry — fine for a list
of this size.

---

## Phase 5 — Write it

**`upsertNodes` is called once.** Its own description says at most once per
request, and it is a batch API — gather *everything* first (all pages, all
blocks, all index edits, for all reels in this run) into a single `operations`
array.

The one sanctioned exception: a `dry-run` pass immediately before the real call.
That commits nothing and catches schema errors while they're still cheap. So:
**two calls maximum, dry-run then real.** If the dry run fails, fix the payload
and dry-run again — never fire a real call at a payload that hasn't passed.

### Operation shape

JSON, with Clojure keywords written as plain strings:

```json
{
  "dry-run": true,
  "operations": [
    {"operation": "add", "entityType": "page", "id": "temp-rigatoni",
     "data": {"title": "Spicy Squash Rigatoni with Walnut Breadcrumbs"}},

    {"operation": "add", "entityType": "block",
     "data": {"page-id": "temp-rigatoni", "title": "A fall pasta where roasted butternut squash becomes the sauce itself..."}},

    {"operation": "add", "entityType": "block",
     "data": {"page-id": "temp-rigatoni", "title": "## Ingredients"}},

    {"operation": "edit", "entityType": "block",
     "id": "6a036e0b-8385-4a6b-babf-e61eedf53c44",
     "data": {"title": "[[Spicy Squash Rigatoni with Walnut Breadcrumbs]] — @gabe_roberge_ · [[Oct 1st, 2026]]"}}
  ]
}
```

Rules that bite:

- **`add` + new page:** give it a temporary string `id` (`temp-<slug>`) and
  reference that same string from each block's `page-id`.
- **`edit`:** `id` must be a real block uuid from `getPage`. A temp id won't do.
- **`page-id` must be a PAGE uuid, not a block uuid.** When appending to an
  existing page, take it from `getPage`'s `entity.uuid` — *not* from one of the
  blocks in the same response, which sit right next to it and look identical.
  **The dry run does not catch this**: a block uuid passes validation and
  reports the expected counts. Read the id out of `entity`, deliberately.
- **Block order is operation order.** Emit the blocks of a page in the order you
  want them read.
- **Unknown keys are rejected**, with a message like
  `{:data {:parent-id ["disallowed key"]}}`. Allowed `data` keys here are
  `title`, `page-id`, and `tags`.
- A successful dry run answers `"Dry run: Added: {:page 1, :block 2}."` —
  **check those counts** against what you expected before the real call.

### After the write

Report back, briefly:

- one line per recipe written: title → the reel it came from
- anything skipped as a duplicate
- anything that failed, with which rung it died on
- any `(amount not given)` gaps the user may want to fill in themselves

---

## Gaps and honesty

The whole value of this skill is that the Logseq page can be trusted at the
stove. Protect that:

- Missing quantity → `garlic (amount not given)`, never a plausible guess.
- Ambiguous step → keep the creator's words rather than smoothing them into
  something you invented.
- Recipe assembled from caption **and** comment → say so in **Notes**.
- Nothing usable found → no page at all, and a clear miss in the report.

Rewriting prose is encouraged. Inventing cooking facts is not.
