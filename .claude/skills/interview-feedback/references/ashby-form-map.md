# Ashby feedback form — structure and Playwright mechanics

Observed 2026-08-13 on `app.ashbyhq.com/interview-briefings/<uuid>/feedback`
(ENG-IC: Technical Discussion, ML Engineer, Round Two).

**The rubric is job-specific. Read it live from the page every run.** This file
records the mechanics — which are stable — plus one worked example of a rubric
instance. Do not hardcode competencies or option labels from here.

---

## Page shape

Tabs, all at `/interview-briefings/<uuid>/…`:

| Tab | Path | Use |
|---|---|---|
| Overview | `` (bare uuid) | candidate + session metadata |
| Briefing | `/briefing` | what this round is meant to probe |
| Resume | `/resume` | context for transcript claims |
| Job Posting | `/job-posting` | level and bar |
| Your Feedback | `/feedback` | the form |
| All Feedback | `/all-feedback` | **other interviewers — do not open before user's scores are locked** |

Header carries candidate name, a LinkedIn link, the interview title, time-until
(`in 15h`), role, round, and a stage badge.

There is also a benign `status.ashbyhq.com` embed iframe on the page. Ignore it.

---

## Control inventory (worked example)

### Section: Overall Assessment and Recommendation

| Field | Control | Options |
|---|---|---|
| Overall Recommendation | native radio ×4 | `4 - Strong Yes`, `3 - Yes`, `2 - No`, `1 - Strong No` |
| Recommend Hire at Interviewed Level? | native radio ×2 | `Yes`, `No` |
| Recommend Hire at a Different Level? | native radio ×3 | `Yes`, `No`, `NA` |
| Recommended Level (if Yes) | **custom popup** | trigger button labeled `Select a value...` |
| Summary | TipTap editor | "Include pros and cons" |
| Will this person up level the team? | native radio ×2 | `Yes`, `No` |
| Up-level explanation | TipTap editor | "If no to the question above, put N/A" |

### Section: Rubric — 4 competencies

Each competency: native radio ×5 (`Strong Yes`, `Yes`, `No`, `Strong No`,
`Not Covered`) followed by a `<Competency>: Summary` TipTap editor.

1. **Technical Breadth & Depth** — fluency across domains, expert in at least one
2. **Leadership & Influence** — drives projects, influences without authority
3. **Communication & Storytelling** — articulates complex work, metrics, lessons
4. **Interaction & Collaboration** — how they engaged during the session

The `Strong Yes` / `Yes` / `No` / `Strong No` option labels are full paragraphs
of behavioral description. Their accessible name in the snapshot is that entire
paragraph — match on a distinctive prefix, e.g.
`Strong Yes - Demonstrates deep expertise in one or more technical domains`.

### Section: Final Thoughts

| Field | Control |
|---|---|
| Optional coding-exercise screenshots/notes | TipTap editor |
| Anything for others to dig into? | **`<button>` pair** `Yes` / `No` (not radios) |
| Dig-deeper detail | TipTap editor |

A single `Submit` button sits below a separator. **Never click it.**

---

## Targeting rules

### 1. Re-snapshot before every fill batch

`browser_snapshot` refs (`f5e320`) are scoped to the snapshot that produced them
and shift as the page re-renders. Snapshot, then act on those refs promptly.

### 2. Radios: match by accessible name, never by id or name

There were 31 native radio inputs. Every one of them:

```
name="labeled-radio-group-<N>"   value="on"   id="labeled-radio-input-<N>"
```

`N` is a render-order counter. It encodes nothing about which question or option
it belongs to, and it is not a stable contract. **Never build a selector from
it.** The accessible name in the a11y snapshot carries the option text — target
that ref:

```
radio "4 - Strong Yes" [ref=f5e162]   → browser_click target=f5e162
```

`browser_select_option` is useless on this page — there are **zero** native
`<select>` elements.

### 3. Text: 8 TipTap editors, click then type

All eight are `div.tiptap.ProseMirror` with `contenteditable="true"`. They show
a `Write here…` placeholder paragraph when empty.

**Do this:**

```
browser_click  target=<ref of the editor / its "Write here…" paragraph>
browser_type   target=<same ref>  text="<the agreed prose>"
```

**Do not** set `innerHTML`, `textContent`, or `.value` via `browser_evaluate`.
ProseMirror maintains its own document model and React state; a direct DOM write
either gets reconciled away or is dropped when Ashby serializes the field on
save. It can look correct on screen and still save empty — the worst failure
mode available here, because it produces a form that *appears* filled.

Editor order in the DOM, as observed (verify at runtime by matching the label
that precedes each one; use the index only as a fallback):

1. Summary (pros and cons)
2. Up-level explanation
3. Technical Breadth & Depth: Summary
4. Leadership & Influence: Summary
5. Communication & Storytelling: Summary
6. Interaction & Collaboration: Summary
7. Optional coding-exercise screenshots/notes
8. Dig-deeper detail

For multi-paragraph text, prefer one `browser_type` call per paragraph with a
`browser_press_key Enter` between, rather than embedding `\n` in a single call —
newline handling inside a rich-text editor is not guaranteed to produce the
paragraph break you expect. Single-block prose per field avoids the issue
entirely and reads fine in Ashby.

### 4. Recommended Level: custom popup

The trigger is `<button class="_0wgTsq_trigger" aria-haspopup="dialog">` reading
`Select a value...`. Click it, `browser_snapshot` to see the popup contents, then
click the option ref. Skip this field entirely unless "Recommend Hire at a
Different Level?" is `Yes`.

### 5. Two unlabeled toggles — never click

Next to the "Your Feedback" heading are two `<button role="radio">` icon
controls with no `aria-label`, no text, and no title (square/outline vs solid
variants). They are a view switcher, not feedback. They are the only
`[role=radio]` elements on the page — the 31 real ones are native `input`s — so
if a snapshot ref resolves to `role=radio` with an empty name, that's one of
these. Leave them alone.

---

## Verification snippet

Run after filling, before handing back. Diff the output against the agreed
Phase 5 block.

```js
() => {
  const editors = [...document.querySelectorAll('.tiptap.ProseMirror')]
    .map((e, i) => ({ i, len: e.innerText.trim().length, head: e.innerText.trim().slice(0, 70) }));
  const checked = [...document.querySelectorAll('input[type=radio]')]
    .filter(r => r.checked)
    .map(r => {
      const lab = document.querySelector(`label[for="${r.id}"]`) || r.closest('label') || r.parentElement;
      return (lab?.innerText || '').trim().replace(/\s+/g, ' ').slice(0, 70);
    });
  const level = [...document.querySelectorAll('button')]
    .filter(b => /Select a value|_0wgTsq_trigger/.test(b.innerText + b.className))
    .map(b => b.innerText.trim());
  return { editorCount: editors.length, editors, checkedCount: checked.length, checked, level };
}
```

Expected on a fully filled form with all four competencies scored: 8 editors with
non-zero length where text was agreed, and `checkedCount` of 9 (1 overall + 1
at-level + 1 different-level + 1 up-level + 4 competencies… = 8, plus any
additional radio question the live form adds). **Count against the agreed block,
not against this number** — the form varies by job.

Empty-but-intended-empty fields are fine; report them explicitly rather than
silently leaving them.

---

## Gotchas

- **Auth:** hitting `/feedback` unauthenticated redirects to
  `/signin?redirect=<encoded path>`. Sign-in offers Google, Microsoft, magic
  link, and SSO — all interactive. Hand this back to the user; don't type
  credentials.
- **Autosave is unconfirmed.** No save/draft indicator was found in the DOM.
  Assume nothing persists until the user submits, and don't rely on a partial
  fill surviving a reload.
- **The dig-deeper Yes/No are `<button>`s**, so they won't appear in a
  `input[type=radio]` sweep and the verification snippet won't see their state.
  Check them in the snapshot instead.
- **`in 15h`-style relative timing** in the header is the interview time, not a
  feedback deadline.
