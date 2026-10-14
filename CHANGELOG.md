# Changelog

Important changes to this project are recorded in this file. The format follows [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/).

## [Unreleased]

## [0.3.15] - 2026-10-07

### Interception

#### Added

- **New built-in intercept rule "delete-style API paths"**: the built-in destructive HTTP rules previously recognized only the DELETE **method**, while most applications expose delete APIs that fire on GET/POST. A call such as `curl 'http://t/api/user/delete?id=1'` would actually delete the target's data. A new `deny` rule covers `/delete /del /remove /unlink /erase /destroy` (the verb must be followed by a separator, so `/delivery`, `/details`, and `/delta` are not blocked by mistake). Existing installs receive it on upgrade, and it can be disabled or deleted under "System → Command intercept".
- **Token use of the model fallback approval can be viewed on its own**: approval calls previously had no separate metering, so the cost was a guess. They are now metered on their own. Under the "model fallback approval" switch on "System → Command intercept" there is a usage card: call count, input, output, and cache read/write, plus a mini bar chart of daily usage for the last 30 days.

### Vulnerability notifications

#### Added

- **Findings can be pushed to IM**: a new "System → Notification push" page can push scan findings to **DingTalk, Feishu, WeCom, a generic webhook, Telegram, and email**. One type can have any number of instances, and each instance has its own enable switch, rate limit, and filter rules.
- **Delivery is either realtime or a digest**: realtime sends each hit as it happens; digest mode merges them into one message on a global interval (default 30 minutes), with the new count and severity breakdown at the top. For "critical in realtime, everything else in a digest", create two channels and configure them separately.
- **Filter rules have four dimensions**: minimum severity, limited task / asset scope, vulnerability-type keywords to include and exclude (exclude wins), and whether disposition changes are received (off by default). Messages include a "View details" button. The target address comes from the global "back-link address"; leave it empty and the button is omitted.
- **Delivery history and manual resend**: each delivery shows status, attempt count, failure reason, and channel, and can be filtered by channel and status. Failed items can be resent in one click (a resend clears the retry count).
- **Channel credentials are shown masked**: webhook URLs, signing secrets, bot tokens, SMTP passwords, and similar values are echoed only as a mask with a hint of the tail. Submitting the mask unchanged means "do not modify". Clearing the input deletes that field.

#### Fixed

> This section is the result of a security audit after the first version of the feature. Each item was reproduced, then fixed, then covered by a regression test.

- **Fixed a mask bypass that changed the target address while keeping the credentials (critical)**: config merge kept the stored value for any key that was not mentioned, so changing only the address and saying nothing about the credentials made the server send the real stored credentials to an arbitrary address, silently. A target-address change now requires every credential field to be stated explicitly: a new value, or an explicit empty value meaning it is no longer needed.
- **Fixed markdown channels that did not escape untrusted content**: vulnerability titles and summaries come from model output, and asset URLs come from scan results. A crafted title rendered as a clickable external link in DingTalk / WeCom / Feishu, and image syntax made the client fetch the image and leak the reader's IP. Content is now collapsed to one line and markdown metacharacters are escaped, and the escape happens at each channel's render exit rather than in the shared title function.
- **Fixed SSRF on the delivery address**: loopback, private-network, and cloud-metadata addresses could all be delivered to, and the first 200 bytes of a failure response body went into `last_error` and were shown in delivery history, which is a semi-blind read primitive. The check is now at **dial time**, covers DNS rebinding, and rejects cross-host redirects. Loopback and link-local addresses require `ARTEX_NOTIFY_ALLOW_LOCAL=1`. RFC1918 private networks are still allowed (an internal SMTP relay is common).
- **Fixed push credentials leaking through error messages**: DingTalk's `access_token`, WeCom's `key`, and Telegram's bot token are all in the URL, and a failed delivery used to put the full URL into `last_error`, delivery history, server logs, and the frontend message. Those are now redacted to `scheme://host` plus the underlying reason.
- **Fixed a truncated digest that marked the whole batch delivered and silently dropped the rest**: vulnerabilities cut off were neither in the message nor in the failure list, while delivery history showed success. Packing is now **per message**. Only the items that fit are marked delivered. The rest go back on the queue for the next message and do not consume a retry. The message header also says how many will continue in the next one.
- **Fixed a misspelled minimum-severity threshold that silently disabled the filter**: a typo such as `min_severity` = `hgih` had unknown-severity rank 0, so the check became always true. The user thought only high severity was delivered, and every vulnerability was poured into the group. The write path now validates the value and lists the allowed values.
- **Fixed `rate_per_min = 0` (unlimited) being unreachable**: the database write layer quietly turned an explicit 0 into the default, so the operator thought the limit was off while it was still applied. The default is now filled at the API layer when the field is omitted.
- **Fixed digest channels bypassing the token bucket entirely**: `rate_per_min` previously had no effect on digest mode. The number claimed each round is now bounded by both this round's quota and the in-memory cap.
- **Fixed failure handling that judged a whole batch by its maximum attempt count, so old deliveries dragged new ones down**: a delivery that had already been retried twice pulled brand-new deliveries in the same batch into failed, and a new vulnerability was lost forever without using a single retry. Each row is now decided on its own.
- **Fixed deliveries in a digest batch whose snapshot could not be parsed being silently marked successful**: those rows were skipped at render time but still counted as success with the batch. They are now an explicit failure with a reason.
- **Fixed a single channel sending more rows per round than the lease allows**: under multiple instances, a row whose lease expired was claimed again, sent twice, and had its attempt count incremented twice. The per-round cap is now derived from "lease / single-attempt timeout", and an assertion pins the relationship between those constants.
- **Fixed Telegram truncation that could cut an HTML entity in half**: truncation avoided a half-open tag but not a fragment such as `&amp`, and a parser can reject the **entire** message because of that. Both are now avoided.
- **Fixed the email channel treating a temporary SMTP failure as permanent**: a greylist `450` and other 4xx codes are temporary refusals and should be retried later. They used to be treated as dead, so a server with greylisting failed every push on the first attempt. The first digit of the reply code now decides: 4xx is retryable, 5xx is permanent.
- **Fixed nested structures that really wrote the mask literal into the database**: an object field such as `webhook.headers` can only be masked or submitted as a whole. Stuffing the mask sentinel inside the object was stored as a real value and auth failed silently. That submission is now rejected explicitly.

#### Design notes

- **The transaction that writes a vulnerability does one blind INSERT**: `notification_events` and the vulnerability are written in the same transaction, so commit makes them atomic. That INSERT deliberately does not read the channel table and does not run the user's filter rules, and it is wrapped in a `SAVEPOINT`. Otherwise one misconfigured filter could abort the transaction and keep a critical vulnerability out of the database.
- **Delivery claims rows with a lease instead of a long transaction**: `FOR UPDATE SKIP LOCKED` claims a row, sets it to `sending`, and pushes `next_attempt_at` into the future as the lease. No database lock is held during delivery. Rows left behind by a crash are claimed again after the lease expires. They heal themselves and do not retry forever.
- **Rate limiting does not consume the retry budget**: this round's quota is computed from the token bucket first, then that many rows are claimed. The reverse order would count a rate-limited delivery as an attempt. After three budgets were spent purely waiting, it would fall into failed.

### Test infrastructure

#### Fixed

- **Fixed company ICP ownership tests whose cleanup order left assets behind forever**: `defer d.Close()` ran first when the function returned, and `t.Cleanup` ran after it, so every cleanup statement hit an already closed connection. The error was discarded, and test assets and companies stayed in the database forever, which also made other tests that assert asset counts fail for no obvious reason. Closing the connection now also goes through `t.Cleanup` and is registered first, and a cleanup failure is visible.

### Traffic

#### Added

- **The traffic list can search content, filter by path / status code / length, and sort columns**: the filter row can filter by **response body** (full-text index), **path**, **status code** (an exact value or a bucket such as `2xx`), and **response length range**. Headers can sort by time, status code, and response length, and the sort preference is stored locally. Two new indexes are added and take effect automatically on existing installs. The agent tool's query contract is unchanged.
- **The traffic list has "Clear all"**: it deletes every traffic record in one step (the current filter is ignored) and cleans historical host directories the index no longer records. After the clear it does a full compaction and reports how much space was actually freed. Traffic evidence already bound to a vulnerability lives in a separate evidence store and is not affected.

#### Fixed

- **Fixed disk space that was not released after traffic was deleted**: the index database was created without `auto_vacuum`, so the file never shrank. The full-text index is `contentless_delete`, so `DELETE` only writes a tombstone and does not reclaim the original postings. Deleting traffic made the index grow (measured: 6MB of body → a 16MB index, still 16MB after everything was deleted). New index databases now use `auto_vacuum=incremental`. After each delete, a background job merges the full-text index in chunks and reclaims incrementally, returning space to the system (the same scenario falls back to 104KB). The write lock is released between chunks, so traffic recording is not blocked.

> Upgrade note: `auto_vacuum` can only be set when the database is created, so **an existing install's index database stays in the old mode**. Incremental reclaim is a no-op there (startup prints one line about it). After the upgrade, tombstone growth on those databases has stopped. Reclaim the space already used once with "Clear all". That step converts the database to incremental reclaim, and ordinary deletes free space after that.

### LLM

#### Fixed

- **Fixed custom session headers that did not apply on the "one-shot LLM call" path**: objective decomposition (round 0) and cold-node compaction are one-shot calls with no transcript store. The context has no session id, so the gateway never saw the header. Endpoints such as opencode zen that return 400 when the header is missing showed up as "round 0 fails, later planner rounds are completely fine". Both paths now explicitly attach a session id that is stable for the exploration, which also attributes token usage to that exploration (previously it was not recorded at all).

### Tools

#### Fixed

- **Fixed output truncation that did not apply to MCP and custom tools, so a large result flooded the context**: MCP tool results were not truncated at all. HTTP custom tools were passed an empty tool context and always used the hardcoded 6000 characters instead of the session limit. Both paths now go through `actool.Capture`: truncate at the session `MaxOutputChars` (default 30000), and when `ToolOutputDir` is set, spill the full output to disk and leave only the opening fragment and a file pointer in the message.
- **Upgraded norma to v0.4.3**: the harness layer always runs `CaptureOnce`, so any tool (including ones added later) can no longer overflow the context by missing truncation.

### Deployment

#### Fixed

- **Fixed the activity stream failing to connect behind a reverse proxy**: the SSE address was unconditionally built as `${hostname}:8787`. On a deploy proxied to 443, the page loaded from `https://domain/` while SSE pointed at port 8787, which was not open to the public internet, and the activity stream kept reconnecting. Production now defaults to the same origin, based on `NODE_ENV`. Only `next dev` keeps `:8787`, to bypass Next.js buffering of SSE. The `NEXT_PUBLIC_SSE_BASE` override remains. The README now includes reverse-proxy deploy notes.

### Findings

#### Fixed

- **Fixed the "by asset" view whose asset list overflowed with no scrollbar**: the outer card had only `max-height` and no definite height, so the scroll viewport's percentage height could not be resolved. With many assets the list either broke out of the card or was clipped and could not scroll. The height cap now sits directly on the asset tree's native scroll container and follows the window height.

### Database

#### Fixed

- **Capped the connection pool**: `database/sql` does not limit how many connections the application itself may open. When the pool has no idle connection it opens a new one unconditionally, all the way up to PostgreSQL `max_connections` (default 100), and only then is it refused. The cap is now 32. Queries beyond that wait for an idle connection. The same load gets slower instead of erroring. If `max_connections` is lowered, lower this cap to match.
- **Raised connection reuse**: the default `MaxIdleConns` is 2. Past two concurrent connections, a connection is closed as soon as it is finished, and the next request repeats the TCP and PostgreSQL authentication handshake. The idle cap now matches the total cap, with a 30-minute connection lifetime and a 5-minute idle eviction, so the pool can naturally replace long-lived bad connections.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.14] - 2026-09-24

### Task list

#### Added

- **The task list has a "Vulnerabilities" column**: counts are shown by severity as critical / high / medium / low. Non-zero buckets are colored by severity, so the size and spread of each task's vulnerabilities is visible at a glance.

#### Changed

- **The "Description / Target" columns are narrower**: long content is truncated, and hovering shows the full text, so long text takes less horizontal space.

### Exploration graph and planning overview

#### Changed

- **The planning overview (`graph_overview`) is slimmer, and each list has a count cap**: recent facts, finished intents, pending intents, cold-zone summaries, and confirmed finding details are now "the latest window plus a count fallback". Anything not shown can be queried on demand. That cuts the LLM context each round and keeps long tasks from inflating the context (the cold zone of a related-task overview is capped the same way).

#### Fixed

- **Folding cold nodes removes the reverse duplicate edge between a digest and a finding on the exploration graph**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.13] - 2026-09-19

### Asset interception

#### Added

- **Global asset interception rules (blocklist)**: exact and fuzzy domain / IP / URL matches, plus CIDR ranges. Rules can be created, updated, deleted, and enabled or disabled. The page is "System → Asset intercept". Fuzzy blocks for government (`.gov` / `.gov.cn`) and education (`.edu` / `.edu.cn`) sites are built in by default.
- **Asset interception is on the execution path**: before an agent dispatches an intent (`add_intent`) or inserts assets (`insert_assets`), the target assets are checked. An intent that hits a block is not dispatched, an asset that hits a block is not inserted, and the agent is told the asset and the reason.
- **Per-task block / allow (allowlist) rules**: independent of the global rules and effective only for this task. The order is "block first, then allow". A block hit is forbidden. If nothing is blocked, the task has allow rules, and none of them hit, the result is "testing not allowed". If no allow rules are configured, the allowlist is not used. Rules can be entered when the task is created, and created, updated, deleted, and enabled or disabled on the task "Overview".
- **Task templates can preset a category and per-task block/allow rules**: a template can store the task category and a set of per-task rules, and applying the template fills them into the new-task form.

### Operation review

#### Fixed

- **Tightened the model-judge output protocol so truncation causes fewer accidental allows**: the decision comment cap dropped from 500 Chinese characters to 120 Chinese characters, and the model is required to "output JSON only, with no preamble or code fence". That avoids a decision truncated by `MaxTokens` that cannot be parsed and is then allowed by the model-failure policy (fail-open).

### Task archive

#### Fixed

- **A symlink in an archive is skipped instead of failing the whole bundle**: the archive format only supports ordinary files and directories end to end. Any symlink in the work directory used to fail the entire task archive. That symlink is now skipped and logged, and the other files are archived (links are not followed, and the walk does not leave the directory tree).

### Accounts and compliance

#### Added

- **A "terms of use and disclaimer" dialog before login**: the box must be checked before login is allowed.

### License and dependencies

#### Changed

- **The project is licensed under AGPL-3.0**, and the README license and disclaimer notes are filled in.
- **norma upgraded to v0.4.1**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.12] - 2026-09-17

### Exploration feed

#### Added

- **The task detail page has an "exploration feed"** (#144): a timeline of the task's exploration nodes (start / goal / intent / fact / finding / hint / compaction). Filter by type, search by keyword, sort ascending or descending, page, and auto-refresh. The "first page with newest first" is the live position and refreshes every round. Leaving that position only accumulates an unread count and does not disturb the current reading, with "N new items · back to latest" to return in one click. Items are grouped by day, so paging a long task still shows which day it was.
- **Each feed row shows the node id** (#145), so it can be matched to the exploration graph and a specific node can be found.
- **Expanding a feed row shows upstream, downstream, and anchored assets** (#147): the node's upstream (came from) and downstream (produced) links. Hovering a related item pops that node's card (type / status / source / time / summary / payload fragment), and lists the assets anchored to the node (type label + recognizable text). That data is sent with the feed page, so expanding does not make another request.
- **Feed search can filter by node id** (#150): besides "content / source", the search box matches node id. A plain number or the "#41" form shown in the UI locates that node.

### Intent management

#### Added

- **Deleting an intent supports "soft delete / hard delete"** (#149): pending, running, and paused intents can be deleted, and a reason is required. The mode is chosen in the confirm dialog.
  - **Soft delete (default)**: the intent is set to "deleted" and the reason is stored in its own field. The intent node and everything it produced, including lineage, are kept.
  - **Hard delete**: the intent is removed physically, along with exclusive descendant nodes "supported only by it" (cascading along the output / intent chain to the leaves), so orphan data is not left behind. Token metering for the deleted intent is archived on the original dates. Shared nodes (still referenced by other intents), goals, and the task root fact are always kept. The hard-delete dialog shows how many nodes the cascade is expected to affect.

  Both modes tell the planner "the user deleted this intent, and why", and the planner replans from that.

### Operation review

#### Added

- **Approval records can be filtered by status and decision source** (#139): filter by approval status and decision source to find a record quickly.

#### Fixed

- **Pending approval requests load independently of history pagination** (#133): pending requests are no longer affected by paging the history list, and stay fully visible while history is browsed.

### Agent

#### Changed

- **The worker role description is now generic wording for a network security platform** (#138).

#### Fixed

- **Fixed cold-node compaction that never fired**: the per-task planner is now wired to the Compactor. Cold-node compaction (cold-digest) never ran because it was not connected. It runs again.

### Chat

#### Added

- **Chat supports @ mentions of several record types, with scroll pagination** (#135): several kinds of records can be @ mentioned, and mention candidates load as you scroll.

#### Fixed

- **Long message bubbles stay inside the session panel** (#137): an overly long bubble no longer overflows the session area.
- **Fixed "missing upload file" when uploading before a new conversation exists**: the composer's file picker snapshots the `FileList` into an array before clearing the input, then calls back. A draft previously had to create the session asynchronously first. By the time execution resumed, the live `FileList` bound to the input had been cleared, the upload had no file field, and the backend returned 400.

### Traffic

#### Fixed

- **The traffic-recording proxy listens on 127.0.0.1 by default** (#129, #130): the default configuration no longer exposes an open proxy on other network interfaces.

### Web

#### Fixed

- **Fixed the task list page of the static export losing the global header**.
- **Filled in the demo vulnerability-detail related-traffic mock, fixing the blank screen**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)
- [@dingpotian](https://github.com/dingpotian)

## [0.3.11] - 2026-09-15

### Operation review

#### Added

- **Approval records can jump to their source** (#125): clicking "source" on an approval record returns to the tool execution that triggered it. The original full session opens, pages load automatically to the target, the command and result expand in place, and the row is centered and highlighted in the scroll area while the messages around it stay readable. Manual scrolling stops the auto-correction. Ordinary chat, task workers, the planner, and segmented main-agent sessions are all located precisely through the persisted `tool_use_id` and the task mapping (a new in-scope call-id index). Missing, duplicate, or ambiguous links show an explicit message instead of jumping to a different execution. A deleted session, or a task that was archived or whose record is missing, shows a clear message.
- **Approval records can be paged** (#115).

#### Changed

- **The model-review input is slimmer** (#124, #125): model review (the LLM judge after no rule hits) now receives a versioned JSON of "this one complete tool call + a clearly chosen short background + this machine's working directory". The background takes only a real user message (the current user message of chat / the task main agent). Workers no longer attach an intent summary, and background inherited from a parent agent is cleared. The planner and auto-triggered sessions do not invent a user message. The task description, goals, operation constraints, the global exploration situation, history calls, and the worker's full intent are no longer attached. Those still follow the original logic for agent execution and separate session audit; they just do not enter operation review. Every decision must output JSON `decision`/`comment`, and the comment must include "the actual operation, the consequence if it succeeds, and the rule that hit". Instructions inside parameters or the background cannot change the review policy. The input actually sent to the model, and its fingerprint, are saved. Old snapshots are kept and marked with a version. Old inputs are not reconstructed from current data.

#### Fixed

- **A model decision wrapped in a code fence is no longer silently allowed** (#126): if the decision JSON came back inside a ``` fence, a parse failure used to be silently allowed by the model-failure policy. The fence is now stripped before parsing. Only a parse that still fails follows the configured failure policy.

### Agent

#### Added

- **Experimental noa context compaction** (norma upgraded to v0.4.0): an experimental switch in system settings, off by default. When it is on, noa (model-driven context compaction) takes over context compaction for the main agent, planner, worker, and chat, replacing the built-in compactor. The original text is archived under `<workDir>/noa/<session id>/` (not scattered across task directories), one directory per session id, globally unique. If the integration fails it falls back to the built-in compactor and does not interrupt a real task. The switch is read once per run, so changing it affects only runs started afterward.

#### Fixed

- **Exploration-graph tools refuse when there is no task context, instead of a nil crash**: calling an exploration-graph tool outside a task context returns a clear error instead of crashing on a nil store dereference.

### MCP

#### Added

- **Legacy SSE MCP servers are supported** (#117): compatible with MCP servers that only offer the old SSE transport.

### Traffic

#### Fixed

- **Traffic search matches a record's host with the port in mind** (#114): host matching in `traffic_search` includes the port, so records for the same host on different ports do not collide.
- **A failed `traffic_search` description migration no longer stops later reporter migrations**: one failed migration is isolated and does not block the ones after it.

### Web

#### Changed

- **Retest vulnerability options have a divider** (#122): a separator between retest vulnerability choices makes them easier to scan.

#### Fixed

- **Fixed the demo task-detail page crashing the whole page**: in mock mode the task detail "Sessions" tab requests `GET /api/tasks/<id>/side-questions` (side-question history), but the mock handler had no such route. The read fallback treated "a path ending in s" as a collection and returned `[]`, so `data.items` was `undefined`. The side hook's `merge()` iterated it and threw `TypeError: t is not iterable`. That exception happened inside the `setItems` updater, was rethrown by React during render, the caller's `catch` could not see it, and the error boundary took the whole page with "This page couldn't load". The mock handler now returns an empty side-question history explicitly, and `sideAPI.history` normalizes the return value (`items` that is not an array becomes `[]`) as defense in depth.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.10] - 2026-09-13

### Network

#### Added

- **Web search has an official DeepSeek source**: it reuses the currently active LLM configuration. It is different in kind from the other three sources. DeepSeek has no search API that can be called directly. Search exists only inside its Anthropic-compatible API (the `web_search_20250305` server tool) and is executed by DeepSeek's servers. It **supports only the official DeepSeek endpoint plus the anthropic protocol** (an OpenAI-protocol endpoint rejects server tools outright). Each search **consumes an extra model call**, the request **does not go through the search egress proxy**, it **is not recorded in traffic**, and the result **has only a title and a link** (no snippet; use WebFetch when the body is needed). The settings page explains those limits and **does not validate or block**. The user confirms whether the setup qualifies, and the "test search" button can run one for real.

### Agent

#### Added

- **Workers can look back across works**: `search_all_worker_traces` (no intent_id required; keyword search across every work's execution trace in this task, returning the steps that hit) and `get_worker_trace` (after locking one work, list its step stream, search it in place by keyword, and fetch full content by step_id). Use them to reuse an observation another work saw but did not write into a fact, and to avoid repeating the work.
- **`node_detail` is available to the worker**: together with the look-back tools, once a worker has an intent_id or a node id it can query that node's full detail directly.
- **A planning round triggered by `add_hint` is announced explicitly**: a new hint used to be folded into the situation overview for the planner to notice on its own. One `add_hint` now records a trigger for the planner, "a person added N strategic hints: …" (a batch counts as one, not one line per hint). The planner is told "this round was triggered by a new hint" and sees the hint text directly.

#### Changed

- **The global situation prompt is looser**: it encourages exploration to fan out and cross-intent clues to be reported in time, rather than converging too early.
- **The worker boundary wording is shorter**: being blocked the first time does not mean the direction is exhausted. Focus on "finish the bypasses inside this intent before concluding".
- `insert_assets` no longer takes a `related` argument per asset: its only effect was whether to add that asset to this task's scope, the value was not stored (registering the same asset again voided it, and the UI did not show who was judged unrelated), and in practice it only made the model do a judgment that would not stick.
- **Test scope (`task_scope`) is no longer affected by the asset-coverage switch**: automatic scope entry from `insert_assets` (`source='auto'`) runs whether coverage is on or off, and `add_task_scope` is always available to the planner, the in-task main agent, and the objective-decomposition agent. Scope is the task's authorization boundary and the filter baseline for asset queries. The coverage switch only decides whether to use it as the denominator of a metric. It should not decide whether scope itself is accumulated. Turning coverage off used to disable both the `auto` and `agent` write paths, leaving `task_scope` with only rows added by hand in the UI. `list_untested_assets` is still hidden when coverage is off (it is a pure coverage view).

#### Fixed

- **Fixed assets leaking across tasks** (#59): `list_assets` used to hardcode the task id as 0 and query the whole shared asset library with no way for the model to write a scope filter, so another task's assets (especially IPs) were treated as this task's targets and the test direction drifted. It now returns only assets inside the test scope (`task_scope`) of **this task and directly related tasks**, matched by **ownership** rather than literal value: a root domain in scope finds every subdomain, service, and endpoint under it, and a network range in scope finds hosts and services inside it. Fetching an out-of-scope asset by id also fails. A non-task context (Auto/pentest) has no scope to apply and still falls back to the whole library. IP-literal hosts (for example `http://1.2.3.4/api`) that a range scope could not own are fixed as well. The UI "test assets" view (filtered by task producer) is unchanged.
- **Fixed worker cross-work look-back tools that were unbound by mistake**: after `search_all_worker_traces` / `get_worker_trace` (and `node_detail`) were added to the worker's default tool set, an old migration that "narrowed the worker tool surface" unbound them again at startup, so the worker never actually had them. They were removed from that unbind list, and databases that already ran the old migration are bound back onto the worker once.
- **Fixed occasional `/btw` side-question insert failures**: before a side_question checkpoint is stored, NUL (`\u0000`) escapes that JSONB does not support are stripped, so content containing that character does not fail the write.

### Triggers

#### Fixed

- **A merged trigger session deduplicates task description and goals per task**: when several tasks fire one merged trigger, the same task's description and goals are no longer repeated in the message, so a long goal does not stack and blow up the context.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.9] - 2026-09-11

### Agent

#### Added

- The in-task main agent supports **multiple sessions**: create, switch, and reset context independently. Each session can be used.
- Persistent `/btw` **side questions**: ask without interrupting the main line. The Q&A is stored and is still there after a restart.
- `spawn_task` has `source_task_ids`. A new task can **read-only inherit** the source task's assets and findings.

#### Changed

- Cross-engagement memory for the worker is off, and its default tool surface is narrower: reading context and reviewing across works belong to planning. The worker only executes and writes back its one intent.
- The worker's default prompt is shorter. `get_worker_output` no longer has `terminated` and `worker_name`. The descriptions of `insert_assets` / `list_assets` are shorter.

#### Fixed

- Fixed work ending early because of an **idle turn**: the model sometimes spends a whole turn on thinking only, with no body and no tool call. The harness sees a natural end (`end_turn` and no `tool_use`) and closes with `completed` plus an empty summary. An intent that is not finished stops halfway, which looks like "the task ended normally but there is no written summary". None of the five LLM retry layers can reach it: it is not an error, the same-provider safe-window retry starts from "stream failed", the circuit breaker treats `err == nil` as success, and an intent rerun only recognizes `model_error`. The SDK empty-response retry cannot reach it either, because that layer treats "did it yield an event" as non-empty, and a thinking delta is itself an event. The work Stop hook now recognizes this turn and injects a continue instruction, so the model keeps going with the thinking it already produced. "Resend the same request" was deliberately not chosen: this idle turn is usually decided by the prompt and the shape of the context. It is stable behavior, not a random glitch, and resending only makes the model think it through again.
- The retry count reuses the **empty-response retry count** on the LLM page under "Retries and backoff" (both answer "the model finished but produced no real content"; the criterion and the mechanism differ). The default is 2. `-1` turns it off and restores the old "an idle turn ends the run" behavior. It is a **total** cap for one intent, not a consecutive count: the harness already limits consecutive idle turns to a single nudge, and the quota refreshes only after a real tool turn, so this number stops a loop of "tool → idle → nudge → tool → idle" from burning the intent budget. When it fires, the log records `[work <worker> · #<intent>] idle turn… inject continue (n/N)`. The design is in `the LLM retry design note` §1.1.
- Fixed the `/btw` context budget and input layout in long conversations. A non-secure context (not accessed over HTTPS) falls back to generating the side-request id.

### Traffic

#### Added

- A vulnerability can be **linked to multiple traffic evidence items**, which can be ordered, annotated, and distinguished by role (request / response / supporting).
- The report agent **links related traffic evidence automatically** before it writes the report.
- An agent **traffic-binding switch**, and evidence handoff between agents, are in place.

### Tasks

#### Added

- **Session-level vulnerability retest**: the retest runs in its own agent session, and the list and detail show the running state.

### Interception

#### Added

- Approval records have **detail and an execution audit**: the tool-request context, the model/rule first judgment, the execution output, and the parameter fingerprint.

### Assets

#### Added

- Task test assets support **DSL search**.

### LLM

#### Fixed

- Fixed the connection test not sending the custom session header, which made opencode zen return 400.

### Network

#### Added

- MCP HTTP transport can **skip TLS certificate verification**, so a service with a self-signed certificate can be connected.

### UI

#### Added

- The session list is grouped by agent, and groups can be expanded, collapsed, and pinned on their own. Chat can be filtered by agent.
- The attack-chain graph renders compaction (digest) nodes and collapses their members.

#### Fixed

- Fixed a blank screen caused by login credentials getting out of sync.
- Intercept copy now says "platform control" explicitly, so it is not misread as a defense on the target side.

### Deploy and update

#### Added

- **One-click update in the UI**: the system settings page has a "Version and updates" card, and the top bar shows a notice when a new version exists. The flow is download the release → check `SHA256SUMS` → smoke test → stage → exit, and the supervisor starts it again and finishes the swap. The page refreshes on its own.
- Supervisor scripts `start.sh` / `start.bat` are the official start entry (release archives and the Docker image include them; `install.sh` is unchanged). The exit code decides whether to start again, and SIGTERM is forwarded to artex. Verification and the binary swap stay in Go. The script stays simple.
- Failure is contained: a failed checksum or smoke test discards the staged file and keeps the current version. If the new version fails to start 3 times in a row, it rolls back to the previous version. The settings page also has a manual rollback (the database schema does not roll back).
- Updates accept only GitHub hostnames and force HTTPS. The release source is not configurable. Dev builds cannot one-click update. GitHub query results are cached for 30 minutes so the top-bar notice does not exhaust the API quota.

#### Known limitations

- Under Docker, only the program is replaced, not the image: the toolchain does not upgrade with it, and recreating the container returns to the version baked into the image. Use `docker compose pull artex` when that is needed.
- `skills/` inside the release archive is not synced, so a built-in skill added in a new version does not take effect automatically.
- An update is a restart and interrupts running tasks.

### Dependencies

#### Changed

- norma upgraded to v0.3.7.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.8] - 2026-09-09

### LLM

#### Added

- The LLM page has a "Retries and backoff" tab: the **count** and **interval** of all five retry layers can be set. A failed model call passes, from the inside out, through connect retry (SDK; connection reset / timeout / 429 / 5xx before the stream starts), empty-response retry (SDK; a normal end with no content, openai format only), same-provider safe-window retry (replay a broken stream before any output was delivered to the caller), polling circuit breaker (consecutive failures past a threshold cool down and skip), and intent rerun (after a worker ends with model_error, the whole intent runs again). An outer layer runs only after the inner one is exhausted. Each layer has two knobs, with the same meaning: empty = the original default count and exponential backoff; a count uses that count; an interval replaces exponential backoff with a fixed interval; `-1` turns that layer off. The first three layers follow the endpoint and can override the global default field by field on each model configuration (an interval-only override still inherits the global count). The circuit breaker and the intent rerun are process-level and exist only as one global setting. Saving takes effect without a restart. Leaving everything empty is the current behavior. An old database is byte-for-byte unchanged after upgrade (new columns default to 0; a missing settings key means all defaults). The connection test deliberately does not take these parameters. It has a 30s hard timeout, and stacking user-configured retries would turn a working endpoint into a timeout failure. The design is in `the LLM retry design note`.
- Each LLM configuration can set a custom **session header** (`session_header_key`): when it is non-empty, every LLM request sends that HTTP header, and the value is the current run's session id (`conv-<id>` for a chat session, `exp<x>-worker-i<intent>` for a worker, and so on). Some gateways use a session-id header for prompt cache or sticky routing. The session id is read from the request context and injected, so norma does not need a change, and the same shared provider can send a different header value per session. Old databases get a migration. The frontend LLM configuration dialog has a new input.

#### Fixed

- Fixed saving an LLM configuration that failed with `23502` because the session-header field was empty: the column is `NOT NULL DEFAULT ''`. It was incorrectly wrapped in `NULLIF($n,'')`, so an unfilled session header was written as NULL and hit the not-null constraint. The empty string is now passed through as an empty string.

### Agent

#### Added

- Wall-clock timeout now **finishes in place**: depends on norma v0.3.6. When `MaxDuration` is reached, the running tool is interrupted and the live ctx continues for the wrap-up rounds (write back what was identified, plus a summary; the terminal state is timeout). It no longer depends on each worker/planner's external hard ctx of `maxDur+90s` killing a stuck run straight into `aborted_tools`. Chat uses the same harness and gets the same behavior. The main agent has no `MaxDuration` and is unaffected.
- Stall fallback: when the planner is woken by a heartbeat or by "nothing changed", and the whole graph has no open or running intent, the opening becomes a stall warning. It states that no worker is running and no direction is queued, and this round must produce one or more new intents that do not repeat each other (zero intents is not allowed).

#### Changed

- The worker prompt is restructured: the intent, the start instruction, and the anchored asset's raw JSON move into the system prompt, are rebuilt every round, and are never dropped by compaction. A continuation no longer depends on the first transcript message surviving. The starting user message is thinned to only the global situation overview (it may be degraded, and stale is tolerated). The cost is that the system prompt mixes in per-intent data and loses cross-intent cache reuse. That is a deliberate trade for "do not lose the intent".
- The default planner / worker body is shorter, and several field problems are corrected: the planner distinguishes `recent_done` by status (blocked/exhausted checks the trace before deciding; it is not a dead end and it is not rerun blindly). A negative conclusion is "an observation / in doubt, not a verdict", and evidence is read before it is believed. If the goal is not met and there is no open/running intent, an intent must be produced (a hard floor). Depth comes before coverage. A worker's negative conclusion writes only "observation + a tentative reading". The verdict belongs to the planner. Cross-intent clues go into the fact summary for the planner; the worker does not chase them itself. Reseed appends the new default as a new version and switches to it. User customizations and old versions stay in history and can be rolled back.
- The work agent's default tool set and the asset write-back prompt are narrowed: the worker only executes and writes back one intent. Reading context and reviewing across works belong to planning, so `list_facts` / `node_detail` / `list_companies` and the cross-work search tools (`search_all_worker_traces` / `list_worker_traces` / `get_worker_trace`) are removed from its defaults. What remains is `list_findings` (dedup before reporting a vulnerability) + `add_finding` / `record_fact` + `insert_assets` / `list_assets`. The prompt drops stale field notes that conflicted with the `insert_assets` schema (`type=tech` / `on_url` / `props`). Old databases get a one-time migration that strips those bindings. Bindings of the same name on the planner and main agent are left alone.

### Exploration graph

#### Added

- **Cold-node compaction on the exploration graph (cold-digest)**: old intents and facts that have been inactive for a long time are folded into digest nodes and shown in `graph_overview`. The original nodes are kept forever and can be restored in full by id (storage is lossless; only the presentation is compressed; folding is reversible). Hot vs cold is reverse reachability, and any live branch stays hot, with R=6 rounds of debounce and connected-component grouping. Background compaction (minor folds uncovered cold blocks; major recompresses from the source and merges fragments) rechecks activity and uses cooldown mutual exclusion. It never runs on the hot path and never overwrites a node that has come back to life. The overview provides `cold_digests` and an asset-indexed `cold_index`. `expand_digest` / `expand_index` restore them. Overviews of related or inherited tasks reuse their own folded view, and `expand_digest` supports cross-task read-only restore. `expand_digest` / `expand_index` are given to the planner and the main agent, not the worker. Old databases get a migration.
- `graph_overview` outputs the full `finding_list`: findings are the highest-value product of a task and a single task usually does not have many, so the overview now carries all of them (unlike facts, which only get the recent window). The planner and worker can see every confirmed vulnerability each round without calling `list_findings`. Each item is slimmed to `{id, summary, evidence?, from_intent?, assets?}`, and affected assets are readable content (url / domain / ip:port) rather than a bare id.

#### Changed

- `graph_overview` no longer lays out `hosts`: the coverage block drops the host list (a wide task carried up to 500 host strings every round, which is of limited value for planning) and keeps only `host_count`. Specific hosts are queried with `list_assets` when needed. A top-level `done_intents_total` (total finished intents) sits beside `recent_done_intents`, which is truncated to at most 15, so the planner knows whether anything was cut when it dedups.

### Tools

#### Changed

- The default binding of `add_company_scope` moved from the worker to the planner: defining a company's asset scope belongs to planning / main control / Auto. The worker only explores. New databases seed the default binding as mainagent/planner/auto. Old databases get a one-time migration.
- The planner binds `list_assets` by default: it now has both `list_assets` (DSL search of the whole library) and `list_untested_assets` (untested inside scope). Old databases are backfilled once, and a user unbind is not overwritten.

### Tasks

#### Added

- The task list has a "Running workers" column: the count of intent nodes in `state='running'` for that task, the same definition as "Running workers" on the task detail overview. It shows 0 when none are running.

### Network

#### Added

- A **global egress proxy**: all target traffic can leave through one global proxy (http/https/socks5, with `user:pass`). When traffic capture is on, it is the upstream of the MITM recording proxy (traffic is still recorded, then goes out through the proxy; both the intercept path and the passthrough path use the upstream, and the source IP is not leaked). When capture is off, it is injected directly into the agent's bash environment and WebFetch (`proxyEnv` adds `ALL_PROXY` for socks5). The setting is stored in the settings KV table (no migration). The frontend system settings page has a "Global proxy" card, independent of the web-search proxy and the LLM proxy.

### UI

#### Fixed

- Corrected the meaning of intent status labels: `exhausted` "exhausted" → "budget exhausted" (it actually hit the step or time budget and was cut off midway, writing back only part of the result; the direction was not fully explored). `blocked` "blocked" → "execution error" (model / API / network failures exhausted their retries, and the intent was barely explored; it is not a target or WAF block). The missing `stopped` "stopped" label was added (work the user stopped by hand).

### Dependencies

#### Changed

- norma upgraded to v0.3.4: MCP tool output is truncated and spilled to disk (see `651b961`). v0.3.6 later supports finishing a wall-clock timeout in place.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.7] - 2026-08-31

### LLM

#### Added

- Model configuration has an "output limit", and you can choose which request field name carries it: the limit caps how many tokens one reply may generate and is sent with every request. 0 (the default) means the field is not sent and the server default decides. It is not the same thing as the "context window". That is the model's total capacity and is used only locally to compute the compaction threshold. It does not appear in the request. The field-name switch matters only for the `openai` (Chat Completions) format: empty (the default) sends `max_tokens`, which almost every compatible gateway recognizes. Official OpenAI reasoning models (the o series / GPT-5) recognize only `max_completion_tokens` and return `unsupported_parameter` if they receive `max_tokens`. Those endpoints must be switched to the new field by hand. The other formats have a fixed field name (Anthropic is `max_tokens`, the Responses API is `max_output_tokens`), so the switch is disabled for non-openai formats and cleared on save. A path that was never connected is wired up at the same time: the planner, worker, chat, main agent, and objective decomposition never set an output limit. The openai family did not send one at all, and Anthropic used the SDK's 8192 fallback. Adding the setting without connecting that path would mean a number you typed never took effect. The value is now resolved each round from the configuration, the same way as "streaming", and takes effect on the next round after a failover switch. Behavior is unchanged after an old database is upgraded (the new column defaults to 0 and an empty field name).
- Clicking "Test" in the background logs each HTTP attempt's status code and the gateway's raw response body into the server log (the body is cut to 4K), so 401s, quota text, empty frames, and HTML pages can be diagnosed instead of only the folded ok / err in the UI. This log does not depend on the "LLM recording" switch.

#### Changed

- The task LLM configuration chain can be edited in any state. It is no longer limited to "running / paused / chain exhausted": after a task ends (done / failed / timeout) the main agent conversation still uses this chain, and when a model on the chain broke you previously could neither change it nor keep talking. The terminal-state block is removed from both the backend HTTP handler and the DB transaction. The frontend dialog also allows edit and save on a terminal task. Saving a terminal task no longer reopens quota-blocked intents (those intents would be moved to open, with no worker to run them, and they would no longer meet the conditions of "rerun intent"). To keep going, rerun an intent or add a goal. Those pull the task back to running.

### Agent

#### Changed

- "Send a message to a running worker to change its direction" now reuses the existing pause / resume + transcript resume mechanism, and behaves like the main agent conversation. It previously used a custom intervention persistence protocol that touched the scheduling barrier, the resume flow, and more than ten activity-query filters. The message is now injected as the next round's input. The intent runs directly in its own goroutine, is not limited by the 3-slot worker pool, and runs as soon as it is sent. The frontend keeps the message box and restores the "continue directly" button. Sending is carried by SSE. There is no DB schema change. Tradeoff: the message lives only in memory and is not recovered after a crash, and sending a message may briefly put that task one worker over its concurrency (uncommon, and acceptable).

### Tasks

#### Fixed

- Fixed an out-of-memory crash while archiving a large task: the archive now streams the snapshot instead of reading the whole task into memory at once. Three recovery gaps on the cold-archive path are filled in. Traffic on a modern install lives only in SQLite. A crash between the PostgreSQL commit and the SQLite commit used to leave an archived task's traffic stuck in hot storage with no way to recover it. A staging journal is now written whether or not a history directory exists, and a missing `journal.json` is treated as disposable.
- Fixed the system stalling when there are many tasks: loading conversations and loading the UI were noticeably slow. Queries for the task list, task context, and exploration records are optimized together, and the frontend dashboard, chat page, and task detail page make fewer duplicate requests.

### Assets

#### Changed

- Removed the "at most 256 rules" cap on a company's asset scope: a company that enters scope as individual IPs or domains hits that cap easily, and the only way past it was to split into several companies, while more rules do not by themselves make scope slower. The cap is removed in the frontend, the backend, and the demo mock. A single rule is still limited to 1024 characters. The 2 MiB request-body cap stays as a backstop (on the order of forty or fifty thousand rules).

### Skills

#### Fixed

- Fixed skill archive uploads that failed with "upload failed: zip: unsupported compression": the Go standard library only includes Store and Deflate decompressors. bzip2 and Zstandard archives written by compression software at a non-default level could not be opened. Those two decompressors are now included (pure Go, no new external dependency). Methods that really cannot be opened, such as Deflate64 / LZMA / XZ / PPMd, and encrypted archives, now fail before decompression with a message that names which file used which compression and how to repack it, instead of throwing the underlying English error at the user.
- Fixed skill filename checks that treated a Chinese filename as illegal: path checks used to be an ASCII allowlist of `[A-Za-z0-9-_./]`. One file in the archive whose name used Chinese characters failed the whole upload with "the archive contains an illegal path". It is now a Unicode denylist: filenames in any language, and spaces, are allowed. Control characters, invalid UTF-8, zero-width and bidirectional control characters (RLO filename spoofing), `\ % # ? * : " < > |`, and `..` / absolute paths / empty path segments are still rejected. Directory traversal behavior is unchanged. Skill names are opened up the same way. ASCII is still limited to lowercase letters, digits, and hyphens (the agentskills.io spec). Non-ASCII letters such as Chinese can be a skill name directly, but spaces, dots, and path separators are not accepted.
- Fixed Chinese archives produced by Windows compression software whose filenames were garbled after unzip, or whose whole archive was rejected: those zips do not set the UTF-8 flag and write filenames in GBK. They are now decoded as GBK before the path check. A quoted frontmatter name whose value is Chinese also yields the skill name correctly.

### UI

#### Added

- The findings page has an "All findings" flat view, and it is the default. The original "group by task" view is the other header tab. The grouped view requires expanding each task before a vulnerability is visible, which is a detour when you only want to scan a list across tasks. The flat view is one cross-task table (10/20/50/100 per page) and behaves the same as the grouped view: select and export, expand a row for evidence and the detailed report, edit name/category/severity inline, change disposition, dig in, and delete. Both views share the stat cards and the filter bar. Switching does not drop filters. The current view and filters are stored locally. Polling hits only the current view. An inline edit writes both caches, so switching does not show stale data.
- The findings page has a "by asset" view: an asset tree on the left (company → root domain / IP → subdomain → service → endpoint; the company layer appears only when an asset actually has an owner) and, on the right, findings under the whole subtree of the selected node. It shares the same table and the same filters as the other two views. The tree includes only assets that have findings. Ancestor chains are filled in on demand (if a finding hangs only on the deepest endpoint, the layers above are still assembled). The count on a node is the subtree aggregate, deduped by finding. Findings whose asset was deleted, or that were never linked to an asset, go into an "unlinked assets" bucket. Unlike the other two views, the asset view does not poll. It queries when you enter the view, change a filter, add or edit or delete a finding on the page, or click refresh on the tree. The left tree is navigation and does not need to be recomputed every 5 seconds. Each layer shows only the increment relative to the layer above (a subdomain drops the root-domain suffix, a service shows `https :443`, an endpoint shows only the path). The full value is in the hover tip and the breadcrumb. When a layer has too many nodes, the endpoint/service level of that whole layer is dropped and a notice is shown. The counts still roll up.

#### Fixed

- Fixed session history in the mobile session detail being squeezed until it could not be read: on a narrow screen the session list is collapsed so the width goes to the history itself.

### Dependencies

#### Changed

- norma v0.3.2 → v0.3.3: `Config.MaxTokensField` lets the OpenAI Chat Completions output limit be sent as `max_completion_tokens` (reasoning models recognize only that key). The two keys are mutually exclusive and only one is sent. The default is still `max_tokens`.
- `golang.org/x/mod` v0.37.0 → v0.40.0, fixing two dependency security alerts.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)
- [@begininvoke](https://github.com/begininvoke)

## [0.3.6] - 2026-08-27

### LLM

#### Fixed

- Fixed the "non-streaming" switch reverting to "streaming" when the dialog was opened again after a save (#69): the configuration list DTO omitted the `streaming` field, so the response never returned it. The frontend read `undefined` and always fell back to streaming by default. The write and the database storage were fine; the value could not be read back. The DTO includes the field again (without `omitempty`, so `false` must appear in the response).
- The connection test now checks that the model actually replied, and tests the configuration's real send/receive mode (#65): it previously only checked whether HTTP succeeded. A request that went through with zero model output (thinking burned the budget / the body was swallowed by a safety policy / a compatibility layer dropped `content`) still reported "connected", which did not match "no reply" in a session. The test also always used streaming, so a non-streaming configuration was really testing a different channel. An empty reply is now a failure. On success the prompt shows the model's reply, and the current streaming switch is included in the test, so "the test passed" and "the session runs" stay the same.

### Agent

#### Changed

- `list_facts` is paginated and filtered by keyword, so a large number of facts does not blow up the context in one call (#74): the default is the latest 20. It supports `limit` (cap 100) / a `before` cursor / `q` summary keywords, and returns `{facts, total, has_more, next_before}`. A single summary that is too long is truncated by length (the full text is still `node_detail`). Worker and planner prompts match the pagination semantics. Old databases get a one-time migration that writes the new parameter schema into the tool catalog (`SeedTool` inserts only the first time; otherwise the tool management page shows "no parameters").

### Tools

#### Added

- The "Tool execution" page counts tool calls (#72): a "Stats" button in the toolbar opens a dialog with call count, share, and failure count per tool (sorted by count descending). It reuses the list's task / keyword filters, counts the whole result set rather than the current page, and loads only when the dialog opens.

### Dependencies

#### Changed

- norma v0.3.1 → v0.3.2: reasoning/refusal that OpenAI-family responses silently dropped is fixed (Responses adds `reasoning_text`), the three names of the reasoning field are deduped, an empty response (zero content blocks) is retried within a bound, and a compaction boundary that cut a tool pair and caused a gateway 400 is fixed (including orphan rows already baked into the transcript).

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.5] - 2026-08-25

### LLM

#### Added

- Each LLM configuration can switch streaming on or off (streaming by default): on uses streaming SSE; off uses true non-streaming (`stream:false`, one complete JSON response). That can avoid a gateway with a bad SSE implementation (empty frames, dropped thinking-field frames), at the cost of live progress and live token counts while a run is in progress. The worker, planner, main agent, chat, and goals all read the currently active configuration dynamically. The `llmpool`, `llmrec`, and task-runtime provider wrappers all accept non-streaming. `llm_profiles` has a new `streaming` column, and an `ALTER` backfills old databases (default `true`, so old configurations do not change).
- LLM configurations can use the OpenAI Responses API format: each configuration has a third format, `openai-responses` (`POST /v1/responses`), alongside Chat Completions and Anthropic. `BaseURL` is normalized, and the default model is `gpt-5`. The `format` constraint on `llm_profiles` includes `openai-responses`, and an idempotent migration backfills old databases. The frontend format dropdown adds "OpenAI (Responses API)". Depends on norma v0.3.1 (including the `reasoning_content` echo fix).
- LLM request/response HTTP bodies are recorded as sent: the real wire body is captured at the HTTP transport layer, keeping tool schemas, `tool_use` blocks, and raw SSE frames that the normalized view does not show. Each attempt of an internal norma retry is kept. `llm_records` has new `raw_request` / `raw_response` columns, and an `ALTER` backfills old databases. The recording page detail panel has a "raw" view toggle and copy buttons for the request and response (with an `execCommand` fallback for a non-secure context).

### Conversations

#### Changed

- Multi-select in the session list is a "multi-select" mode switch: the default list no longer keeps a checkbox on every row (it looks cleaner). The header is "N total + multi-select". Clicking "multi-select" enters selection mode (checkboxes, select all, bulk delete). "Done" exits and clears the selection. After a bulk delete that fully succeeds, it exits on its own (if any fail, it stays in selection mode so they can be retried). Rename, pin, and delete of a single row still use the ⋯ menu on the row.

### UI

#### Changed

- Task management, session actions, and traffic viewing are easier to use (#57): task-list column sort preferences are remembered, inline rename is triggered by an icon directly (no longer through a menu), and sheet interaction and traffic-view details are cleaned up.

#### Fixed

- Worker asset labels show only the domain and IP, and empty labels are handled correctly.

### Agent

#### Added

- When the planner judges a goal, it does a "quantitative acceptance check": if the goal has a measurable acceptance condition (asset test coverage reaches X%, N flags obtained, a given privilege obtained), it must check the measured values in `graph_overview` (`coverage.pct`, the findings count, and so on) before `prove_goal`. If the measurement misses the bar, `prove_goal` is forbidden and an intent is dispatched to close the gap. "The main part is done / roughly achieved" is not a reason to mark it met early. This fixes a goal that required 100% coverage being marked complete when the measurement was only 40%.

#### Changed

- The basis for "zero intents this round" in the planner prompt is rewritten: the old text described zero intents as "the most common and most important principle", which made the planner stop early while the goal was unmet and untested surface remained in scope. Zero intents should now be produced only in two concrete cases: (1) every direction that was thought of is already covered by an open, running, or recent_done intent; (2) the next step depends on output from work that is running now, and that output is not here yet (wait for it to finish, then plan on the next wake after the graph updates). The reverse constraint is added: if there is a new direction that is not covered and does not depend on running work, or the goal is unmet and untested surface remains, do not stop because "zero intents is common".
- The "evidence bar for a negative conclusion" in the worker prompt is stronger: a negative conclusion that might make the planner abandon a whole direction ("not injectable / port closed / no login") must exhaust the reasonable means inside that intent (other encodings, parameters, paths, methods) before the conclusion. If the means are not finished, or the evidence is weak, mark `confidence=inferred`. A casual `observed` negative must not weld the route shut (an early wrong negative in particular sends the direction off course and is hard to recover from).
- Those default planner / worker prompt changes are appended as a new version and switched to current by a one-time migration (`reseedPlannerPrompt` / `reseedWorkerPrompt`, each guarded by a settings flag). The old version stays in version history. A user who customized the prompt can restore it from the version record.

### Operations

#### Added

- `reset-password.sh` resets the administrator password (the username is fixed as `ARTEX`): local and docker deploys are supported. Connection info can be given explicitly or read automatically from `--dsn` / `$ARTEX_PG_DSN` / `config.json`. A bcrypt hash compatible with backend login is generated inside the database with `pgcrypto` and written back to `settings.auth.password_hash`. The service does not need a restart. The password is passed through an environment variable, never enters the process argv, and is escaped against injection.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

## [0.3.4] - 2026-08-24

### UI

#### Added

- After several tasks are selected in the task list, their category can be changed in bulk. The target category supports "uncategorized", which moves the tasks out of the current category. The whole batch is written in one transaction. The only failures are tasks that were deleted after they were selected.

#### Changed

- "Task category" on a new task changed from a dropdown to a searchable input: typing filters existing categories. A name that is not in the library is created and selected immediately on Enter (or by clicking "Create" in the dropdown). The selected item is shown as a removable tag. Still limited to one category.

### Conversations

#### Fixed

- Fixed sending a message being blocked by "LLM is not configured, cannot chat" when the session already had an LLM configuration selected but nothing was active globally. The pre-send check looked only at the globally active configuration, while execution prefers the configuration selected for the session. The two resolvers disagreed. They now use the same one (session / agent binding first, global as fallback).
- The message when chat is impossible is split by state: no configuration at all says "add a configuration"; configurations exist but none is active says "activate one, or pick one for this session". It no longer always says "not configured", which makes the cause easier to find.

### Agent

#### Added

- On a task whose goals are all met, an intent the main agent dispatches through `add_intent` can be claimed and executed directly by a worker, and the run stops when it finishes: the planner is not running in that case (a task with no open goal does not enter planning), so it does not re-judge the goal as met and cancel the intent that was just dispatched. After the frontier drains, the task returns to completed. Before the main agent dispatches an intent, it asks the user, based on the intent, whether to register it as a formal goal. If it is registered, the task returns to ordinary autonomous planning.

#### Changed

- When `step_ids` on `get_worker_trace` / `get_task_worker_trace` exceeds the per-call cap (5), it no longer errors immediately. It returns the full content of the first 5 steps, and the result's `returned_step_ids` / `omitted_step_ids` / `notice` say which were taken, which were not, and that there is no need to fetch more if what came back is enough. Duplicate and illegal ids in the input are deduped and dropped before the count.

#### Fixed

- The task list is sorted by task id descending (newest created first), replacing the old sort by creation time. Several tasks created at the same moment share a timestamp, so the order was unstable. Combined with a poll every 10 seconds and random iteration of an in-memory map, they kept swapping places in the list.

### Assets

#### Fixed

- Fixed company ownership recomputation being taken down by one dirty row: when `assets.ip` stored a hostname (written by an agent or the asset API), `a.ip::inet` threw `22P02`, so creating or editing a company asset scope and deleting a company all failed and rolled back. A safe cast, `try_inet()`, is used instead. An illegal value is skipped instead of aborting the whole statement.
- An unparseable `ip` is no longer skipped silently: after a company scope is saved, a clear notice says how many assets have an `ip` that is not a legal IP (with the specific id and value), and that those assets will not be owned by an IP/CIDR rule. The same information is written to the server log, covering paths that have no frontend response (deleting a company, ScopeSentry sync, agent writes).
- IP/CIDR matching of a task's test scope also uses `try_inet()`, replacing the old regex guard that only checked the character set (a hostname made entirely of hex letters, such as `abc.def`, used to slip through and raise the same error).

#### Changed

- The `ip` field of `ip`, `service`, and `endpoint` assets no longer accepts a hostname: `insert_assets` and the asset API return a per-row error with `index`, and say how to fix it (use `type=subdomain` and fill `domain`, or resolve the A/AAAA record first). The agent can correct it and insert again. Other assets in the same batch are unaffected and are stored as usual.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.3] - 2026-08-23

### Worker

#### Added

- A running worker can be paused, resumed, and cancelled individually. Pause keeps the intent, facts, and findings. Cancel transactionally cleans up the current intent and its direct products after execution exits.
- An intent state `paused`, an execution fence, and named termination reasons, so a late blackboard write cannot land after a pause, a cancel, or a task delete.
- An optional per-task concurrency cap. Create, resume, vulnerability dig-in, and queue backfill all go through one persisted FIFO admission path.

#### Changed

- The default wall-clock duration of one worker run changed from 600 seconds to 1200 seconds. After a timed-out task is revived, the clock starts again on the next real execution.
- The real execution state of the worker, planner, and main agent drives the task status badge together, fixing a badge that still said "idle" while a worker was running.
- A worker session keeps individual controls and the current session's token total. The model name moved to the right of the current session title.

#### Removed

- Removed worker multi-select, select-all, and the bulk pause/resume UI, and removed the worker bulk-control API and mock contract.
- Removed the token badge and tooltip on a worker list row. The underlying full token ledger and the task aggregate API remain.

### LLM

#### Added

- A task supports an ordered LLM configuration chain. When a provider quota shortage is clearly recognized, it switches to the next configuration automatically, and persists the current configuration, the exhausted state, and an error summary.
- A running or paused task can edit the full configuration chain, reorder it, and switch the current configuration by hand. The change applies from the next LLM call.
- Automatic switches, manual switches, and a fully exhausted chain are written as structured system activity, and a deduped notice pops on the task activity stream.
- A task-role model resolution API resolves, in one place, the configuration and model the main agent, planner, and worker will use on their next call.
- An optional global LLM pool, with call order, fallback when a named model fails, health, cooldown recovery, and a manual reset.
- An LLM usage ledger that is always on, aggregating input, output, and cache tokens by task, session, model, and configuration.

#### Changed

- Objective decomposition, the planner, the worker, and the main agent share the task-level LLM runtime (resolution order below).
- Failover responds only to a clear quota, balance, or billing error. Ordinary rate limits, auth, network, server, and context errors do not switch by mistake.
- The LLM configuration page is a card list plus a drawer on the right, with pool polling order, priority, exclusions, health, and recovery actions.
- Chain-exhausted text wraps on a narrow screen. The current model changed from an icon in the session list to a text label next to the current session title, with a full tooltip.
- Each role inside a task resolves its LLM in the order "agent binding → task configuration chain → global": a role with an explicit model binding always runs on that model. An unbound role falls through to the task chain, and an empty task chain falls through to global.
- An empty outbound proxy means a direct connection. It no longer falls back to the `HTTP_PROXY` / `HTTPS_PROXY` environment variables (an explicit `ARTEX_LLM_PROXY` is unaffected). The proxy input accepts `socks5://user:pass@host:port` with a username and password.

#### Fixed

- A transient streaming failure before any output has been produced is retried on the same provider with a safe backoff, which sharply reduces "it ran one or two tools and then stopped, with no summary, terminal state `model_error`". Quota exhaustion, an over-long context, and deterministic 4xx errors are not retried. They still go to failover and compaction recovery respectively.

#### Removed

- Removed the LLM icon marker from the session list, so it is not confused with the worker status icon.

### UI

#### Added

- Tasks support creating, renaming, deleting, and filtering global categories. A new task can pick a category directly.
- Global task-template CRUD and a management drawer on the right. A new task can load a preset description and goals, or save the current content as a template.
- A new task can link several source tasks and several company asset scopes. Company scope is recognized automatically from one multiline text box: domains, URLs, IPs, CIDRs, ICP, and company keywords.
- Task test assets can be added and deleted live, and record the source (human, company, inherited, or discovered by an agent). The current test assets and a source summary are shown next to the worker session title.
- The findings page is grouped by task, with its own pagination inside a group. A vulnerability can be given a description, and a high-priority dig-in intent can be created.
- Conversations can be renamed, pinned, unpinned, and deleted. The task list can bulk-pause and bulk-resume the current page.
- Traffic detail and tool-execution detail are drawers on the right. HTTP messages include `Host`, and the request line, status code, headers, JSON, and markup body are highlighted.
- The task list actions column has "Details" and "Pause/Resume". The session send key can be set in system settings (`Enter` / `Cmd+Enter`, and so on). The web search area has a proxy input.
- The badge to the right of the session title shows the LLM configuration name. The model id moved to the hover tooltip. A task can have an optional name. The task list has a name column and search (empty falls back to the description).
- The skills page shows use count, last used time, and missing-dependency counts, so a skill that is not taking effect or is not installed is easier to find.

#### Changed

- The task title is a focusable detail link. Task assets, company assets, finding task groups, and vulnerabilities inside a group use real server pagination, a stable sort, and an exact total.
- Creating a company uses a drawer on the right. Scope entry is one multiline text box, with live recognition, validation, and a type preview.
- The main agent input grows automatically, sends on `Enter`, and inserts a newline on `Shift+Enter`, and does not send by mistake during an IME composition.
- Agent preview, task reports, and related detail views share one Markdown renderer. Delete confirmation, long errors, and mobile drawer width are fixed in one pass.
- The task template picker displays and searches by template name and submits the template id. The system's original font size is restored. The app version is shown uniformly as `0.3.3`.
- Task totals and the current session's tokens highlight only input, cache read, and output. The send button uses a simpler up-arrow icon.
- Test scope on the task overview shows the company name rather than the id.

#### Fixed

- Ordinary text that contains a dot is no longer misread as an ICP filing number.
- Fixed a name collision in the variable catalog with a global runtime variable (such as `{{.Now}}`) that made the agent editor's variable list render a duplicate key.

#### Removed

- Removed the separate "enter" button on a task card. Clicking the task title opens the detail.
- Removed the dashboard "new task" button, the Logo URL field when adding a company, and the vulnerability-count badges at the top of the findings page and inside task groups.
- Reverted the global font increase of 10%, and removed the worker checkbox, model icon, and row-level token stats from the session list.

### Agent

#### Added

- A new task can be linked directly to several existing tasks, and in real time, read-only, inherits the direct source tasks' goals, facts, findings, finished intents, asset scope, and blackboard context.
- Blackboard read tools can query source-task nodes, facts, findings, and execution traces on demand. Inherited nodes carry a source mark, and every write tool refuses to modify them.
- Company scope tools accept domains, URLs, IPs, CIDRs, ICP, and company keywords. Keywords are only a scope hint for the agent. They do not take part in automatic asset ownership.
- Digging into a vulnerability creates, on the original task, a high-priority human worker intent with an asset anchor and a `derived_from` edge.
- The overview has a "Goal management" card for viewing, adding, editing, and deleting goals by hand. Adding or editing notifies the planner and revives the task. Deleting hard-deletes the goal node (edges and anchors cascade) and does not revive the task.
- The main agent gets `steer_work` by default. It can inject a course correction into a running worker in real time without interrupting it or losing progress (it still checks that the intent belongs to this task).

#### Changed

- Source-task intents do not enter the new task's frontier. The new task keeps its own exploration, execution queue, work directory, and conversation history.
- Deleting a running intent no longer destroys data: it stops at `stopped`, requires a delete reason, hangs that reason on the intent as a fact and writes it into the payload, and the planner perceives a `cancelled` trigger (the intent content and the reason are kept).
- The main agent conversation is driven by the server activity stream, and a failed send can restore the input. Opening a session sticks to the bottom, and after detail lazy-load the last reply stays visible.
- Pausing a task stops the current main agent, planner, and worker calls, but does not stop the user from starting a new main-agent orchestration session while the task is paused.
- Cancel, close, and a broken stream all keep the real termination reason, the content already generated, the round count, the duration, the tokens, and tool calls that had not returned.

#### Removed

- Removed the main agent's optimistic client message echo, so pause, failure, or concurrent activity does not produce duplicate messages or a stream crossing sessions.

### Constraints

#### Added

- Objective decomposition extracts operation constraints (allow/deny) before it splits goals, and the main agent can add more at runtime. Constraints are injected into the planner and worker system prompts at the highest priority. After the bias is removed, diversity and surface-expanding exploration explicitly obey the constraints.
- The overview has a "Constraint management" card and create/update/delete APIs. Constraint injection can be switched per planner / worker (both on by default, read every round, effective immediately). The new table `task_constraints` uses `CREATE TABLE IF NOT EXISTS`, so old databases create it automatically.

### Interception

#### Added

- Command interception adds a model fallback approval beside regex/string rules: when no rule hits, the model makes an `ALLOW` / `ASK` / `DENY` semantic decision. The action on failure and the action on approval timeout are configurable. The intercept page is split into "Intercept rules" and "Model configuration" tabs. A model decision is marked with a `[model]` prefix and a reason.

#### Fixed

- Hardened parsing of the judge output, so a correct `DENY` is not mistaken for an allow.

### Traffic

#### Changed

- Traffic recording stores the whole exchange in SQLite (a large body overflows into a blob bucket deduped by hash). Text bodies get a trigram full-text index, so any substring can be searched, including Chinese. Delete collapses to a single SQL transaction, from hours down to milliseconds, and recording no longer stalls during it. Large-body streaming download and a `traffic_search` full-text parameter were added. Old file-tree data needs no migration and can still be read, searched, and deleted.

### Tasks and assets

#### Added

- Creating a task has an "asset coverage" switch (on by default): when it is off, coverage is not computed or shown, the situation graph shows only assets, `task_scope` is not accumulated automatically, and the related tools are removed from the planner and the main agent. Company links are not affected by this switch.
- Each asset in `insert_assets` has a `related` flag (default `true`): it applies only when coverage is on. `false` enters the shared asset library only and does not count toward this task's coverage (for example a side site found along the way, or an unrelated asset).
- Company scope and task test assets both recognize domain, URL, IP, CIDR, ICP, and keyword text. A domain or IP can create or reuse a global asset. CIDR, ICP, and keywords stay as task-scope context.

### Build

- `build.sh --release` builds Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 in one pass, and writes a zip release that includes `skills/`, the config example, and the README.
- Debug info is stripped with the Go linker and the release is a zip. UPX is explicitly optional, so its self-extracting ELF does not segfault at startup on some Linux environments.
- The release workflow smoke-tests the Linux amd64 binary at startup and ships `SHA256SUMS` with the release.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.2] - 2026-08-20

### Added

- A new task can be linked to several existing tasks, and in real time, read-only, inherits the direct source tasks' facts, findings, finished intents, asset scope, and blackboard context. The new task still uses its own execution queue, work directory, and session history.
- A task supports an ordered LLM configuration chain. When a provider quota shortage is clearly recognized, it switches to the next configuration automatically, and persists the current configuration, the exhausted state, and structured audit activity.
- A running or paused task can edit the LLM configuration chain, reorder it, and switch the current configuration by hand. An automatic switch, a manual switch, and a fully exhausted chain are shown on the task detail page.
- A running worker can be paused, resumed, and cancelled. Pause keeps the blackboard data. Cancel transactionally cleans up that intent and the facts, findings, and execution records it produced directly, after the worker stops writing.
- Deleting a task can also clean up related assets, traffic, findings, and task files on disk, with a delete barrier, concurrency protection, and auditable delete statistics.
- An optional per-task concurrency cap. A new task that hits the cap queues FIFO and starts automatically when a running slot is free.
- Task asset pagination, company asset pagination, task category stats, and a paginated intent mock contract.
- `build.sh`, a single-binary build script, with static frontend export, asset embedding, cross-platform targets, and build version injection.

### Changed

- A task-level explicit LLM configuration chain coexists with the existing global LLM pool. An explicit chain keeps strict quota-failover semantics. With no explicit chain, the agent binding and global configuration rules are used as before.
- The main agent input supports auto-growing multiline input, sends on `Enter`, inserts a newline on `Shift+Enter`, and does not send by mistake during an IME composition.
- The planner, worker, and main agent share the task-level LLM runtime. The task chain uses the smallest context window among the candidate models as the safe compaction threshold.
- The task detail page shows running, idle, and paused from the real LLM call state. Facts, findings, intents, asset references, and graph nodes from a source task are marked with their source and stay read-only.
- Agent preview, task reports, and related detail views share one Markdown rendering component.
- LLM model configuration uses cards and a drawer. The model list scrolls on its own inside the drawer.
- Pausing a task no longer blocks the main agent conversation: the main agent orchestration session is independent of the task pause, and new messages can still be sent while paused (pause only stops the round that is in progress).
- The default wall-clock duration of one worker run changed from 600 seconds to 1200 seconds.

### Fixed

- Removed the optimistic echo on the main agent console and render from server data only, fixing messages that got scrambled or pulled in another session's content on pause or a failed send.
- Opening a main agent session scrolls to the bottom by default and shows the last reply in full: after the last reply's full content lazy-loads and expands, it sticks to the bottom and is not pushed off the screen.
- Fixed the main agent session keeping running after the task was paused, and the orchestrating agent not stopping the main agent when it paused the task.
- Fixed the task status badge and action buttons getting out of sync when the task completed or when the planner, worker, or main agent was actually running.
- Fixed a late blackboard write or leftover files that could appear between a worker cancel, a task delete, and concurrent writes.
- Fixed Markdown preview failing, long text overflowing the delete confirmation, mobile width, and some task-detail buttons that were not aligned.
- Every agent cancel path has a named termination reason. Activity detail can show who cancelled, the terminal state, the round count, the duration, token usage, and tool calls that had not returned.
- Fixed a race where, on backend shutdown, the parent context could overwrite a named `shutdown` reason first, and a Chinese activity summary truncated by bytes became garbled.
- Fixed a cancel event with partial streaming output losing the real termination reason, while keeping the content generated before the cancel.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

[Unreleased]: https://github.com/Autumn-27/ARTEX/compare/v0.3.10...HEAD
[0.3.10]: https://github.com/Autumn-27/ARTEX/compare/v0.3.9...v0.3.10
[0.3.9]: https://github.com/Autumn-27/ARTEX/compare/v0.3.8...v0.3.9
[0.3.8]: https://github.com/Autumn-27/ARTEX/compare/v0.3.7...v0.3.8
[0.3.7]: https://github.com/Autumn-27/ARTEX/compare/v0.3.6...v0.3.7
[0.3.6]: https://github.com/Autumn-27/ARTEX/compare/v0.3.5...v0.3.6
[0.3.5]: https://github.com/Autumn-27/ARTEX/compare/v0.3.4...v0.3.5
[0.3.4]: https://github.com/Autumn-27/ARTEX/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/Autumn-27/ARTEX/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/Autumn-27/ARTEX/compare/v0.3.1...v0.3.2

