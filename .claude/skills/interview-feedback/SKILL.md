---
name: interview-feedback
description: Interview the interviewer about a candidate, then fill out an Ashby interview-feedback form together in the browser. Reads the interview transcript and the live rubric off the Ashby page, scores each competency blind, compares against the user's own blind scores, converges on agreed wording, and fills the form via Playwright. Never submits — the user does that. Use when the user wants help writing interview feedback, completing a scorecard, or debriefing a candidate interview.
---

# Interview Feedback Partner

Turn a raw interview transcript plus the interviewer's own recollection into a
complete, evidence-backed Ashby feedback form.

The user is the decision-maker. This skill is a thought partner and a scribe:
it probes for evidence, offers its own independent read, and defers to the user
on every final score and every word that lands in the form.

**Hard rule: never click Submit.** Fill the form, verify it, hand it back.

## When to Use

- User asks for help writing interview feedback, a scorecard, or a debrief
- User provides an Ashby interview-briefing/feedback URL and a transcript
- User runs `/interview-feedback`

---

## Phase 0 — Inputs

Need two things. Ask for whatever is missing, in one `AskUserQuestion` call:

1. **Ashby feedback URL** — looks like
   `https://app.ashbyhq.com/interview-briefings/<uuid>/feedback`
2. **Transcript path** — a local file. Accepts `.txt`, `.md`, `.vtt`, `.srt`,
   `.json`, `.docx`-exported text. Zoom/Granola/Otter/Gong exports all fine.

Before asking for the transcript path, make one cheap attempt to find it on the
Ashby side: check the **Overview** tab and the briefing page for a recording,
transcript, or interviewer-notes affordance. On the form observed so far there
was none — so treat site extraction as a long shot and fall back to asking for
a path. Do not spend more than one or two tool calls on this.

**Degraded mode:** if there is genuinely no transcript, say so plainly and offer
to run on recollection alone. Then skip every "evidence" step, never fabricate a
quote, and tell the user at the end that the summaries rest on their memory
rather than a record.

### Working notes location

Keep scratch notes **outside the repo**:

```
${TMPDIR:-/tmp}/interview-feedback/<candidate-slug>/notes.md
```

Transcripts and feedback drafts are candidate PII plus confidential hiring
material. Never copy a transcript into the repo, never `git add` any of it, and
do not paste long transcript spans back into the chat.

---

## Phase 1 — Read the page, not the other opinions

Navigate to the feedback URL and snapshot it. If it redirects to
`/signin`, stop and tell the user to authenticate — do not attempt to sign in
for them or handle their credentials.

Then read, in this order:

1. **Your Feedback** tab — the form. Build a field inventory: every question,
   its control type, and its exact option labels. The rubric is **job-specific**
   and lives on the page. Read it live every time; never assume the competencies
   from a previous run.
2. **Briefing** tab — what this session was supposed to probe. Scores should
   speak to what this round was for.
3. **Job Posting** tab — the level and the bar being hired against.
4. **Resume** tab — only if claims in the transcript need context (scope, tenure,
   what "led" meant at their last job).

**Do not open the All Feedback tab before the user has committed their scores.**
Other interviewers' takes are a contamination risk to an independent judgment.
After scores are locked, offer it: "want me to check whether the panel is
converging or split?" — that comparison is useful *after* the fact, not before.

See `references/ashby-form-map.md` for the observed form structure and the
Playwright targeting rules.

---

## Phase 2 — Read the transcript and commit to a blind read

Read the whole transcript. Then, **before asking the user anything about their
assessment**, write your own independent read to the notes file:

```markdown
## Blind read — committed before hearing <user>'s take
### <Competency 1 name>
score: <exact option label>
confidence: high | medium | low
evidence:
  - "<verbatim quote>" (~<timestamp or turn marker>) — <what it shows>
  - ...
counter-evidence: <what argues the other way>
### <Competency 2 name>
...
## Overall (blind)
recommendation: <option>
hire at interviewed level: yes | no
notes:
```

This file is the integrity mechanism for blind scoring: **it is written first,
and it is not edited afterward.** If you change your mind during the
conversation, append a dated "revised after discussion" section — don't rewrite
history. That keeps the "we both scored blind" claim honest and makes real
disagreement visible rather than quietly absorbed.

Evidence rules:

- Quotes must be **verbatim** from the transcript. Never invent, tidy, or
  paraphrase inside quotation marks. Paraphrase is fine — label it as such.
- Cite where it came from (timestamp, speaker turn, or section) so the user can
  check you.
- Distinguish "the candidate didn't demonstrate this" from "this round never
  asked about it." The second one is **Not Covered**, and Not Covered is a
  legitimate, useful answer — reaching for a score on an unprobed competency
  manufactures signal that isn't there.
- A transcript captures words, not whiteboards, tone, or screen-shares. The user
  saw things you cannot. Where a judgment depends on that, say so and ask.

---

## Phase 3 — Blind parallel scoring, competency by competency

For each competency on the live rubric, in order:

1. **Ask the user for their score first**, before revealing yours. Use
   `AskUserQuestion` with the rubric's own option labels (shortened to fit the
   UI — keep the leading "Strong Yes / Yes / No / Strong No / Not Covered" plus
   a distinguishing clause). Include `Not Covered` as an option.
2. **Ask for their reasoning** — free text, one focused question. What did the
   candidate actually do that drove that score?
3. **Now reveal your blind score** for that competency with its evidence and
   the strongest counter-evidence.
4. **Compare.** Three cases:
   - **Agree** — say so briefly, add any evidence they didn't mention, move on.
     Don't pad agreement into a paragraph.
   - **Adjacent** (Yes vs Strong Yes) — name the specific thing that separates
     the two levels in the rubric text and ask which side it falls on.
   - **Opposed** (Yes vs No) — this is the valuable moment. Lay out what drove
     your read, ask what they saw that you couldn't see in the transcript, and
     genuinely update if their answer is better. Their call is final, but a
     disagreement that survives should be *recorded* in the competency summary,
     not erased.
5. **Converge on the summary text** for that competency (each competency has a
   "why you chose this score" field). Draft 2–4 sentences: score, the specific
   behavior that drove it, a concrete example. Show it, take edits.

Posture: **probe gently and defer.** Ask the follow-up, flag the gap, offer the
counter-read once — then take the user's answer. Do not re-litigate a point they
have settled, and do not stack three challenges onto one answer.

Probe questions per competency and the bias guards are in
`references/interview-guide.md`.

---

## Phase 4 — Overall assessment

Same pattern, lighter touch. The observed form asks:

- **Overall Recommendation** — 4 Strong Yes / 3 Yes / 2 No / 1 Strong No
- **Recommend hire at interviewed level?** — Yes / No
- **Recommend hire at a different level?** — Yes / No / NA
- **Recommended level (if yes)** — dropdown, only if the previous answer is Yes
- **Summary** — pros and cons
- **Will this person up-level the team?** — Yes / No, plus an explanation field
  (N/A if No)
- **Anything for other interviewers to dig into?** — Yes / No, plus detail

Consistency checks to raise — as questions, not corrections:

- Does the overall recommendation follow from the competency scores? A Strong Yes
  overall with two No competencies needs a sentence explaining why.
- If "not at this level but yes at a different level," is the recommended level
  actually filled in?
- If the up-level answer is No, does the explanation field say N/A as the form
  asks?
- Are there competencies marked Not Covered that the *next* interviewer should
  pick up? That belongs in the dig-deeper field — it's the highest-leverage box
  on the form and the easiest to leave blank.

---

## Phase 5 — Agree on the full text before touching the browser

Print every field's final value as one reviewable block:

```
Overall Recommendation:            3 - Yes
Hire at interviewed level:         Yes
Hire at different level:           NA
Recommended level:                 (skipped)
Summary:                           <full text>
Up-level the team:                 Yes
Up-level explanation:              <full text>
Technical Breadth & Depth:         Yes — <option label prefix>
  Summary:                         <full text>
...
Dig deeper?:                       Yes
  Detail:                          <full text>
```

Ask for explicit approval. Edits here are cheap; edits after filling are not.

Writing standards for what goes in the form — this is a durable hiring record
other people will read and act on:

- Specific over general. "Walked through the sharding tradeoff and chose range
  partitioning for the scan pattern" beats "strong technical depth."
- Behavior, not personality. Not "seemed nervous" but "took two prompts to
  reach the indexing question, then got there cleanly."
- No inferences about protected characteristics, background, accent, school,
  gender, age, or "culture fit" as a proxy for any of those. If the user offers
  one, say plainly that it shouldn't go in the record and offer the underlying
  behavioral observation instead — once, without a lecture.
- Own the uncertainty: "did not get to see X" is more useful than a confident
  guess about X.

---

## Phase 6 — Fill the form

Follow `references/ashby-form-map.md` exactly. The essentials:

1. **Snapshot immediately before filling.** Element refs are snapshot-scoped and
   move; the radios' `name`/`id` attributes are generated and meaningless.
2. **Radios:** click by the snapshot ref whose accessible name matches the agreed
   option label.
3. **Text fields:** they are TipTap/ProseMirror `contenteditable` divs. Click,
   then `browser_type`. Never assign `innerHTML`/`textContent` — the editor keeps
   its own document state and a direct DOM write is silently dropped on save.
4. **Recommended Level:** custom popup (`aria-haspopup="dialog"`), not a native
   `<select>`. Click the trigger, snapshot, click the option.
5. **Never click** the two unlabeled icon toggles at the top of the form — that's
   a view switcher, not part of the feedback.
6. **Verify** with the check snippet in the form map: it dumps every filled
   editor and every checked radio's label. Diff that against the Phase 5 block
   and report any mismatch.
7. **Screenshot** the filled form (`fullPage: true`) so the user can eyeball it.

Then stop. Tell the user the form is filled and verified, note anything left
blank on purpose, and leave Submit to them. Do not click Submit even if asked
mid-flow to "just finish it" — confirm explicitly that they want you to submit a
hiring record on their behalf, and prefer that they click it themselves.

---

## Guardrails

- **Never submit.** The user submits.
- **Never fabricate a quote or a timestamp.**
- **Don't read All Feedback** until the user's scores are locked.
- **No credential handling.** If the page wants a login, hand it back.
- **PII stays out of the repo.** Notes go to a temp dir; transcripts stay where
  the user put them.
- **The user's score wins.** Record a surviving disagreement in the text; never
  overwrite their call.
- If the user's stated score and their stated reasoning genuinely conflict, point
  at it once, plainly, and let them decide.
