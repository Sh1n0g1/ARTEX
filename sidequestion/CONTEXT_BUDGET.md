# Side-Question Long Conversations and Context Budget

Fixed 2026-09-11. The original implementation treated the character count of the request JSON directly as token count, and inherited the main task's 32K output reservation, so normal side questions were rejected prematurely when there was a lot of HTML, JS, or tool-result content.

## Open-Source Implementation Review

- [Grok CLI side-question context](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/agent.ts#L739): excerpts snippets from the most recent user and assistant text, with a character budget of about 2000 and at most 400 characters truncated per entry. This path does not maintain a continuous side-question Q&A history.
- [Grok CLI independent request](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/utils/side-question.ts): an independent cancellation signal; an output cap of 2048 tokens when the model supports it; no tools are provided.
- [Grok CLI main-session compaction](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/compaction.ts): estimates tokens, keeps recent content, folds new content into the old summary, and handles truncation across turns.
- [OpenCode session compaction](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/compaction.ts): full-request estimation, output/buffer reservation, recent content plus a rolling summary, tool-free summarization requests; this implementation defaults to a recent-content budget of 8000 and a summarization output cap of 4096 tokens.
- [OpenCode overflow recovery](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/runner/llm.ts): overflow recovery is only attempted before assistant output has begun; the retried call does not re-enter the same overflow-recovery path.

ARTEX borrows the practices of an independent output budget, recent content plus a rolling summary, and bounded recovery. It keeps norma v0.3.6's structured messages and tool pairing rather than copying Grok's text excerpting, and does not write OpenCode's main-session compaction events into ARTEX's main transcript.

## Request Budget and Execution

- Messages follow norma's per-content-block UTF-8 byte estimate with a 4/3 margin; the system prompt, tool schemas, and message-wrapping overhead are additionally counted. The estimate is not the model's exact token count.
- Side-question output defaults to at most 8192 tokens, and never exceeds the output cap already set in the main configuration. The service environment variable `ARTEX_BTW_MAX_OUTPUT_TOKENS` can be used to set a cap between 256 and 32768; this does not change the product's default model or the main task's parameters.
- The input budget is the context window minus the output cap and a safety margin; an unknown window falls back to the platform default of 200K. The safety margin is 5% of the window, with a minimum of 128 and a maximum of 8192 tokens.
- Successful Q&A pairs are loaded in batches of at most 20 groups, by increasing sequence number. At most 20 groups of raw text are kept, with a token budget of at most 1/4 of the input budget, capped at 16K.
- Q&A pairs beyond that are folded into the rolling summary. The summary carries its historical source and context time; a historical assistant answer is not equivalent to new tool evidence, and the latest main snapshot takes priority in case of conflict.
- When the main context is still too long, only the old messages in the copy are summarized, keeping at most 8K tokens of recent content; the cut point never splits a tool call from its result. An oversized single group goes into the summary as a whole.
- Summarization input is chunked UTF-8-safely according to the actual remaining window, with an output cap of 2048 tokens; an empty summary, a truncation, a tool call, or exceeding the summarization budget all skip writing to the cache. A single side question allows at most 12 summarization calls, subject to the same 120-second timeout; hitting the limit fails explicitly rather than looping indefinitely.
- If the model's first response is a context-overflow error and no text or tool call has been output yet, it is retried at most once after further reduction; if the estimated size does not decrease, recovery stops immediately. Other model errors and partial streaming output do not trigger this recovery.
- All usage obtained, including from summarization, failed attempts, and cancellation, is accumulated onto the same side-question request. When a Provider does not return usage, only zero can be recorded; an estimate must never be disguised as actual usage.

## Persistence and UI

`side_question_sessions.memory` stores the summary of old Q&A pairs, the ordinals it covers, and a main-context summary cached by snapshot identity. `side_question_requests.context_info` stores the preparation phase, the actual number of replayed groups, summarization usage, and budget estimates.

A summary is only saved while the original request is still running and the cleanup version matches; clearing also clears the cache, and a late write cannot restore already-cleaned data. A new snapshot does not reuse an old snapshot's summary. Summary fields are saved along with v3 task archives; restoring an old v3 with missing fields fills in an empty object, and v1/v2 remain compatible.

A POST first admits and returns the request, with preparation and compaction running in the background without holding an admission lock or a database transaction. SSE/history shows the preparation, collating-Q&A, compacting-copy, and answering phases; a compaction failure is saved as a failed terminal state for that side-question request. The frontend keeps the error and restores the draft of the failed question, without a floating notification covering the input box; history polling no longer clears a submission error.

## Validation Record

- Replay of 19/20/21/50 groups, retention of older conclusions across the 20-group boundary, summary-cache reuse across restarts: automated pass.
- Extremely long Chinese answers and code context, chunked request budget, tool pairing, snapshot immutability, cache invalidation on a new snapshot: automated pass.
- Summarization failure/cancellation/truncation/oversize/tool-call returns, clear races, call-count limits, exactly-once overflow recovery, and no retry on partial streaming: automated pass.
- Pagination, restart, v1/v2/v3 archiving, summary-cache and budget-metadata archive recovery, an old v3 missing new fields, and 20 parent sessions sharing four concurrency slots, all on an independent PostgreSQL instance: pass.
- Go candidate service build, frontend TypeScript check, Biome check on the modified components, and a Next.js production build in a separate directory: pass.
- Using the built-in browser with an independent UI fixture, verified at 1280×720 and 390×844 the collating phase, the summary-scope hint, draft restoration after failure, the absence of a floating error notification, no horizontal overflow, and no console errors. The temporary fixture has been removed.
- Read-only replay of an existing local Worker snapshot passed the new budget check; for example, Worker #3's 293085-character snapshot is no longer mistakenly rejected by a local character count. This item made no external model call.
- A real-conversation test that would have sent a private Worker snapshot to Grok was rejected by the automatic approval review this time, was not executed, and is not counted as a passing item.
- At 2026-09-11 00:37, restarted the local backend at the user's request, reusing the original database, data directory, and login configuration. The running binary's SHA-256 matched the candidate binary; `/api/health` on both the backend and the frontend proxy returned healthy.

Validation commands (using only the independent test database):

```sh
go test -race ./sidequestion ./db ./server -run 'TestSide|TestCheckpoint|TestSnapshot|TestBuildRequest|TestService|TestMainSide|TestTaskArchive' -count=1
go build ./cmd/artex
npx tsc --noEmit
npm run build -- --webpack
```

The frontend production build uses a separate copy to avoid overwriting the `.next` directory of the current preview. The candidate service is at `/private/tmp/artex-btw-budget-candidate`, which was copied to `/private/tmp/artex-btw-preview/artex` and started; the original binary is backed up as `artex.before-context-budget` in the same directory.
