# What the traces said

Notes from the first sweeps of the task set, written from `runs/*/trace.jsonl`
rather than from summary lines. Every claim here has a trace behind it.

Baseline: `deepseek/deepseek-v4-flash-0731` via OpenRouter, 34 tasks,
`$0.000038` per task, 126s for a full sweep.

---

## The first finding is that most failures were ours

The first sweep that produced any failures reported two. Neither was the model.

```
FAIL hard-chain-02  answer does not contain "1157.625"
FAIL hard-word-03   answer does not contain "74.5"
```

Reading those two traces produced three defects, all in the harness:

**`calc` truncated its own results.** `constant.Value.String` returns a "short,
human-readable form" of about six significant figures, so `1102.5 * 1.05` came
back as `1157.62` when the exact value is `1157.625`. `go/constant` holds exact
rationals — only the default formatter discarded the digits.

The worst part is visible only in the trace: the model **answered correctly
anyway**, having computed the value itself despite the tool handing it a wrong
one. A wrong tool result was laundered into a right answer. Nothing in the
summary could have shown that.

**The scorer rejected `$1,157.625`** because of the thousands separator, and
**`$74.50`** because the boundary matcher sees a digit after `74.5`. Both
answers were correct.

So: *three for three*. In the first sweep that discriminated at all, every
failure was a defect in the measuring apparatus rather than in the thing being
measured. A scorer that is too strict and a tool that is quietly wrong both
produce the same symptom — a failing task that looks like a model problem.

---

## Categories seen so far

| category | seen in | how it shows up |
| --- | --- | --- |
| **harness defect as model failure** | 3× in one sweep | a correct answer scored as wrong, or a tool returning a plausible wrong number |
| **model answers without the tool** | `mistral-nemo`, 3 of 6 tasks | correct answer, one step, no `tool_use` block — only `must_call` catches it |
| **model fails multi-hop chains** | `ling-3.0-flash` | passes every single-step task, fails the only two-hop one |
| **tool silently wrong** | `calc` twice | `^` is Go's XOR (`2^3 % 5` → `1`), and `String()` truncation |
| **formatting mistaken for meaning** | markdown, commas, trailing zeros | `**36**`, `$1,157.625`, `$74.50` |

The second row is the one that would have done the most damage. `mistral-nemo`
is the cheapest tool-capable model on the gateway, and it is cheap *because* it
answers arithmetic from its own weights and never calls the tool. Scoring on the
final answer alone would have ranked it first.

---

## A capable model hides a broken tool

The calculator's truncation bug was reintroduced deliberately, to check that the
suite would catch it. **It did not.** The pass rate stayed at 34/34.

The trace says why:

```
call   1000 + 1000*0.05      -> 1050
call   1050 + 1050*0.05      -> 1102.5
call   1102.5 + 1102.5*0.05  -> 1157.62     <- wrong, truncated
call   1102.5*1.05           -> 1157.62     <- re-checked another way, same wrong number
MODEL: ...answers 1157.625
```

The model got a wrong number, did not believe it, verified it a second way, got
the same wrong number, and then answered correctly from its own arithmetic. The
task passed. The tool was broken the whole time.

**An end-to-end pass rate cannot see a component defect that the model works
around.** That is a real limit of outcome-based scoring, and it is not obvious
until you watch it happen.

But the compensation was not free: three tool calls became four. That *is*
detectable, and `StepRegressions` now reports it — a task that still passes while
taking more steps than it used to is usually an agent working around something
that broke.

Two lessons, in order of importance:

1. **Score the path, not only the destination.** Steps and tool calls are
   signal. `must_call` was the first instance of this; step counts are the
   second.
2. **A unit test caught this bug in milliseconds and the suite never did.** Evals
   earn their keep on regressions unit tests cannot see — prompt changes, loop
   changes, model swaps. They are not a substitute for testing the components.

---

## This set is a regression suite, not a benchmark

The baseline now scores 34/34. That means it measures nothing about *this*
model: there is no headroom, so no change to the prompt or the loop can show up
as an improvement, and only a real breakage can show up as a decline.

It is still useful, but for one job rather than two:

- **as a regression suite** — it catches breakage. Reintroducing the `calc`
  truncation bug drops the score, and the history names the tasks that fell over.
- **not as a capability benchmark** — it cannot rank competent models, because
  they all score 100%. It does separate at the bottom: `mistral-nemo` scored 3/6
  and `ling-3.0-flash` 5/6 on the smaller early set.

Ranking frontier models would need tasks with real depth: five or more dependent
steps, or problems where the obvious first tool call is the wrong one. The
current set has `mean=2.4` steps and a maximum of 4.

---

## Two measurement hazards

**Pass rates are noisy.** `mistral-nemo` scored 4/6 and then 3/6 on identical
code and tasks twenty minutes apart, and the second run failed a *different*
subset. Any single sweep is one sample. Differences of one task are not
evidence, and a "regression" of one task may be nothing at all.

**Latency says nothing about difficulty.** Within one sweep, single-call tasks
took 1.3s and 18.2s. The variance is provider-side and swamps any real
difference, so nothing should be concluded from timing without many repeats.

Cost, by contrast, is stable and reported by the gateway rather than estimated
from a price table — it is the one number here that can be trusted from a single
run.

---

## What to do about it

1. Deepen the task set until the baseline sits near 70–85%, where a pass rate
   carries information.
2. Decide how many repeats a scorecard needs before a one-task difference means
   anything.
3. Keep the current set regardless: a suite that catches the `calc` truncation
   bug is worth having even if it cannot rank models.
