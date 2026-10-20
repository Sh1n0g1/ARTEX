# `/btw` Validation Record

Date: 2026-09-10. Branch: `codex/btw-side-question`. Baseline: `8dae851b9b622f2ff2631f332fde9719d0b16fba`.

Uses an independent PostgreSQL test database and data directory; real model credentials were injected only into the isolated test environment, never written into code or this record, and the product's default model was not changed. Go 1.26.3, norma v0.3.6, Next.js 16.2.9.

Actual model conversations, returned objects, engineering assertions, and the raw Qwen review text are saved in [validation-2026-09-10.json](validation-2026-09-10.json), which contains no API credentials.

## Engineering Checks

| Scope | Result | Evidence |
| --- | --- | --- |
| Structured messages, deep copy of tool arguments | Pass | `TestCheckpointDeepCopyAndBoundaries` |
| Summarization / compaction requests do not overwrite, complete replies and terminal state are published, half-generated replies are excluded | Pass | `TestCheckpointDeepCopyAndBoundaries`, `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Actual model pool member identity | Pass | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Tool pairing, 20-group replay, budget trimming and overflow errors | Pass | `TestBuildRequestCompactionToolPairingAndBudget` |
| Main/side concurrency, bidirectional cancellation isolation | Pass | Blocking Provider, `TestMainSideConcurrencyAndIndependentCancellation` |
| No tool execution, streaming / non-streaming, usage already present on failure | Pass | `TestServiceNoToolsAndUsageOnFailure` |
| Real norma ChatAgent + local Read tool, main transcript / activity isolation | Pass | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`, streaming and non-streaming subcases |
| Persistence, pagination, idempotency, partial answer retained across restart | Pass | `TestSideHistoryIdempotencyPagingAndRecovery` |
| Clear vs. late-write races, parent resource deletion, version comparison | Pass | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent / Worker archiving and recovery, v1/v2/v3 | Pass | `TestSideTaskArchiveVersions` |
| Three parent endpoints, authentication, resource ownership, Worker soft delete | Pass | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`, `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Side questions work while the main session is busy, independent SSE reconnect / disconnect, cancellation, clearing | Pass | `TestSideHTTPBusyIsolationClearAndReconnect` |
| 1 per parent session / 4 global concurrency | Pass | Two `TestSideHTTP…` test cases |
| Snapshot persisted before submission, continuing questions after restart, old sessions cannot forge a snapshot | Pass | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Rejects continuation when the cached configuration is deleted or the model changes | Pass | `TestSideRejectsDeletedOrChangedCachedProfile` |
| Cancels and waits for the final answer and usage to persist before archiving | Pass | `TestSideTaskDrainPersistsBeforeArchive` |
| Usage and side-question attribution are recorded only once when a streaming consumer cancels early | Pass | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| A Worker/deadline run context automatically restored after restart continues to publish new snapshots | Pass | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| Race checks for related packages | Pass | The commands below |
| TypeScript and production build | Pass | `npx tsc --noEmit`, `npm run build` |
| Biome on newly added frontend modules | Pass | `biome check`, 3 new modules |

After configuring `ARTEX_PG_DSN` against a separate, disposable database, the automated checks can be reproduced (do not point it at the production database):

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

The full Go regression suite is not all green: two pre-existing tests in the `server` package fail during temp-directory cleanup, both reporting `TempDir RemoveAll … directory not empty`:

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

Re-running the `server` package in the same isolated environment from source exported off the unmodified baseline above also reproduces these two cleanup failures. The baseline run additionally showed a target-node-count assertion failure in `TestCoreTaskLifecyclePG`; the final `server` regression after the changes does not show that assertion failure. Other packages pass, and the side-question-related test cases and race checks in this effort pass. Baseline issues were not marked as passing for this acceptance, and no existing assertions were modified to hide an issue.

The Next.js build output shows the existing multiple-lockfile / workspace-root inference warning; the build completes and all pages generate successfully.

## Browser Checks

Used the Codex in-app browser, connected to an independent local Go service and the Next.js dev server. The following manual/automated operations were completed on desktop and a 390 × 844 narrow screen, checking screenshots and browser logs:

- Typed `/btw` while regular chat was running; main content and the side question displayed simultaneously; the desktop side panel worked normally.
- Asked consecutive follow-up questions; the already-generated part was kept after stopping the side question; the main flow continued.
- The request continued while the panel was closed, and the completed answer was restored on reopening; an empty `/btw` after a page refresh restored the history.
- On the narrow-screen Drawer, input, buttons, history, and closing all worked normally, with no horizontal overflow.
- Clearing uses a confirmation dialog; after clearing, the history disappears while the main transcript and snapshot are retained.
- Asked questions from the task MainAgent and two Workers separately and switched between them; Agent labels and history did not cross-contaminate.
- A blocking local-model fixture kept the Worker running; `/btw` was submitted from the Worker's main input box, and after stopping the side question the Worker still showed a live run and its own pause button, with the side question's partial answer saved.
- Browser error / warning logs were empty.

A controllable fixture was used to precisely verify concurrency timing, without depending on a real model's output speed. During debugging, two Worker runtime checks failed to form a valid concurrency window (the task had already ended / the answer finished early); after fixing the fixture, the checks were redone and passed. These initial attempts are not counted as valid passes.

## Real Model Conversations

First probed `grok-4.6` via the OpenAI-compatible endpoint `http://127.0.0.1:12580/tingly/openai`. The probe returned HTTP 200, the model name `grok-4.6`, and `READY`, taking 2.82 seconds. Since the preferred option was available, the Tingly `glm` or Zhipu `glm-5.3` fallback chain was not enabled; these two fallback services were not verified this time.

| Scenario | Actual result |
| --- | --- |
| Asked about the asset, objective, and marker while the main session was running | Returned `redhaze.top`, the objective of reading and summarizing the homepage, `BTW-REAL-0910`; side question completed in 16.97 seconds |
| Asked for the tool evidence after the main session finished reading the homepage | Correctly cited WebFetch 200, the curl redirect chain 301 → 302 → 200, and the page title; 7.24 seconds |
| Side question asked Bash to create a test file | Refused to execute; the target file was not created; 7.74 seconds |
| A completed side question does not change the main context | Main transcript SHA-256 remained consistent with the main activity log; side-question tool execution count was 0 |
| Follow-up question after actually stopping / restarting the Go service | Retained the previous 3 side-question history entries, answered the asset, marker, and title directly from the persisted snapshot, without rerunning the main Agent |
| New session using the Grok non-streaming configuration | Correctly answered the asset and `ATOMIC-0910`; returned and saved usage: input 11734, output 138, cache_read 11520 |

For the asset case, the main session used WebFetch and Bash/curl to read the public homepage; the landing page was `https://id.redhaze.top/home`, with the title “RedHaze Technology RedHaze Group · Global Comprehensive Group Portal”. Bash staged the response in a local test file; no write was performed against the remote end. This fact was verified separately from “the side question executed no tool.”

Main transcript checksum: `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`.

**Usage limitation:** Tingly's Grok streaming responses did not return usage. This was separately verified by directly sending `stream_options.include_usage=true`: HTTP 200, 12 data frames, 0 usage frames. So the 0 values in the streaming tests mean the endpoint did not supply usage, and should not be read as no billing. Non-streaming usage, as well as the fixture's failure / cancellation usage, were all saved correctly.

## Qwen Review

Review model `qwen-flash`, OpenAI-compatible endpoint `https://dashscope.aliyuncs.com/compatible-mode/v1`, HTTP 200. It was given the first three real side-question conversations, the main session's tool evidence, and the engineering assertions; it returned `verdict: accept`, `concerns: []`, judging the answers consistent with the asset, marker, and page-read evidence, and the side question's tool refusal compliant with the constraints. Review usage: prompt 6625, completion 312, total 6937.

This Qwen review did not cover the service restart and non-streaming tests added afterward. Qwen's generalization of “no writes” was too broad: the main session's curl did create a local temporary response file, as explicitly recorded above. Concurrency, zero tool execution, and transcript isolation are judged by engineering assertions; the model review only assists in assessing answer quality.
