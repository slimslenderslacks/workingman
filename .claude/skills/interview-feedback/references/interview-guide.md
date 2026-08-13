# How to interview the interviewer

The user sat in the room. You read a text file. They have signal you cannot
reconstruct — what the whiteboard looked like, how long a silence ran, whether a
hedge was modesty or uncertainty. Your job is to help them convert that into a
record another person can act on, and to notice where the transcript disagrees
with their memory.

Posture: **probe gently, defer.** One good follow-up beats three. Ask, listen,
offer your read once, take their answer.

---

## Conversation shape

Open with a single wide question before any rubric mechanics:

> "Before we go competency by competency — what's your overall gut on this
> candidate, and what's the one moment that shaped it most?"

The gut answer and the *moment* are both useful: the moment is usually where the
strongest evidence lives, and if the moment doesn't appear in the transcript,
that's the first thing worth reconciling.

Then run the per-competency loop from SKILL.md Phase 3. Close with the overall
questions, then the summary draft.

Keep your turns short. This is their debrief, not your analysis.

---

## Per-competency probes

Ask one or two, not all of them. Pick the ones the transcript leaves genuinely
open.

### Technical Breadth & Depth

- Where was the deepest point you reached — and did they get there on their own?
- Did they show fluency outside their main domain, or only inside it?
- When they hit something unfamiliar, what did they do?
- Did they teach you anything, or correct you?
- Any answer that sounded rehearsed rather than understood?

### Leadership & Influence

- On the project they described, what was actually theirs versus their team's?
  Listen for "we" that never resolves to "I."
- Did they change anyone's mind, or just execute a decision handed to them?
- How did they handle a disagreement they lost?
- Who did they develop? Any evidence beyond the claim?
- Did they drive across team boundaries or only within one?

### Communication & Storytelling

- Did you have to work to follow them, or did the structure carry you?
- Did they state the problem and the constraints before the solution?
- What were the success metrics — and did they know whether they hit them?
- How did they handle your hardest question?
- Would they land this story with a PM? With an exec?

### Interaction & Collaboration

- Did it feel like solving something together, or like a performance?
- What happened when you offered a hint — taken, ignored, argued with?
- Did they ask clarifying questions before diving in?
- Anything that would concern you about them on a team?

---

## When to say something

Raise these once, as a question, then let it go:

- **Score not supported.** "You've got Strong Yes on Leadership — the transcript
  has them describing the project but I didn't find a moment where they moved
  someone else's position. Was there something outside what got recorded?"
- **Halo effect.** One dazzling answer lifting every score. "The sharding answer
  was genuinely strong — does it tell us much about the collaboration score, or
  are those separate reads?"
- **Level mismatch.** Scoring against the wrong bar — a great senior against a
  staff loop, or vice versa. Check the Job Posting tab for what level this
  actually is.
- **Not Covered dressed as No.** The single most common scorecard error. "Did
  this round actually probe that, or did we just not get to it?" A `No` says the
  candidate lacks something; `Not Covered` says we didn't look. They are not
  interchangeable and the difference changes what the next interviewer does.
- **Recency.** The last ten minutes dominating a ninety-minute read.
- **Similarity.** Warmth toward a candidate who worked on familiar systems, took
  a familiar path, or reminds them of themselves.
- **Unfalsifiable language.** "Not a fit," "lacks polish," "seemed junior" — ask
  what the candidate did that produced the impression. If there's no behavior
  behind it, it shouldn't go in the record.
- **Protected characteristics and their proxies.** Accent, school, name, age,
  parental status, gender, background, "culture fit." State plainly that it
  doesn't belong in the record, offer the behavioral observation underneath it if
  there is one, and move on. No lecture, no repetition.

---

## Blind-score protocol

The point is that the user's judgment is formed before they see yours.

1. Your read is written to the notes file **before** the first scoring question.
   Not edited afterward — revisions get appended and dated.
2. Per competency: their score → their reasoning → **then** your score.
3. Reveal your score in full even when it's less favorable, and even when it's
   clearly going to lose. A blind protocol that quietly suppresses disagreement
   is worse than no protocol, because it launders agreement it didn't earn.
4. Update genuinely when their in-room evidence is better than your text
   evidence. Don't perform stubbornness — and don't cave to be agreeable either.
5. A disagreement that survives the conversation belongs in the competency
   summary, phrased as the user chooses. "Interviewer scored Yes; transcript
   review flagged limited evidence of cross-team influence" is a legitimate, and
   often valuable, thing for the panel to read.

---

## Drafting the text

Each competency summary: 2–4 sentences.

1. The score and the single behavior that drove it
2. One concrete example — ideally with a quote or a near-quote
3. The caveat, if there is one

> Yes. Solid depth in distributed storage — walked through the range-vs-hash
> partitioning tradeoff unprompted and picked range for the scan pattern, with
> the right reasoning about hot partitions. Outside storage they followed along
> but didn't drive; the ML-serving discussion stayed at a high level. Did not
> get to see them navigate an unfamiliar codebase.

The overall Summary field wants pros **and** cons. A summary with no cons is a
tell that the read isn't finished — ask for the reservation. Everyone has one.

The dig-deeper field is the highest-leverage box on the form and the one most
often left blank. Anything marked Not Covered, any unresolved doubt, and any
claim worth verifying belongs there, addressed to the next interviewer.
