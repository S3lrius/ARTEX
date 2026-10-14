# Side-question long conversations and context budget

Fixed on 2026-09-11. The old implementation treated the character count of the request JSON as tokens and inherited the main task's 32K output reserve, so a normal side question was rejected early when HTML, JS, and tool results were large.

## Open-source implementation check

- [Grok CLI side context](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/agent.ts#L739): excerpts are taken from recent user and assistant text, with a character budget of about 2000 and at most 400 characters per item. That path does not keep a continuous side-question history.
- [Grok CLI independent request](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/utils/side-question.ts): its own cancel signal; output cap of 2048 tokens when the model supports it; no tools.
- [Grok CLI main-session compaction](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/compaction.ts): estimates tokens, keeps recent content, folds new content into the old summary, and handles truncation across turns.
- [OpenCode session compaction](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/compaction.ts): full-request estimate, output and buffer reserve, recent content plus a rolling summary, and a summary request with no tools. That implementation defaults to a recent budget of 8000 and a summary output cap of 4096 tokens.
- [OpenCode overflow recovery](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/runner/llm.ts): overflow recovery is attempted only before the assistant has started output, and the recovered call does not enter the same overflow-recovery path again.

ARTEX borrows the separate output budget, recent content plus a rolling summary, and limited recovery. It keeps norma v0.3.6 structured messages and tool pairing, and does not copy Grok's text excerpts. It does not write OpenCode's main-session compaction events into the ARTEX main transcript.

## Request budget and execution

- Messages keep norma's per-content-block UTF-8 byte estimate and the 4/3 margin. System prompt, tool schema, and message-framing overhead are counted too. The estimate is not the model's exact token count.
- Side-question output defaults to at most 8192 tokens, and also does not exceed the output cap already set on the main config. The service environment variable `ARTEX_BTW_MAX_OUTPUT_TOKENS` can set a cap from 256 to 32768. It does not change the product's default model or main-task parameters.
- The input budget is the context window minus the output cap and a safety margin. An unknown window uses the platform default of 200K. The safety margin is 5% of the window, at least 128 and at most 8192 tokens.
- Successful Q&A pairs are loaded in batches of at most 20, in increasing sequence order. At most 20 pairs are kept verbatim. Their token budget is at most one quarter of the input budget, and no more than 16K.
- Pairs over that budget are folded into the rolling summary. The summary carries historical source and context time. An earlier assistant answer is not the same as new tool evidence. When they conflict, prefer the latest main snapshot.
- When the main context is still too long, only older messages in the copy are summarized, and at most 8K tokens of recent content are kept. The cut point does not split a tool call from its result. A single pair that is too large goes into the summary as a whole.
- Summary input is split into UTF-8-safe chunks against the remaining window, with an output cap of 2048 tokens. An empty summary, a truncated summary, a tool call, or a summary over its budget is not cached. One side question makes at most 12 summary calls, under the same 120-second timeout. Hitting the cap fails explicitly and does not loop forever.
- If the model's first response is a context-length error and it has not yet emitted text or a tool call, the context is reduced further and retried at most once. If the estimated size does not go down, recovery stops immediately. Other model errors and partial streamed output do not trigger that recovery.
- All usage that was obtained, including summaries, failed attempts, and cancellation, is added onto the same side-question request. When the Provider returns no usage, only zero can be recorded. An estimate must not be presented as actual usage.

## Persistence and UI

`side_question_sessions.memory` stores the summary of older Q&A, the ordinal it covers, and the main-context summary cached by snapshot identity. `side_question_requests.context_info` stores the preparation stage, how many pairs were actually replayed, summary use, and the budget estimate.

A summary is saved only while the original request is still running and the clear version matches. Clear also drops the cache, and a late write does not restore cleared data. A new snapshot does not reuse an old snapshot's summary. Summary fields are stored with the v3 task archive. Restoring an older v3 that lacks the fields fills in empty objects. v1/v2 stay compatible.

POST accepts the request and returns it first. Preparation and compaction run in the background and do not hold the admission lock or a database transaction. SSE and history show preparation, Q&A compaction, copy compression, and the answer stage. A compaction failure is stored as that side request's failed terminal state. The frontend keeps the error and restores the draft of the failed question, and does not cover the input with a toast. History polling no longer clears the submit error.

## Verification record

- Replay of 19/20/21/50 pairs, keeping an old conclusion across 20 pairs, and reusing the summary cache after restart: automated checks passed.
- Very long answers and code context, chunked request budgets, tool pairing, an unchanged snapshot, and cache invalidation on a new snapshot: automated checks passed.
- Summary failure, cancellation, truncation, oversized output, and tool returns; clear races; the call cap; a single overflow recovery; and no retry of a partial stream: automated checks passed.
- Paging, restart, v1/v2/v3 archives, summary-cache and budget-metadata archive restore, an old v3 missing the new fields, and 20 parent sessions sharing four concurrency slots, on a separate PostgreSQL: passed.
- Go candidate service build, frontend TypeScript check, Biome check of the changed components, and a Next.js production build in a separate directory: passed.
- The built-in browser used a separate UI fixture at 1280×720 and 390×844 to check the compaction stage, the summary-scope hint, draft restore after failure, no floating error notification, no horizontal overflow, and an empty console. The temporary fixture was removed.
- A read-only replay of an existing local Worker snapshot passed the new budget check. For example, Worker #3's 293085-character snapshot is no longer rejected by a mistaken local character count. This item made no external model call.
- A real conversation test that would have sent a private Worker snapshot to Grok was refused by automatic approval review, was not run, and is not counted as a pass.
- At 2026-09-11 00:37 the local backend was restarted as requested, reusing the original database, data directory, and login config. The running file and the candidate binary have the same SHA-256. `/api/health` on the backend and on the frontend proxy both returned healthy.

Verification commands (separate test database only):

```sh
go test -race ./sidequestion ./db ./server -run 'TestSide|TestCheckpoint|TestSnapshot|TestBuildRequest|TestService|TestMainSide|TestTaskArchive' -count=1
go build ./cmd/artex
npx tsc --noEmit
npm run build -- --webpack
```

The frontend production build used a separate copy so it would not overwrite the `.next` directory of the current preview. The candidate service was at `/private/tmp/artex-btw-budget-candidate`, copied to `/private/tmp/artex-btw-preview/artex` and started. The previous binary was backed up in the same directory as `artex.before-context-budget`.
