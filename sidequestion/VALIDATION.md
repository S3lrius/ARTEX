# `/btw` verification record

Date: 2026-09-10. Branch: `codex/btw-side-question`. Baseline: `8dae851b9b622f2ff2631f332fde9719d0b16fba`.

A separate PostgreSQL test database and data directory were used. Real model credentials were injected only into that separate test environment. They were not written into code or this record, and the product's default model was not changed. Go 1.26.3, norma v0.3.6, Next.js 16.2.9.

The real model conversation, returned objects, engineering assertions, and Qwen's raw review text are in [validation-2026-09-10.json](validation-2026-09-10.json). That file contains no API credentials.

## Engineering checks

| Scope | Result | Evidence |
| --- | --- | --- |
| Deep copy of structured messages and tool arguments | Passed | `TestCheckpointDeepCopyAndBoundaries` |
| Summary / compaction requests do not overwrite; complete replies and terminal state are published; partial replies are excluded | Passed | `TestCheckpointDeepCopyAndBoundaries`, `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Identity of the actual model-pool member | Passed | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Tool pairing, replay of 20 pairs, budget trimming, and over-limit errors | Passed | `TestBuildRequestCompactionToolPairingAndBudget` |
| Main and side in parallel, cancellation isolated in both directions | Passed | Blocking Provider, `TestMainSideConcurrencyAndIndependentCancellation` |
| No tool execution, streaming / non-streaming, usage already collected on failure | Passed | `TestServiceNoToolsAndUsageOnFailure` |
| Real norma ChatAgent plus a local Read tool, main transcript / activity isolation | Passed | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`, streaming and non-streaming subcases |
| Persistence, paging, idempotency, restart keeps a partial answer | Passed | `TestSideHistoryIdempotencyPagingAndRecovery` |
| Clear racing a late write, parent resource deletion, version comparison | Passed | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent / Worker archive and restore, v1/v2/v3 | Passed | `TestSideTaskArchiveVersions` |
| Three parent APIs, authentication, resource ownership, Worker logical delete | Passed | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`, `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| A busy main session can still take a side question; independent SSE reconnect / disconnect, cancel, clear | Passed | `TestSideHTTPBusyIsolationClearAndReconnect` |
| 1 concurrent request per parent session / 4 global | Passed | Two `TestSideHTTP…` cases |
| Snapshot stored before submit, restart continues the question, an old session cannot forge a snapshot | Passed | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Refuse to continue after a cached config is deleted or the model changes | Passed | `TestSideRejectsDeletedOrChangedCachedProfile` |
| Cancel before archive and wait for the final answer and usage to be stored | Passed | `TestSideTaskDrainPersistsBeforeArchive` |
| When a stream consumer cancels early, usage is recorded once and attributed to the side question | Passed | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| A Worker / deadline run context restored after restart keeps publishing new snapshots | Passed | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| Race check of the related packages | Passed | Commands below |
| TypeScript and production build | Passed | `npx tsc --noEmit`, `npm run build` |
| Biome on the new frontend modules | Passed | `biome check`, 3 new modules |

After setting `ARTEX_PG_DSN` to a separate disposable database, the automated checks can be reproduced (do not point it at production):

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

The full Go regression is not all green. Two existing tests in the `server` package fail while cleaning a temporary directory, both with `TempDir RemoveAll … directory not empty`:

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

Exporting source from the unmodified baseline above and rerunning the `server` package in the same isolated environment reproduced those two cleanup failures. The baseline run also failed a target-node count assertion in `TestCoreTaskLifecyclePG`. The final `server` regression after these changes did not have that assertion failure. The other packages passed, and the side-question cases and race checks for this change passed. Baseline problems were not marked as accepted by this change, and existing assertions were not edited to hide a problem.

The Next.js build still prints the existing multi-lockfile / workspace-root inference warning. The build finished and every page was generated.

## Browser checks

Codex In-app Browser was connected to a separate local Go service and the Next.js dev server. Desktop and a 390 × 844 narrow screen completed the following manual automated steps. Screenshots and the browser log were checked:

- During an ordinary chat, typing `/btw` shows the main content and the side question at the same time. The desktop sidebar is normal.
- Follow-up questions in a row. After the side question is stopped, the generated part is kept. The main flow continues.
- Closing the panel does not stop the request. Reopening restores the finished answer. After a refresh, an empty `/btw` restores history.
- On a narrow screen, the Drawer's input, buttons, history, and close action work, with no horizontal overflow.
- Clear uses a confirmation dialog. After clear, history is gone, and the main transcript and snapshot remain.
- The task MainAgent and two Workers were each asked a question and then switched. Agent labels and history did not cross.
- A blocking local-model fixture kept the Worker running. `/btw` was submitted from the Worker's main input. After the side question was stopped, the Worker still showed a live run and its own pause button, and the side question saved a partial answer.
- The browser error / warning log was empty.

A controllable fixture was used to check concurrency timing precisely, without depending on a real model's output speed. During debugging, two Worker runtime checks never formed a real concurrency window (the task had already ended / the answer ended early). After the fixture was fixed, the check was repeated and passed. Those first attempts are not counted as a valid pass.

## Real model conversation

`grok-4.6` was probed first, through the OpenAI-compatible endpoint `http://127.0.0.1:12580/tingly/openai`. The probe returned HTTP 200, model name `grok-4.6` and `READY`, in 2.82 seconds. The first choice was available, so the Tingly `glm` and Zhipu `glm-5.3` fallback chains were not enabled. Those two fallback services were not verified this time.

| Scenario | Actual result |
| --- | --- |
| Ask for the asset, goal, and marker while the main session is running | Returned `redhaze.top`, the homepage-read and summary goal, and `BTW-REAL-0910`. The side question finished in 16.97 seconds |
| After the main session finished reading the homepage, ask for the tool evidence | Correctly cited WebFetch 200, curl redirects 301 → 302 → 200, and the page title. 7.24 seconds |
| The side question asks Bash to create a test file | Execution was refused. The target file was not created. 7.74 seconds |
| A finished side question does not change the main context | The main transcript SHA-256 stayed consistent with the main activity record. Side-question tool executions were 0 |
| Follow up after a real stop / restart of the Go service | The previous 3 side-question history rows were kept. The asset, marker, and title were answered directly from the persisted snapshot. The main Agent was not rerun |
| A new session using Grok's non-streaming config | Correctly answered the asset and `ATOMIC-0910`. Usage was returned and saved: input 11734, output 138, cache_read 11520 |

The asset case's main session used WebFetch and Bash/curl to read the public homepage. The landing page was `https://id.redhaze.top/home`, titled "RedHaze Technology RedHaze Group · Global conglomerate portal". Bash stored the response in a local test file. Nothing was written to the remote side. That fact was checked separately from "the side question did not run tools".

Main transcript checksum: `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`.

**Usage limit:** Tingly's Grok streaming response did not return usage. A separate direct request with `stream_options.include_usage=true` was checked: HTTP 200, 12 data frames, 0 usage frames. A 0 in the streaming test means the endpoint did not provide usage. It does not mean there was no charge. Non-streaming usage, and the fixture's failure / cancellation usage, were saved correctly.

## Qwen review

Review model `qwen-flash`, OpenAI-compatible endpoint `https://dashscope.aliyuncs.com/compatible-mode/v1`, HTTP 200. It was given the first three real side-question conversations, the main session's tool evidence, and the engineering assertions. It returned `verdict: accept` and `concerns: []`, and judged the answers consistent with the asset, marker, and page-read evidence, and the side-question tool refusal consistent with the constraints. Review usage: prompt 6625, completion 312, total 6937.

This Qwen review did not include the service-restart and non-streaming tests added later. Qwen's summary of "no writes" was too broad: the main session's curl did create a local temporary response file, which is recorded above. Concurrency, zero tool executions, and transcript isolation are judged by the engineering assertions. The model review only helps assess answer quality.
