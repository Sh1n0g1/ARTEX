# Changelog

Notable changes to this project are recorded in this file, following the format of [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/).

## [Unreleased]

## [0.3.15] - 2026-10-07

### Interception

#### New features

- **Added a built-in intercept rule for "delete-style endpoint paths"**: the previously built-in HTTP-destructive rule only recognized the DELETE **method**, but most applications' delete endpoints can be triggered with GET/POST, so a call like `curl 'http://t/api/user/delete?id=1'` would actually delete the target's data. Added a `deny` rule covering `/delete /del /remove /unlink /erase /destroy` (the verb must be followed by a separator, so `/delivery`, `/details`, `/delta` are not falsely blocked). Existing instances get it automatically after upgrading, and it can be disabled or removed under "System → Command interception".
- **Token usage from model-fallback approval can now be viewed separately**: approval calls previously had no separate metering attribution, so how much they cost could only be guessed. It is now metered on its own — a usage card has been added below the "Model-fallback approval" toggle in "System → Command interception", showing five cumulative counters (call count, input, output, cache read/write) plus a mini bar chart of daily usage over the last 30 days.

### Finding notifications

#### New features

- **Findings can now be pushed to IM**: added a "System → Notification push" page that can push scan findings to six channel types: **DingTalk, Feishu, WeCom, a generic webhook, Telegram, and email**. Any number of instances can be configured per channel type, each with its own enable/disable, rate limit and filter rules.
- **Push timing has a real-time mode and a digest mode**: real-time mode sends each match individually as it happens; digest mode merges matches over a global interval (default 30 minutes) into a single message, prefixed with the count of new findings and their severity breakdown. To get "real-time for critical, digest for the rest", set up two channels configured differently.
- **Filter rules support four dimensions**: minimum severity, restricting to a task / asset scope, finding-type keyword include/exclude (exclude takes priority), and whether to receive disposition-status changes (off by default). Messages include a "View details" button whose target address comes from the global "callback link" setting; leaving it empty omits the button.
- **Delivery history and manual resend**: lists each delivery's status, attempt count, failure reason and channel, filterable by channel and status; failed deliveries can be resent with one click (resending resets the retry count).
- **Masked echo of channel credentials**: webhook URLs, signing secrets, bot tokens, SMTP passwords and so on are echoed back only as a masked value with a trailing hint; submitting the mask unchanged means "do not modify", and clearing the field deletes it.

#### Bug fixes

> This section is the outcome of a round of security review after the initial feature was completed; each item was first reproduced, then fixed, then covered with a regression test.

- **Fixed a mask bypass via "change the target address, keep the credentials" (critical)**: config merging kept the original stored value for any "unmentioned key", so changing only the address while saying nothing about the credentials let the server send the real stored credentials to an arbitrary address, completely silently. Now, whenever the target address changes, every credential field must be stated explicitly — either given a new value, or explicitly cleared to mean it's no longer needed.
- **Fixed zero escaping of untrusted content on markdown-based channels**: finding titles and summaries come from model output, and asset URLs come from scan results, so a crafted title would render as a clickable external link in DingTalk / WeCom / Feishu, and image syntax would be fetched by the client at render time, leaking the reader's IP. Now content is flattened to a single line and markdown metacharacters are escaped, with the escaping happening at each channel's render boundary rather than in the shared title function.
- **Fixed the SSRF surface on delivery addresses**: previously loopback, internal-network and cloud-metadata addresses could all be delivered to, and the first 200 bytes of a failed response body went into `last_error` and were echoed in delivery history — a semi-blind read primitive. Protection now happens at the **dial stage**, covering DNS rebinding, and cross-host redirects are rejected; loopback and link-local addresses require explicitly setting `ARTEX_NOTIFY_ALLOW_LOCAL=1`, while RFC1918 private ranges are still allowed (self-hosted internal SMTP relays are common).
- **Fixed push credentials leaking through error messages**: DingTalk's `access_token`, WeCom's `key`, and Telegram's bot token are all in the URL, and on a failed delivery the full URL flowed into `last_error`, delivery history, server logs and the frontend prompt. These are now uniformly redacted, keeping only `scheme://host` and the underlying reason.
- **Fixed silent loss when a truncated digest message was marked as fully delivered**: findings cut off by truncation were neither in the message nor in the failure list, yet delivery history showed success. Now packing is done per **whole item**: only the findings that actually made it into the message are marked delivered; the rest go back to the queue for the next message without consuming a retry, and the message header states how many more will continue in the next one.
- **Fixed a typo in the minimum-severity threshold silently disabling the filter**: a typo like `hgih` for `min_severity` resolved to an unknown severity ordinal of 0, which degenerated the check into always-true — users who thought they'd only get critical findings actually got everything flooding the group. Now the value is validated on the write path, with the allowed values listed.
- **Fixed `rate_per_min = 0` (unlimited) being unreachable**: the database-write layer silently replaced an explicit 0 with the default, so operators who thought they had disabled rate limiting were still throttled. The default is now applied at the API layer only when the field is omitted.
- **Fixed digest channels completely bypassing the token bucket**: `rate_per_min` previously had no effect in digest mode. The number of items claimed per round is now bounded by both the round's quota and an in-memory cap.
- **Fixed failure handling deciding by the whole batch's maximum attempt count, so old deliveries dragged down new ones**: an old delivery that had already been retried twice would drag brand-new deliveries in the same batch into `failed` along with it, so a new finding could be permanently lost without ever getting a single retry. This is now decided per item.
- **Fixed deliveries whose snapshot couldn't be parsed in a digest batch being silently marked as succeeded**: such entries were skipped during rendering but still counted as succeeded along with the rest of the batch. They are now explicitly marked failed, with a reason given.
- **Fixed a single channel's per-round delivery count possibly exceeding the lease duration**: under multi-instance deployment, rows whose lease had expired could be re-claimed and sent again, double-incrementing the attempt count. The per-round cap is now derived from the lease duration and the per-call timeout, with an assertion pinning down the relationship between these constants.
- **Fixed Telegram truncation possibly cutting an HTML entity in half**: truncation only avoided splitting a tag in half, not an entity fragment like `&amp`, which could cause the parser to reject the **entire** message. Both are now avoided.
- **Fixed the email channel treating a transient SMTP failure as permanent**: greylisting-style 4xx codes like `450` are temporary rejections meant to be retried later, but were previously always treated as a hard failure, so every push to a server with greylisting enabled would fail on the first attempt. The response code's first digit is now used to distinguish: 4xx is retryable, 5xx is permanent.
- **Fixed submitting a nested structure actually writing the mask literal into the database**: object fields like `webhook.headers` could only be masked or submitted as a whole; putting the mask sentinel inside the object was stored as a real value, silently breaking authentication. Such submissions are now explicitly rejected.

#### Design notes

- **The transaction that writes a finding does only one blind INSERT**: `notification_events` is written in the same transaction as the finding, so committing guarantees the two stay atomically consistent. This INSERT deliberately does not read the channel table or run the user's filter rules, and is wrapped in a `SAVEPOINT` — otherwise a misconfigured filter condition could abort the transaction and prevent a critical finding from being stored at all.
- **Delivery uses lease-based claiming rather than a long transaction**: `FOR UPDATE SKIP LOCKED` claims a row, sets it to `sending`, and pushes `next_attempt_at` into the future as a lease, so no database lock is held during delivery; rows left behind by a crashed process are reclaimed once their lease expires, self-healing without retrying forever.
- **Rate limiting does not consume the retry budget**: the round's quota is computed from the token bucket first, and that many items are then claimed. Doing it the other way around would charge a delivery blocked by rate limiting an attempt for nothing, and a 3-attempt budget burned purely by waiting would end in failure.

### Test infrastructure

#### Bug fixes

- **Fixed permanent leftover assets caused by cleanup ordering in the company ICP attribution test**: `defer d.Close()` ran when the function returned, before `t.Cleanup`, so all cleanup statements landed on an already-closed connection, the error was discarded, and test assets and companies were left in the database forever — causing other tests that assert on asset counts to fail mysteriously. Closing the connection now also goes through `t.Cleanup`, registered earlier, and cleanup failures are now surfaced.

### Traffic

#### New features

- **Traffic list supports content search, path / status-code / length filters, and column sorting**: the filter row now supports filtering by **response content** (via full-text index), **path**, **status code** (an exact value or a bucket like `2xx`), and **response length range**; the header can sort by time, status code, or response length, with the sort preference remembered locally. Two new indexes were added and take effect automatically on existing instances after upgrading; the agent tool's query contract is unchanged.
- **Added "Clear all" to the traffic list**: deletes every traffic record in one shot (ignoring the current filters), cleans up historical host directories the index no longer references, and runs a full compaction afterward, reporting how much space was actually freed. Traffic evidence already attached to a finding lives in a separate evidence store and is unaffected.

#### Bug fixes

- **Fixed disk space not being freed after deleting traffic**: `auto_vacuum` was not enabled when the index database was created, so the file never shrank; the full-text index is also `contentless_delete`, so `DELETE` only writes a tombstone without reclaiming the original postings — deleting traffic actually made the index grow (measured: 6MB of captured bodies → a 16MB index, still 16MB after deleting everything). New index databases now enable `auto_vacuum=incremental`, and after every deletion a background job runs full-text index merging and incremental reclamation in chunks, gradually returning space to the system (the same scenario drops back to 104KB); reclamation releases the write lock between chunks, so it does not block traffic recording.

> Upgrade note: `auto_vacuum` can only be set when a database is created, so **existing instances' index databases are still in the old mode**, where incremental reclamation is a no-op (a notice is printed on startup). On these databases, tombstone growth has already stopped after upgrading, and the space already used can be reclaimed once with "Clear all" — that step converts the database to incremental-reclamation mode, after which ordinary deletions take effect on their own.

### LLM

#### Bug fixes

- **Fixed custom session headers not taking effect on "one-off LLM call" paths**: goal decomposition (round 0) and cold-node compaction are both one-off calls with no transcript store attached, so there was no session id in the context and the gateway couldn't read the header — on endpoints like opencode zen that return a hard 400 without it, this showed up as "round 0 fails, but later planner rounds work fine". Both paths now explicitly attach a session id stable per exploration, which also lets token usage be correctly attributed to the corresponding exploration (previously not tracked at all).

### Tools

#### Bug fixes

- **Fixed output truncation not taking effect for MCP and custom tools, letting large results flood the context whole**: MCP tool output previously wasn't truncated at all, and HTTP-style custom tools were passed an empty tool context and always used a hardcoded 6000 characters instead of the session's configured limit. Both paths now go through `actool.Capture` uniformly: truncated to the session's `MaxOutputChars` (default 30000), and when `ToolOutputDir` is configured, the full output is spilled to disk with only a leading excerpt and a file pointer left in the message.
- **Upgraded norma to v0.4.3**: the harness layer now runs `CaptureOnce` uniformly, so no tool (including any added in the future) can flood the context again by missing truncation.

### Deployment

#### Bug fixes

- **Fixed the activity stream failing to connect behind a reverse proxy**: the SSE address was previously built unconditionally as `${hostname}:8787`, so in a deployment reverse-proxied to 443, the page loaded from `https://your-domain/` while SSE was pointed at port 8787, which wasn't exposed publicly — this showed up as the activity stream endlessly reconnecting. It is now distinguished by `NODE_ENV`: production defaults to same-origin, and only `next dev` keeps `:8787` to work around Next.js's SSE buffering; the `NEXT_PUBLIC_SSE_BASE` override is still available. The README now documents reverse-proxy deployment.

### Findings

#### Bug fixes

- **Fixed the asset list in the "by asset" view having no scrollbar when it overflowed**: the outer card only had a `max-height` with no fixed height, so the scroll viewport's percentage height couldn't resolve, and with many assets the list would either burst out of the card or get clipped with no way to scroll. The height cap is now applied directly to the asset tree's native scroll container, adapting to the window height.

### Database

#### Bug fixes

- **Set a cap on the connection pool**: `database/sql` does not by default limit how many connections the application itself can open, so with no idle connection available it would unconditionally open a new one, all the way up until PostgreSQL's `max_connections` (default 100) started rejecting them. The pool is now capped at 32; queries over that limit now wait for a free connection, so under the same load the behavior is slower rather than erroring — if `max_connections` has been lowered, this cap should be adjusted accordingly.
- **Improved connection reuse**: `MaxIdleConns` defaults to 2, so connections beyond 2 concurrent ones were closed as soon as they were done, and the next request had to redo the TCP and PostgreSQL auth handshake. The idle cap is now raised to match the total cap, and a 30-minute connection lifetime plus a 5-minute idle eviction were added, so the pool can naturally cycle out long-lived bad connections.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.14] - 2026-09-24

### Task list

#### New features

- **Added a "Findings" column to the task list**: shows the count of Critical / High / Medium / Low findings by severity tier, with non-zero tiers colored by severity, so a task's finding volume and distribution are visible at a glance.

#### Changes

- **Narrowed the "Description / Goals" columns**: overly long content is now shown with an ellipsis, with the full text available on hover, so long text no longer crowds out the list's horizontal space.

### Exploration graph and planning-state overview

#### Changes

- **Trimmed the planning-state overview (`graph_overview`) and set count caps on each list**: recent facts, finished intents, pending intents, cold-region summaries and confirmed-finding details are now all "latest window + count fallback", with anything not shown queryable on demand. This significantly reduces the per-round LLM context size and prevents context bloat on long tasks (the cold region of linked-task overviews is rate-limited the same way).

#### Bug fixes

- **Removed a reverse duplicate edge between digest and finding nodes in the exploration graph after collapsing cold nodes.**

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.13] - 2026-09-19

### Asset interception

#### New features

- **Added global asset intercept rules (blacklist)**: supports entering exact-match and fuzzy-match domains / IPs / URLs as well as CIDR ranges, with create/read/update/delete and enable/disable for rules; the management page is under "System → Asset interception". Fuzzy blocking of government (`.gov` / `.gov.cn`) and education (`.edu` / `.edu.cn`) sites is enabled by default.
- **Asset interception wired into the execution chain**: before dispatching an intent (`add_intent`) or inserting an asset (`insert_assets`), the agent now checks the target asset against intercept rules — an intercepted intent is not dispatched, an intercepted asset is not inserted, and the asset information and the reason for interception are returned to the agent.
- **Added task-level intercept / allow (whitelist) rules**: independent of the global rules and effective only for the current task. The evaluation order is "intercept first, then allow": a rule match on intercept always forbids it; if it doesn't match any intercept rule but the task has allow rules configured and none of them match, testing is "not allowed"; the whitelist is not enabled at all if no allow rules are configured. These can be entered when creating a task, or added, edited, enabled/disabled in the task detail "Overview".
- **Task templates support preset categories and task-level intercept/allow rules**: a template can save a task category and a set of task-level rules, which are carried into the new-task form when the template is applied.

### Operation review

#### Bug fixes

- **Tightened the model-judge output protocol to reduce false allows caused by truncation**: the decision comment's cap was lowered from 500 Chinese characters to 120, and the output is now forced to be "JSON only, no preamble or code fences" — avoiding a decision becoming unparseable after being truncated by `MaxTokens` and then falling back to fail-open under the model failure policy.

### Task archiving

#### Bug fixes

- **A symlink encountered during archiving is now skipped rather than failing the whole package**: the archive format only supports regular files and directories end to end, so previously any symlink anywhere in the working directory would fail the entire task archive. It is now skipped (with a log entry) while the rest of the files archive normally (the link is not followed, and nothing outside the directory tree is included).

### Account and compliance

#### New features

- **Added a "Terms of use and disclaimer" dialog before login**: it must be checked and agreed to before logging in.

### License and dependencies

#### Changes

- **The project now uses the AGPL-3.0 open-source license**, and the README's license and disclaimer sections were expanded.
- **Upgraded norma to v0.4.1.**

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.12] - 2026-09-17

### Exploration-chain broadcast board

#### New features

- **Added an "Exploration-chain broadcast board" to the task detail page** (#144): presents the task's exploration nodes (start / goal / intent / fact / finding / hint / digest) as a timeline feed, with filtering by type, keyword search, ascending/descending order, pagination and auto-refresh. While on "page 1, newest first" it acts as a live position that refreshes every round; leaving that position only accumulates an unread count without disturbing the current reading position, with an "N new entries · back to latest" button to jump back. Entries are grouped by day, so even paging through a long task you can tell "which day this happened on".
- **The board now shows each row's node id** (#145), making it easier to cross-reference the exploration-chain graph and locate a specific node.
- **Expanding a node's detail on the board now shows its upstream/downstream relations and anchored assets** (#147): expanding an entry shows the node's upstream (derived from) / downstream (produced from this) relations, and hovering a related entry pops up that node's card (type / status / source / time / summary / payload excerpt); it also lists the assets anchored to that node (type tag + recognizable text). The related data ships together with the board page, so expanding does not trigger an extra request.
- **Board search supports filtering by node id** (#150): besides "content / source", the search box now also matches node ids — typing plain digits, or the `#41` form shown in the UI, precisely locates that node.

### Intent management

#### New features

- **Intent deletion now supports "soft delete / hard delete" modes** (#149): intents that are pending, running or paused can all be deleted, and a reason must be given; the deletion mode is chosen in the confirmation dialog.
  - **Soft delete (default)**: the intent is set to "deleted" and the reason recorded in a dedicated field, keeping the intent node and all of its output and lineage.
  - **Hard delete**: physically removes the intent and any descendant nodes "supported only by it" (cascading to the leaves along the output / intent chain), avoiding orphaned data. Token usage for the deleted intent is kept archived under its original date; shared nodes (still referenced by other intents), goals, and task-root facts are always kept. The hard-delete dialog shows the expected number of nodes affected by the cascade.

  Both modes notify the planner that "this intent was deleted by the user, with a reason" and trigger replanning accordingly.

### Operation review

#### New features

- **Approval records can now be filtered by status and decision source** (#139): approval records can be filtered by approval status and decision source to quickly locate the target record.

#### Bug fixes

- **Pending approval requests now load independently of history pagination** (#133): pending requests are no longer affected by the history list's pagination, and remain fully visible while paging through history.

### Agent

#### Changes

- **The worker role description was changed to generic cybersecurity-platform wording** (#138).

#### Bug fixes

- **Fixed cold-node compaction never triggering**: wired the Compactor into the per-task planner; cold-node compaction (cold-digest) previously never ran because it wasn't connected, and has now been restored.

### Chat

#### New features

- **Chat supports @ references to multiple record types with scroll pagination** (#135): multiple types of records can be @-referenced in chat, and the reference candidate list supports scroll-based pagination.

#### Bug fixes

- **Long message bubbles are now constrained within the session panel** (#137): overly long message bubbles no longer overflow the session area.
- **Fixed "missing uploaded file" when uploading before a new conversation was created**: the composer's file picker now snapshots the `FileList` into an array before clearing the input and then calls back; previously, since a draft-state conversation had to be created asynchronously first, by the time execution resumed the input-bound `FileList` had already been cleared, causing the upload to be missing its file field and the backend to return 400.

### Traffic

#### Bug fixes

- **The traffic-recording proxy now defaults to listening only on 127.0.0.1** (#129, #130): avoids an open proxy being exposed on other network interfaces under the default configuration.

### Web

#### Bug fixes

- **Fixed the static-export task list page losing the global header.**
- **Added the missing related-traffic mock for the demo finding detail page, fixing a blank screen.**

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)
- [@dingpotian](https://github.com/dingpotian)

## [0.3.11] - 2026-09-15

### Operation review

#### New features

- **Approval records support source navigation** (#125): clicking an approval record's “Source” jumps back to the specific tool execution that triggered that approval — opening the original full session, auto-paginating to the target position, expanding the command and result in place, and highlighting it centered in the scroll area, with surrounding messages kept readable; auto-correction stops once the user scrolls manually. Across chat, task workers, the planner, and the main agent's segmented sessions, this is located precisely via a persisted `tool_use_id` and task mapping (a new scoped call-id index was added); when it's missing, duplicated, or ambiguously linked, it shows an explicit notice instead of jumping to the wrong execution. A clear message is shown when the session has been deleted or the task has been archived / its record is missing.
- **Approval records support pagination** (#115).

#### Changes

- **Trimmed the model-review input** (#124, #125): the model review (the LLM judge used after no rule matches) now receives a versioned JSON input narrowed down to “the current complete tool call + an explicitly selected short context + the local working directory”. The context takes only real user messages (the current user message of a chat or task main agent); a worker no longer includes an intent summary, and context inherited from a parent agent is cleared; the planner and auto-triggered sessions do not fabricate a user message. The task description, goals, operational constraints, global exploration state, call history, or the worker's full intent are no longer included — these still feed agent execution and standalone session auditing as before, they just no longer enter operation review. Every decision must output JSON with `decision`/`comment`, and the explanation must state “the actual action, the consequence if it succeeds, and the rule matched”; instructions embedded in parameters or context cannot change the review policy. The exact input snapshot sent to the model, and its fingerprint, are saved; older snapshots are kept and labeled with their version, and old inputs are never reconstructed from current data.

#### Bug fixes

- **A model decision wrapped in a code fence is no longer silently allowed** (#126): if the decision JSON returned by the model was wrapped in a ``` code fence, the previous parse failure would fall back to the model failure policy and be silently allowed; code fences are now stripped before parsing, and only a still-failing parse falls back to the configured failure policy.

### Agent

#### New features

- **Added experimental noa context compaction** (norma upgraded to v0.4.0): an experimental feature that can be enabled in system settings, off by default. When enabled, noa (model-driven context compaction) takes over context compaction for all four agent types — main agent, planner, worker, chat — replacing the built-in compaction. The original pre-compaction text is persisted and archived centrally under `<workDir>/noa/<session-id>/` (rather than scattered across task directories), in a directory unique per session id globally. A failure to engage it falls back to built-in compaction automatically without interrupting real tasks; the toggle is read once per run, so switching it only affects runs started afterward.

#### Bug fixes

- **Exploration-graph tools now reject cleanly instead of crashing on nil when there is no task context**: calling exploration-graph-related tools outside a task context now returns a clear error instead of crashing from dereferencing an empty store.

### MCP

#### New features

- **Support for legacy SSE MCP servers** (#117): compatible with MCP servers that only offer the legacy SSE transport.

### Traffic

#### Bug fixes

- **Traffic search now matches a record's host port-aware** (#114): `traffic_search`'s host matching now accounts for the port, preventing records on the same host but different ports from cross-contaminating matches.
- **A migration error in the `traffic_search` description no longer blocks subsequent reporter migrations**: a single failed migration is now isolated and does not affect subsequent migrations from running.

### Web

#### Changes

- **Added a divider between task retest finding options** (#122): a separator was added between retest finding selection items for better visual clarity.

#### Bug fixes

- **Fixed the demo task detail page crashing entirely**: in mock mode, the task detail “Sessions” tab fetches `GET /api/tasks/<id>/side-questions` (the side-question history), but the mock handler had no such route; the catch-all fallback returned `[]` based on “a path ending in s is treated as a collection”, leaving `data.items` as `undefined`. The side-question hook's `merge()` then threw `TypeError: t is not iterable` while iterating it; this exception happened inside the `setItems` updater, which React defers and re-throws during render, so the caller's `catch` couldn't catch it, and the whole page was taken over by the error boundary showing “This page couldn't load”. The mock handler now explicitly returns an empty side-question history, and `sideAPI.history` also normalizes its return value (treating a non-array `items` as `[]`) as defense in depth.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.10] - 2026-09-13

### Web search

#### New features

- **Added DeepSeek's official source to web search**: reuses the currently active LLM configuration directly. It differs in nature from the other three sources — DeepSeek has no directly callable search API; search only exists inside its Anthropic-compatible endpoint (the `web_search_20250305` server-side tool), executed by DeepSeek's own servers. So it **only supports the DeepSeek official endpoint + the anthropic protocol** (an OpenAI-protocol endpoint rejects server-side tools outright), and every search **consumes one extra model call**, the request **does not go through the search egress proxy**, and it is **not recorded as traffic**. The returned results **have only a title and a link** (no summary; use WebFetch to fetch the body when needed). The settings page documents these limitations but **does not validate or block on them** — it's up to the user to confirm they're acceptable, with a “Test search” button to run a real check.

### Agent

#### New features

- **Workers gained the ability to look back across works**: added `search_all_worker_traces` (search by keyword across the execution processes of all works in the current task without first knowing the `intent_id`, returning matching steps) and `get_worker_trace` (after pinning down a specific work, list its step stream, search within it by keyword in place, or fetch the full content of a step by `step_id`), used to reuse observations another work saw but never wrote into a fact, avoiding duplicated effort.
- **`node_detail` was made available to workers**: paired with the look-back tools above, once a worker has an intent_id / node id it can directly query that node's full detail.
- **Planning rounds triggered by `add_hint` are now explicitly announced**: previously a newly added hint was only folded into the state overview for the planner to notice on its own; now a single `add_hint` records a trigger note for the planner — “a human added N strategic hints: …” (a batch counts as one, not flooding the feed per item) — explicitly telling the planner “this round was triggered by a new hint” and showing the hint content directly.

#### Changes

- **Reworded the global state hints to a “loosen up” tone**: encourages divergent exploration and prompt reporting of cross-intent leads, rather than converging too early.
- **Trimmed the worker boundary wording**: being blocked on the first attempt does not mean the direction has been fully explored; focus on “exhausting the bypass techniques available within this intent before concluding”.
- Removed the per-asset `related` parameter from `insert_assets`: its only effect was deciding whether to add that asset to the task's scope, and the value itself was never persisted (re-registering the same asset discards it, and the UI can't show which ones were judged irrelevant) — in practice it just made the model do an extra judgment call that left no trace.
- **Test scope (`task_scope`) is no longer affected by the asset-coverage toggle**: `insert_assets`'s automatic scope entry (`source='auto'`) now runs regardless of whether coverage is enabled, and `add_task_scope` is always available to the planner / the in-task main agent / the goal-decomposition agent. Scope is the task's authorization boundary and the filter basis for asset queries; the coverage toggle should only decide whether scope is used as the denominator for a metric, not whether scope itself gets accumulated. Previously, turning off coverage disabled both the `auto` and `agent` write paths at once, leaving `task_scope` with only rows added manually in the UI. `list_untested_assets` is still hidden when coverage is off (it is inherently a pure coverage view).

#### Bug fixes

- **Fixed assets leaking across tasks** (#59): `list_assets` previously hardcoded the task id to 0 and queried the entire shared asset store directly, and the model had no way to write a scope filter condition, so it would treat another task's assets (especially IPs) as this task's targets, skewing the testing direction. It now returns only assets that fall within the test scope (`task_scope`) of **the current task and its directly linked tasks**, matched by **ownership** rather than literal value: if the scope contains a root domain, all of its subdomains/services/endpoints can be found; if it contains a network range, hosts and services within that range can be found; fetching an out-of-scope asset directly by id likewise returns nothing. Non-task contexts (Auto/pentest) have no scope to rely on and still fall back to the whole store. This also fixed a blind spot where a directly-connected-by-IP host (e.g. `http://1.2.3.4/api`) couldn't be matched by a network-range scope. The UI's “Test assets” view (filtered by task producer) is unaffected.
- **Fixed the worker cross-work look-back tools being accidentally unbound**: after `search_all_worker_traces` / `get_worker_trace` (and `node_detail`) were added to the worker's default toolset, an old migration meant to “narrow the worker's tool surface” unbound them again on startup, so workers couldn't actually use these tools. They have been removed from that unbind list, and instances that already ran the old migration get them rebound to the worker in a one-time fix-up.
- **Fixed occasional storage failures for `/btw` side questions**: before writing a side_question checkpoint, the JSONB-unsupported NUL (`\u0000`) escape is now stripped, avoiding a write error for content containing that character.

### Triggers

#### Bug fixes

- **Merged trigger sessions now dedupe task descriptions/goals per task**: when multiple tasks are merged into one trigger, the same task's description/goals are no longer concatenated into the message repeatedly, preventing a long goal from piling up and blowing out the context.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.9] - 2026-09-11

### Agent

#### New features

- The in-task main agent now supports **multiple sessions**: you can create new ones, switch between them, and reset context independently, with every session interactive.
- Persistent `/btw` **side questions** are now supported: side questions that don't interrupt the main thread, with the Q&A stored in the database and surviving restarts.
- `spawn_task` gained `source_task_ids`, so a new task can **read-only inherit** assets and findings from source tasks.

#### Changes

- Disabled the worker's cross-engagement memory and narrowed its default toolset: reading context and reviewing across works are planning responsibilities, while the worker only handles the execution and write-back of a single intent.
- Trimmed the worker's default prompt; `get_worker_output` no longer has the `terminated` and `worker_name` fields; trimmed the descriptions of `insert_assets` / `list_assets`.

#### Bug fixes

- Fixed work being cut short prematurely by an **idle turn**: the model sometimes spends an entire turn only producing thinking, with no body text and no tool call, and in that case the harness sees a natural end (`end_turn` with no `tool_use`) and immediately closes it out as `completed` with an empty summary — leaving an unfinished intent cut off midway, showing up as "the task ended normally but produced no written summary at all". None of the five LLM retry layers could catch this: it's not an error, so the same-provider safety-window retry (which triggers on a "stream failure") and the circuit breaker (which treats `err == nil` as success) both let it through, and intent re-run only recognizes `model_error`; the SDK's empty-response retry can't catch it either, since that layer judges emptiness by "were any events yielded at all", and a thinking delta is itself an event. Now the work's Stop hook recognizes this kind of turn and injects a continuation instruction, letting the model carry on to the next step with the thinking it already produced — deliberately not choosing "resend as-is", because this kind of idling is usually caused by the shape of the prompt and context, i.e. stable behavior rather than random flakiness, and resending would just make the model think it over again.
- Reused the **empty-response retry count** from the LLM page's "Retry and backoff" settings for this count (both are answering "the model finished but produced no real content", just with different triggers and mechanisms), defaulting to 2, with `-1` turning it off and reverting to the original "idle turn just ends it" behavior. It is a **total** cap per intent, not a consecutive-count one: the harness itself already limits consecutive idling to pushing through only once, and the quota only refreshes after a real tool turn actually happens, so this number is there to stop a "tool → idle → push → tool → idle" loop from burning through the intent's budget. When triggered, the log records `[work <worker> · #<intent>] idle turn… injected continuation instruction (n/N)`. See `docs/LLM重试设计.md` §1.1 for the design.
- Fixed the context budget and input layout for `/btw` in long conversations; in a non-secure context (non-HTTPS access), the side-question request id now falls back to a degraded generator.

### Traffic

#### New features

- Findings now support **linking multiple pieces of traffic evidence**, which can be reordered, annotated, and tagged by role (request / response / supporting).
- The report agent now **automatically links** relevant traffic evidence before writing a report.
- Added a **traffic-binding toggle** for agents, and completed the hand-off of evidence between agents.

### Tasks

#### New features

- Added support for **session-level vulnerability retest**: a retest runs in a dedicated agent session, with its running status shown in both the list and the detail view.

### Interception

#### New features

- Added **detail and execution audit** to approval records: you can view the tool-request context, the model's/rule's initial verdict, the execution output, and the parameter fingerprint.

### Assets

#### New features

- Task test assets now support **DSL search**.

### LLM

#### Bug fixes

- Fixed the test-connection call not reattaching the custom session header, which caused opencode zen to return 400.

### Network

#### New features

- MCP's HTTP transport now supports **skipping TLS certificate verification**, making it easier to connect to services with self-signed certificates.

### UI

#### New features

- The session list is now grouped by agent, with expand/collapse and independent pinning per group; chat supports filtering by agent.
- The attack-chain graph now renders collapsed (digest) nodes and shrinks their member nodes.

#### Bug fixes

- Fixed a blank screen caused by out-of-sync login credentials.
- Changed the interception notice wording to clearly label it as "platform-enforced", avoiding it being mistaken for a defense on the target side.

### Deployment and updates

#### New features

- Added **one-click update from the UI**: a "Version & updates" card was added to the system settings page, and the top bar shows a notice when a new version is available. The flow is: download the release package → verify `SHA256SUMS` → smoke test → stage it → exit so the supervisor script relaunches it and completes the swap, and the page refreshes automatically.
- Added the supervisor startup scripts `start.sh` / `start.bat` as the official entry point (already bundled into the release package and the Docker image; `install.sh` is unchanged). They decide whether to relaunch based on the exit code, and are responsible for forwarding SIGTERM to `artex`. All verification and swap logic lives in Go; the scripts stay deliberately simple.
- Automatic fallback on failure: if verification or the smoke test fails, the staged file is discarded and the current version keeps running; if the new version fails to start 3 times in a row, it rolls back to the previous version. The settings page also has a manual rollback option (note that the database schema is not rolled back).
- Updates only trust GitHub domains and force HTTPS; the release source is not configurable, and dev builds have one-click update disabled. GitHub query results are cached for 30 minutes to avoid the top-bar notice exhausting the API quota.

#### Known limitations

- In Docker, only the program is replaced, not the image: toolchains do not get upgraded along with it, and recreating the container falls back to the version bundled in the image — use `docker compose pull artex` when you need to upgrade that too.
- The `skills/` directory in the release package is not synced, so new built-in skills added in a newer version do not automatically take effect.
- Updating is a restart, and interrupts running tasks.

### Dependencies

#### Changes

- Upgraded norma to v0.3.7.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.8] - 2026-09-09

### LLM

#### New features

- Added a "Retry and backoff" tab to the LLM page: both the **count** and the **interval** are configurable for all five retry layers. A single model call's failure is handled, from inner to outer, by: connection retry (SDK; connection reset / timeout / 429 / 5xx before the stream starts), empty-response retry (SDK; a normal end with no content at all, openai format only), same-provider safety-window retry (replay after a stream break before any output has been delivered to the caller), polling circuit breaker (cooldown-and-skip once consecutive failures hit a threshold), and intent re-run (the whole intent reruns after a worker ends with `model_error`) — the outer layer only gets a turn once the inner one is exhausted. Each layer has two knobs with the same semantics: leave it blank to use the original default count and exponential backoff; fill in a count to use it; fill in an interval to replace exponential backoff with a fixed interval; fill in -1 to turn that layer's retry off entirely. The first three layers follow the endpoint and can be overridden field by field in each model's configuration (pinning only the interval still inherits the global count); the circuit breaker and intent re-run are process-level semantics with only one global copy. Changes take effect live on save, no restart needed. Leaving everything blank is the current behavior, byte-for-byte unchanged after upgrading an existing database (new columns default to 0, and a missing settings key means all defaults). The connection test deliberately does not carry these parameters — it has a hard 30s timeout, and stacking the user's configured retries on top of that would just turn a working endpoint into a timeout failure in the test. See `docs/LLM重试设计.md` for the design.
- Each LLM configuration supports a custom **session header** (`session_header_key`): when non-empty, every LLM request carries this HTTP header, with the value set to the current run's session id (`conv-<id>` for a chat session, `exp<x>-worker-i<intent>` for a worker, and so on), useful for gateways that do prompt caching / sticky routing keyed on a session-id header. It's implemented by reading the session id from the request context and injecting it, with no changes needed to norma, so even the same shared provider can emit different header values per session. Existing databases get a migration, and an input field was added to the frontend's LLM configuration dialog.

#### Bug fixes

- Fixed saving an LLM configuration throwing a raw `23502` when the session-header field was empty: that column is `NOT NULL DEFAULT ''`, and it was previously wrapped in `NULLIF($n,'')` by mistake, turning an unfilled session header into NULL and tripping the not-null constraint; the empty-string value is now passed through directly as intended.

### Agent

#### New features

- Wall-clock timeouts now do an **in-place wind-down** instead of a hard kill: relying on norma v0.3.6, when `MaxDuration` is reached the running tool is interrupted and the run continues on the live context for a limited number of wind-down rounds (writing back whatever has already been identified plus a summary, with a final state of timeout), instead of relying on the worker's/planner's own `maxDur+90s` external hard context to kill a stuck run outright as `aborted_tools`. Chat goes through the same harness and automatically gets the same behavior; mainagent has no `MaxDuration` and is unaffected.
- Added a stall fallback: when the planner wakes on a heartbeat / no-change wake-up and the whole graph has no open or running intents left, its opening message is now replaced with a stall warning, explicitly stating that no worker is running and no direction is queued, and that this round must produce one or more mutually distinct new intents (producing zero intents is not allowed).

#### Changes

- Reworked the worker prompt: the intent / launch instructions / anchored-asset raw JSON were moved into the system prompt, re-assembled every round, never compacted away, and a resumed run no longer depends on the transcript's first message being preserved; the launch user message was slimmed down to just the global-state overview (which can degrade gracefully and tolerate staleness). The cost is that per-intent data is now mixed into the system prompt, losing cross-intent cache reuse — a deliberate trade-off in favor of "never lose an intent".
- Trimmed the planner / worker default prompts and fixed several real-world issues: the planner now distinguishes `recent_done` by status (for blocked/exhausted, it checks the trace before deciding, treating it as neither a dead end nor something to blindly rerun), a negative conclusion is now phrased as "an observation / still uncertain, not conclusive" and evidence is checked before trusting it, the planner must produce output when the goal isn't met and there are no open/running intents (a hard floor), and depth now takes priority over coverage. A worker's negative-leaning conclusion now only states "an observation + a tentative reading", with the actual verdict left to the planner; cross-intent leads are written into a fact's summary for the planner to pick up, rather than the worker chasing them itself. Reseeding appends the new defaults as a new version and switches to it; user customizations / old versions stay in history and can be rolled back to.
- Narrowed the work agent's default toolset and the asset write-back prompt: a worker is only responsible for executing and writing back a single intent, while reading context / reviewing across works is a planning responsibility — so `list_facts` / `node_detail` / `list_companies` and the cross-work lookup tools (`search_all_worker_traces` / `list_worker_traces` / `get_worker_trace`) were removed from its default tools, leaving only `list_findings` (dedup check before reporting a finding) + `add_finding` / `record_fact` + `insert_assets` / `list_assets`. The prompt also dropped stale field descriptions that conflicted with the `insert_assets` schema (`type=tech`/`on_url`/`props`). Existing databases get a one-time migration stripping the corresponding bindings; the planner's/main agent's bindings of the same tools are untouched.

### Exploration graph

#### New features

- **Exploration-graph cold-node compaction (cold-digest)**: collapses old, long-inactive intents / facts into digest nodes shown in `graph_overview`, while the original nodes are kept permanently and can be fully restored by id (storage is lossless; only the presentation is compacted, and collapsing is reversible). Hot/cold is judged by reverse reachability plus "hot if any branch is active", with R=6-round debouncing and grouping by connected component; background compaction (minor folds uncovered cold blocks, major re-derives from source and re-compacts to merge fragments) includes an activity recheck and a cooldown mutex, and never runs on the hot path and never overwrites a node that has come back to life. The overview exposes `cold_digests` and an asset-indexed `cold_index`, with `expand_digest` / `expand_index` handling restoration. Linked / inherited tasks' overviews likewise reuse their own collapsed view, and `expand_digest` supports cross-task read-only restoration. `expand_digest` / `expand_index` are given only to the planner and the main agent, not to workers. Both come with a migration for existing databases.
- `graph_overview` now outputs `finding_list` in full: findings are a task's highest-value output and there usually aren't many per task, so they are now included in full in the overview (unlike facts, which only get the most recent window) — the planner/worker can see every confirmed finding at a glance each round without calling `list_findings` again. Each entry is trimmed to `{id, summary, evidence?, from_intent?, assets?}`, where affected assets are given as readable content (url / domain / ip:port) rather than raw ids.

#### Changes

- `graph_overview` no longer flattens out `hosts`: the coverage block no longer includes a host list (a large-scope task could carry up to 500 host strings per round, of limited value to planning decisions), keeping only `host_count`; specific hosts can be looked up with `list_assets` as needed. Added a top-level `done_intents_total` (total finished intents) alongside `recent_done_intents`, which is capped at ≤15, so the planner knows whether it's looking at a truncated list when deduping.

### Tools

#### Changes

- Changed `add_company_scope`'s default binding from worker to planner: defining a company's asset scope is a planning / orchestration / Auto responsibility, while the worker only executes exploration. New databases seed the default binding as mainagent/planner/auto; existing databases get a one-time migration.
- Bound `list_assets` to the planner by default: it now has both `list_assets` (full-store DSL search) and `list_untested_assets` (untested within scope); existing databases get a one-time backfill that does not override a user's manual unbind.

### Tasks

#### New features

- Added a "Running workers" column to the task list: counts the intent nodes under that task with `state='running'`, matching exactly the "Running workers" figure on the task detail page's overview, showing 0 when there are none running.

### Network

#### New features

- Added a **global egress proxy** configuration: all target traffic can be routed out through a unified global proxy (http/https/socks5, with `user:pass` support). When traffic capture is enabled, it acts as the upstream for the MITM recording proxy (traffic is still recorded and then sent out through the proxy; both the intercept and pass-through paths go through the upstream without leaking the source IP). When capture is disabled, it is injected directly into the agent's bash environment and WebFetch (`proxyEnv` gained `ALL_PROXY` support for socks5). The configuration is stored in the settings KV table (no migration needed), and a "Global proxy" card was added to the frontend's system settings page, independent of the web-search proxy and the LLM proxy.

### UI

#### Bug fixes

- Fixed incorrect wording on intent status labels: `exhausted` "Exhausted" → "Budget exhausted" (it actually means the step/time budget was cut off midway with only partial results written back, not that the direction has been fully explored), `blocked` "Blocked" → "Execution error" (it actually means the model/API/network failed and retries were exhausted, with the intent barely explored at all, not that the target/a WAF blocked it), and added the previously missing `stopped` "Stopped" (a work manually stopped by the user).

### Dependencies

#### Changes

- Upgraded norma to v0.3.4: added truncation and spill-to-disk for MCP tool output (see `651b961`), which later enabled v0.3.6's in-place wall-clock wind-down.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.7] - 2026-08-31

### LLM

#### New features

- Added "Output cap" to model configuration, along with a choice of which request field name to use for it: the cap limits how many tokens a single reply can generate at most, sent with every request, with 0 (default) meaning the field is not sent at all and the server's default decides. This is a different thing from "context window" — the latter is the model's total capacity, used only locally to compute the compaction threshold, and never appears in the request. The field-name toggle only matters for the `openai` (Chat Completions) format: empty (default) sends `max_tokens`, which the vast majority of compatible gateways expect; OpenAI's own reasoning models (the o-series / GPT-5) instead only accept `max_completion_tokens` and return `unsupported_parameter` outright if sent `max_tokens`, so these endpoints need to be switched to the new field manually. Both of the other formats have a fixed field name (`max_tokens` for Anthropic, `max_output_tokens` for the Responses API), so the toggle is disabled and cleared on save for non-openai formats. This also closes a path that was never actually wired up before: none of the five places — planner / worker / chat / main agent / goal decomposition — ever set an output cap, so openai-format endpoints simply never sent it and Anthropic fell back to the SDK's 8192 default; adding the config field without wiring it up meant entering a number had no effect. The value is now resolved every round from configuration just like "streaming" is, so it takes effect starting the very next round after a failover switch. Behavior after upgrading an existing database is completely unchanged (the new column defaults to 0 with an empty field name).
- When clicking "Test" in the backend, the status code and the gateway's raw response body for each HTTP attempt are now logged server-side (the body truncated to 4K), making it easier to diagnose a 401, a quota message, an empty frame, an HTML page being returned, and so on — no longer limited to the collapsed ok / err shown in the UI. This log does not depend on the "LLM recording" toggle.

#### Changes

- A task's LLM configuration chain can now be modified in any state, no longer restricted to "running / paused / chain exhausted": after a task ends (done / failed / timeout), the main agent conversation still uses this chain, and previously, if a model on the chain had a problem, it could neither be changed nor continued. Both the backend HTTP and the DB-transaction final-state blocks were removed, and the frontend dialog now allows editing and saving even for a finished task. Saving on a finished task no longer reopens quota-blocked intents (those intents are moved to open, where they have no worker executing them and no longer meet the conditions for "rerun intent"); to keep going, use rerun intent / add a new goal, which pull the task back into a running state.

### Agent

#### Changes

- "Send a message to a running worker to steer its direction" now reuses the existing pause/resume + transcript-resume mechanism, behaving the same as a main agent conversation: it previously went through a bespoke, self-built intervention persistence protocol that touched the scheduling barrier, the resume flow, and over a dozen activity-query filters. The message is now injected via the next round's input, and the intent runs directly in its own dedicated goroutine, unconstrained by the 3-slot worker pool and running as soon as it's sent; the frontend keeps the message box and restores the "continue directly" button, with sending now carried over SSE. No DB schema change. Trade-off: the message is only kept in memory with no crash recovery, and the task may momentarily run one more worker than its concurrency limit when a message is sent (a low-frequency scenario, acceptable).

### Tasks

#### Bug fixes

- Fixed an out-of-memory (OOM) crash when archiving large tasks: archiving now streams the snapshot to disk instead of reading the whole task into memory at once; also closed three recovery gaps on the cold-archive path — traffic on modern installs is only stored in SQLite, and a crash previously occurring between the PostgreSQL commit and the SQLite commit could leave an archived task's traffic stranded in hot storage with no way to recover it. A staging log is now written regardless of whether a history directory exists, and a missing `journal.json` is now treated as safely discardable.
- Fixed the system becoming sluggish when there are many tasks: loading conversations and the UI had noticeable lag. The queries for the task list, task context and exploration records were all optimized, and the dashboard, conversation page and task detail page in the frontend had their duplicate requests reduced accordingly.

### Assets

#### Changes

- Removed the "max 256 rules" limit on a company's asset scope: companies that enter their scope one IP / domain at a time hit this cap easily, and once hit, the only option was to split into multiple companies, even though scope size itself doesn't make lookups slower. The cap was removed in all three places — frontend, backend, and the demo mock — while a single rule is still capped at 1024 characters, and the 2 MiB request-body limit remains as a backstop (roughly forty to fifty thousand rules).

### Skills

#### Bug fixes

- Fixed uploading a skill archive failing with "Upload failed: zip: unsupported compression": the Go standard library only has Store / Deflate decompressors built in, so bzip2 or Zstandard archives produced by compression tools at non-default settings could never be opened. Both decompressors have now been added (pure-Go implementations, no new external dependency); methods that genuinely can't be decompressed — Deflate64 / LZMA / XZ / PPMd and so on — as well as encrypted archives, now produce a clear message before extraction even starts, naming exactly which file uses which compression method and how to repackage it, instead of dumping the underlying English error on the user.
- Fixed filename validation inside a skill rejecting Chinese filenames as illegal: path validation used to be an ASCII whitelist of `[A-Za-z0-9-_./]`, so a single file with a Chinese name in the archive (like `参考/说明.md`) would fail the whole upload with "archive contains an illegal path". It is now a Unicode blacklist instead: filenames and spaces in any language are allowed, while control characters, invalid UTF-8, zero-width and bidirectional control characters (RLO filename spoofing), `\ % # ? * : " < > |`, and `..` / absolute paths / empty path segments are still rejected, with directory-traversal protection unchanged. The skill name itself was loosened the same way — ASCII is still restricted to lowercase letters, digits and hyphens (per the agentskills.io spec), but Chinese and other non-ASCII scripts can now be used directly as a skill name, though spaces, dots and path separators are still not accepted.
- Fixed Chinese-named archives produced by Windows compression tools extracting with garbled filenames or being rejected outright: such zips don't set the UTF-8 flag and write filenames in GBK; these are now decoded with a GBK fallback before path validation. Quoted frontmatter like `name: "中文技能"` is also now correctly read for the skill name.

### UI

#### New features

- Added an "All findings" flat view to the findings page and made it the default, switchable via a tab in the page header alongside the original "Grouped by task" view: the grouped view requires expanding each task individually to see its findings, which gets in the way when you just want to scan the list across tasks. The flat view is one big cross-task table (10/20/50/100 rows per page), behaving exactly like the grouped view — checkbox export, inline expansion to view evidence and the detailed report, inline editing of name/category/severity, changing disposition status, drilling in, and deleting. Both views share the same stats cards and filter bar; switching views keeps the filters, and both the current view and the filter selections are remembered locally. Polling only hits the current view, and an inline edit writes through to both views' caches, so switching views never shows stale data.
- Added a "By asset" view to the findings page: an asset tree on the left (Company → root domain / IP → subdomain → service → endpoint, with the company level only shown when an asset actually has one), and on the right, the findings under the selected node's entire subtree — sharing the same table and the same filters as the other two views. The tree only includes assets that have findings, with ancestor chains filled in as needed (even when a finding is only attached to the deepest endpoint, the levels above it can still be reconstructed); the count on each node is the subtree's aggregate, deduplicated by finding. A finding whose asset has been deleted, or that never had an associated asset, goes into an "Unassociated assets" bucket. Unlike the other two views, the asset view does not poll — it queries only when entering the view, changing a filter, adding/editing/deleting a finding in-page, or clicking the tree's refresh button, since the left tree is a navigation structure with no need to be recomputed every 5 seconds. Each level of the tree shows only the increment relative to the level above (a subdomain drops the root domain's suffix, a service shows as `https :443`, an endpoint shows only the path), with the full value available in the hover tooltip and the breadcrumb; when there are too many nodes, an entire endpoint/service level is dropped with a notice, while the count still rolls up to the level above.

#### Bug fixes

- Fixed session records being squeezed and unreadable in the mobile session-detail view: the session list now collapses on narrow screens, giving the width back to the records themselves.

### Dependencies

#### Changes

- Upgraded norma v0.3.2 → v0.3.3: added `Config.MaxTokensField`, letting OpenAI Chat Completions' output cap be sent as `max_completion_tokens` instead (reasoning models only accept this key). The two keys are mutually exclusive, with only one ever sent, and the default remains `max_tokens`.
- Upgraded `golang.org/x/mod` v0.37.0 → v0.40.0, fixing two dependency security advisories.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)
- [@begininvoke](https://github.com/begininvoke)

## [0.3.6] - 2026-08-27

### LLM

#### Bug fixes

- Fixed the "non-streaming" toggle reverting to "streaming" after saving and reopening (#69): the config-list API's DTO was missing the `streaming` field, so the response never returned it and the frontend fell back to the default of streaming whenever it read `undefined`; the actual write and DB storage were never the problem, it just couldn't be read back. The field was added back to the DTO (without `omitempty`, so `false` must also appear in the response).
- Changed the test-connection check to verify the model actually replied, and to test using that configuration's real send/receive mode (#65): it previously only checked whether the HTTP call succeeded, so a request that went through but got zero reply from the model (thinking burning through the budget / the body being swallowed by a safety policy / a compatibility layer dropping `content`) was still reported as "connection succeeded" — inconsistent with the session actually "not responding". The test also always ran in streaming mode, so a non-streaming configuration was actually testing a different channel. An empty reply is now treated as a failure outright, the model's reply is shown in the prompt on success, and the current streaming toggle is now carried into the test, keeping "test passed" consistent with "the session actually works".

### Agent

#### Changes

- Changed `list_facts` to pagination + keyword filtering, avoiding a single call blowing out the context when there are many facts (#74): it now returns the latest 20 by default, supports `limit` (capped at 100) / a `before` cursor / a `q` summary keyword, and returns `{facts, total, has_more, next_before}`; an overly long summary is truncated by character count (the full text is still available via `node_detail`). The worker / planner prompts were updated to match the pagination semantics. Existing databases get a one-time migration that refreshes the new parameter schema into the tool catalog table (`SeedTool` only runs on first insert, otherwise the tool management page would show "no parameters").

### Tools

#### New features

- Added call-count statistics to the "Tool execution" page (#72): a "Statistics" button in the toolbar opens a dialog showing, per tool, the call count, its share, and the failure count (sorted by count descending); it reuses the list's task / keyword filters and aggregates over the whole result set rather than just the current page, fetched only when the dialog is opened.

### Dependencies

#### Changes

- Upgraded norma v0.3.1 → v0.3.2: fixed reasoning/refusal being silently dropped in OpenAI-family responses (added `reasoning_text` for the Responses API), deduplicated the three different names for the reasoning field, added bounded retry for an empty response (zero content blocks), and fixed a gateway 400 caused by a compaction boundary cutting a tool pairing in half (also fixing existing orphans already baked into the transcript).

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.5] - 2026-08-25

### LLM

#### New features

- Each LLM configuration supports a streaming/non-streaming toggle (streaming by default): on uses streaming SSE; off uses genuinely non-streaming (`stream:false`, returning the complete JSON in one shot), which can work around some gateways' poor SSE implementations (empty frames, dropped frames in the thinking field), at the cost of losing real-time progress and real-time token counting while a run is in progress. The worker / planner / mainagent / chat / goals all resolve this dynamically from the currently active configuration; all three layers of provider wrapping — `llmpool` / `llmrec` / the task runtime — are compatible with non-streaming; `llm_profiles` gained a `streaming` column, with an `ALTER` to backfill existing databases (defaulting to `true`, so existing configurations are unaffected).
- Added support for the OpenAI Responses API format in LLM configuration: each configuration gained a third format, `openai-responses` (calling `POST /v1/responses`), alongside Chat Completions / Anthropic; `BaseURL` is normalized, with a default model of `gpt-5`; the `format` constraint on `llm_profiles` now includes `openai-responses`, with an idempotent migration to backfill existing databases, and the frontend's format dropdown gained "OpenAI (Responses API)". This depends on the norma v0.3.1 upgrade (which includes a fix for `reasoning_content` pass-through).
- Added recording of the raw HTTP request/response of LLM calls: the real wire body is captured at the HTTP transport layer, preserving the tool schema, `tool_use` blocks and raw SSE frames that the normalized view can't show; each of norma's internal retry attempts is kept individually; `llm_records` gained `raw_request` / `raw_response` columns with an `ALTER` to backfill existing databases; the recording page's detail panel gained a "Raw" view toggle and request/response copy buttons (with an `execCommand` fallback for non-secure contexts).

### Chat

#### Changes

- Changed multi-select in the session list to a "Multi-select" mode toggle: the list no longer keeps a checkbox on every row by default (a cleaner look), with the header now showing a "N total + Multi-select" button; clicking "Multi-select" enters selection mode (checkboxes, select-all, bulk delete), "Done" exits and clears the selection, and a bulk delete that fully succeeds exits automatically (a partial failure stays in selection mode to make retrying easier); renaming / pinning / deleting a single session still goes through each row's ⋯ menu.

### UI

#### Changes

- Improved the task management, session operation and traffic viewing experience (#57): the task list's column sort preference is now remembered persistently, inline renaming now triggers directly via an icon (no longer through a menu), and the drawer (sheet) interaction and traffic-viewing details were refined.

#### Bug fixes

- The worker asset tag now shows only domains and IPs, and correctly handles the case of an empty tag.

### Agent

#### New features

- Added a "quantitative acceptance check" to the planner's goal judgment: when a goal has a quantifiable acceptance condition (asset test coverage reaching X%, obtaining N flags, gaining a certain privilege), it must check the actual measured values in `graph_overview` (`coverage.pct`, finding counts, etc.) before `prove_goal`; if the measured values don't meet the condition, `prove_goal` is forbidden and an intent must be dispatched to close the gap instead — it must not be marked met early just because "the main part is done / mostly achieved". This fixes a goal requiring 100% coverage being judged complete when only 40% had actually been measured.

#### Changes

- Rewrote the basis for the planner prompt's "0 intents this round" decision: the original text described 0 intents as "the most common, most important principle", which caused the planner to stop too early when the goal wasn't met and there were still untested areas within scope. It is now changed to produce 0 intents only in two specific situations: ① every direction it can think of is already covered by an open/running/recent_done intent; ② the next step depends on output from a work currently running, and that output hasn't appeared yet (it should wait for that work to finish and the graph to update, then plan again at the next wake-up). An inverse constraint was also added: when there genuinely is an uncovered new direction that doesn't depend on a running work, or the goal isn't met and there are still untested areas, don't stop just because "0 intents is common".
- Strengthened the "evidence bar for negative conclusions" in the worker prompt: for negative conclusions like "not injectable / port closed / no login entry point" that could make the planner abandon an entire direction, it must now exhaust the reasonable techniques within that intent (alternate encodings/parameters/paths/methods) before concluding; if the techniques weren't fully tried or the evidence is weak, it must be marked `confidence=inferred`, avoiding a careless `observed` negative conclusion welding an entire line of attack shut (an incorrect negative conclusion early in a task is especially likely to misdirect exploration and hard to self-correct).
- The planner / worker default-prompt changes above were applied via a one-time migration (`reseedPlannerPrompt` / `reseedWorkerPrompt`, each guarded by a settings flag), appended as a new version and switched to as the current one; old versions remain in version history, and users who had customized their prompt can restore it from version history.

### Operations

#### New features

- Added `reset-password.sh` to reset the administrator's (username fixed as `ARTEX`) password: supports both local and docker deployments, with connection info either given explicitly or read automatically from `--dsn`/`$ARTEX_PG_DSN`/`config.json`; it uses `pgcrypto` inside the database to generate a bcrypt hash compatible with backend login and writes it back to `settings.auth.password_hash`, with no service restart needed after resetting. The password is passed in via an environment variable, never appearing in the process argv, and is escaped against injection.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

## [0.3.4] - 2026-08-24

### UI

#### New features

- Selecting multiple tasks in the task list now allows bulk category changes, with "Uncategorized" available as a target category to move tasks out of their current one; the whole batch is written in a single transaction, and the only items that can fail are tasks that were deleted after being selected.

#### Changes

- Changed the "task category" field on task creation from a dropdown to a searchable input: typing filters the existing categories, and pressing Enter (or clicking "Create" in the dropdown) on a name not in the database creates and selects it immediately, with the selection shown as a removable tag; it is still limited to a single category.

### Chat

#### Bug fixes

- Fixed sending a message being blocked with "LLM not configured, cannot chat" when a session had an LLM configuration selected but there was no globally active configuration. The pre-send check previously only looked at the global active configuration, while actual execution preferred the session's selected configuration — the two resolution paths were inconsistent; they are now unified into one (session/agent binding first, global as fallback).
- The "cannot chat" message is now broken down by state: no configuration at all shows "Add a configuration", having a configuration but none active shows "Activate one, or assign one to this session", instead of always showing "Not configured" — making it easier to pinpoint the issue.

### Agent

#### New features

- For a task whose goals have all been met, an intent dispatched by the main agent via `add_intent` can now be claimed and executed directly by a worker, stopping once it's done: the planner does not run in this case (a task with no open goal doesn't enter planning), avoiding it re-judging the goal as met and canceling the intent that was just dispatched; once the frontier drains, the task returns to completed. Before dispatching an intent, the main agent asks the user whether to register it as a formal goal based on its content; if registered, the task resumes regular autonomous planning.

#### Changes

- When `step_ids` for `get_worker_trace` / `get_task_worker_trace` exceeds the per-call cap (5), it no longer errors out directly — it now returns the full content of the first 5 steps, and tells the caller via `returned_step_ids` / `omitted_step_ids` / `notice` which ones were fetched this time and which were not, with a note that "no further fetch is needed if this is already enough". Duplicate and invalid ids passed in are deduplicated and stripped before counting.

#### Bug fixes

- Changed the task list to sort by descending task id (most recently created first), replacing the previous sort by creation time. Multiple tasks created at the same instant had identical timestamps, making the sort unstable, and combined with 10-second polling and the random iteration order of the in-memory map, this caused them to frequently swap positions in the list.

### Assets

#### Bug fixes

- Fixed company-attribution recalculation being taken down by a single row of dirty data: when `assets.ip` held a hostname (written by an agent or the asset API), `a.ip::inet` would throw `22P02`, causing adding/modifying a company's asset scope and deleting a company to all fail and roll back. Switched to the safe conversion `try_inet()`, which skips invalid values instead of aborting the whole statement.
- An unparseable `ip` is no longer silently skipped: after saving a company's scope, it now explicitly reports how many assets have an `ip` that isn't a valid IP (including the specific id and value), warning that these assets won't be attributed by IP/CIDR rules; the same information is also written to the server log, covering paths with no frontend response (deleting a company, ScopeSentry sync, agent writes).
- Switched the task test-scope's IP/CIDR matching to `try_inet()` as well, replacing the previous regex guard that only checked the character set (an all-hex-letter hostname like `abc.def` would previously still slip through and trigger the same error).

#### Changes

- The `ip` field on `ip`, `service` and `endpoint` assets no longer accepts a hostname: `insert_assets` and the asset API now return a per-item error with an `index`, along with a direct fix (switch to `type=subdomain` and fill in `domain`, or resolve the A/AAAA record first), which the agent can use to self-correct and reinsert. Other assets in the same batch are unaffected and are inserted as usual.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.3] - 2026-08-23

### Worker

#### New features

- A running worker now supports per-item pause, resume and cancel; pausing preserves the intent, facts and findings, while canceling transactionally cleans up the current intent and its direct output once execution exits.
- Added a `paused` intent status, an execution fence, and named termination reasons, preventing a late blackboard write after a pause, a cancel, or task deletion.
- Added an optional per-task concurrency cap; creating a new intent, resuming one, drilling into a finding, and backfilling the queue all now go through a unified, persisted FIFO admission path.

#### Changes

- The default wall-clock duration for a single worker run was changed from 600 seconds to 1200 seconds; a timed-out task that is revived has its clock restarted at the next real execution.
- The real execution state of the worker, planner and main agent now drives the task status badge uniformly, fixing a problem where a running worker still showed as “idle”.
- The worker session retains per-item controls and the current session's token summary, with the model name moved to display to the right of the current session's title.

#### Removed

- Removed multi-select, select-all, and the bulk pause/continue UI for workers, along with the worker bulk-control API and its mock contract.
- Removed the token badge and tooltip from worker list rows; the underlying full token ledger and the task-aggregation endpoint remain.

### LLM

#### New features

- Tasks now support an ordered LLM configuration chain; when a provider's quota being insufficient is explicitly recognized, it automatically switches to the next configuration, persisting the current configuration, the exhausted state, and an error summary.
- Running and paused tasks support editing the full configuration chain, reordering it, and manually switching the current configuration, taking effect from the next LLM call.
- Automatic switches, manual switches, and the whole chain being exhausted are written as structured system activity, and surfaced as a deduplicated alert via the task activity stream.
- Added a task-role model resolution endpoint, unifying how the configuration and model used for the main agent's, planner's and worker's next call are resolved.
- Added an optional global LLM pool, supporting a configurable call order, a fallback on a specified model's failure, health status, cooldown recovery, and manual reset.
- Added an always-on LLM usage ledger, aggregating input, output and cache tokens by task, session, model and configuration.

#### Changes

- Goal decomposition, the planner, the worker and the main agent now share a task-level LLM runtime (see the resolution priority below).
- Failover now only responds to explicit quota, balance or billing errors; ordinary rate-limit, auth, network, server-side and context errors no longer trigger a mistaken switch.
- The LLM configuration page was changed to a card list with a right-side drawer for editing, and now provides pool polling order, priority, exclusions, health status and recovery actions.
- The configuration-chain-exhausted message now wraps on narrow screens; the current model moved from a session-list icon to a text label next to the current session's title, with a full tooltip.
- The order in which each role within a task resolves its LLM was changed to “agent binding → task configuration chain → global”: a role with an explicitly bound model always runs on that model, an unbound role falls back to the task chain, and if the task chain is empty it falls back further to the global configuration.
- An empty outbound proxy setting now means a direct connection; it no longer falls back to the `HTTP_PROXY`/`HTTPS_PROXY` environment variables (an explicit `ARTEX_LLM_PROXY` is unaffected); the proxy input now supports `socks5://user:pass@host:port` with credentials.

#### Bug fixes

- A transient streaming failure before any output has been produced now retries with a safe backoff on the same provider, significantly reducing the case of “only one or two tools ran before it broke off, no summary, ending in `model_error`”; quota exhaustion, overly long context, and deterministic 4xx errors are not retried, and are still handled by failover and compaction recovery respectively.

#### Removed

- Removed the LLM icon-style indicator from the session list, to avoid confusion with the worker status icon.

### UI

#### New features

- Tasks support creating, renaming, deleting and filtering global categories; a new task can select a category directly.
- Added global task-template CRUD and a right-side management drawer; creating a task can load a preset description and goals, and the current content can be saved as a template.
- Creating a task now supports linking multiple source tasks and multiple company asset scopes; a company scope is entered as a single multi-line text box that auto-recognizes domains, URLs, IPs, CIDRs, ICP numbers and company keywords.
- Task test assets support adding and removing in real time, recording the source as manual, company, inherited, or agent-discovered; the current test assets and a source summary are shown next to the worker session title.
- The findings page is now grouped by task with independent pagination within each group, and supports filling in a description for a finding and creating a high-priority drill-in intent.
- Chat supports renaming, pinning, unpinning and deleting; the task list supports bulk pause and continue for the current page.
- Traffic detail and tool-execution detail were changed to a right-side drawer; HTTP messages now have `Host` filled in and highlight the request line, status code, headers, JSON, and markup-language bodies.
- Added “Details” and “Pause/Continue” buttons to the task list's action column; the chat send key can now be configured in system settings (`Enter` / `Cmd+Enter`, etc.), and a proxy input was added to the web-search area.
- The badge to the right of the session title now shows the LLM configuration name, with the model id moved to a hover tooltip; a task can have an optional name, and the task list gained a name column and search (falling back to the description when empty).
- The skills page now shows usage count, last-used time, and missing-dependency statistics, making it easier to find skills that aren't taking effect or aren't installed.

#### Changes

- The task title is now a focusable link to its detail page; task assets, company assets, finding task groups, and findings within a group now use real server-side pagination, stable sorting, and exact totals.
- Adding a company now uses a right-side drawer, with scope entry unified into multi-line text with real-time recognition, validation, and a type preview.
- The main agent's input box supports auto-growing, `Enter` to send, `Shift+Enter` for a new line, and avoids sending accidentally while composing with an IME.
- Agent preview, task reports, and related detail views now all use a shared Markdown renderer; delete confirmation, long error messages, and mobile drawer widths were fixed consistently.
- The task-template selector now displays and searches by template name while submitting the template id; the system's original font size was restored, and the app version now consistently shows as `0.3.3`.
- The task total and current-session token figures now only highlight the input, cache-read and output values; the send button uses a simpler up-arrow icon.
- The test scope in the task overview now shows company names rather than ids.

#### Bug fixes

- Plain text containing a dot is no longer misidentified as an ICP registration number.
- Fixed a name collision in the variable directory with global runtime variables (such as `{{.Now}}`), which caused the agent editor's variable list to render duplicate keys.

#### Removed

- Removed the task card's standalone “Enter” button, unifying entry into the detail page through clicking the task title.
- Removed the dashboard's “New task” button, the logo URL input when adding a company, and the finding-count badges at the top of the findings page and within task groups.
- Reverted the global 10% font-size increase, and removed the worker selector, model icon, and row-level token stats from the session list.

### Agent

#### New features

- A new task can now directly link multiple existing tasks, inheriting the directly-linked source tasks' goals, facts, findings, completed intents, asset scope and blackboard context in real time, read-only.
- Blackboard read tools support querying a source task's nodes, facts, findings and execution traces on demand; inherited nodes carry a source marker, and all write tools reject modifying them.
- Company-scope tools support domains, URLs, IPs, CIDRs, ICP numbers and company keywords; keywords serve only as a scope hint for the agent and are not used for automatic asset attribution.
- Drilling into a finding now creates a high-priority manual worker intent in the original task, with an asset anchor and a `derived_from` edge.
- Added a “Goal management” card to the overview, supporting manually viewing, adding, modifying and deleting goals; adding or modifying one notifies the planner and revives the task, while deleting hard-deletes the goal node (cascading the deletion of its edges and anchors) without reviving the task.
- The main agent now gets the `steer_work` tool by default, letting it inject a course-correction instruction into a running worker in real time without interrupting it or losing progress (it still verifies the intent belongs to the current task).

#### Changes

- A source task's intents do not enter the new task's frontier; the new task keeps using its own independent exploration, execution queue, working directory, and conversation history.
- Deleting a running intent no longer destroys its data: it is now stopped to `stopped`, requires a deletion reason, attaches the reason to the intent as a fact and writes it into the payload, and the planner is made aware of it via a `cancelled` trigger (keeping the intent's content and the reason).
- The main agent conversation is now driven by the server-side activity stream, with input recoverable after a failed send; opening a session now auto-scrolls to the bottom and keeps the last reply visible after its detail lazy-loads.
- Pausing a task terminates the current main agent, planner and worker calls, but does not prevent the user from starting a new main-agent orchestration session while paused.
- Cancellation, closing, and a streaming interruption now all uniformly preserve the real termination reason, the content already generated, the number of rounds run, the elapsed time, token usage, and any tool calls that never returned.

#### Removed

- Removed the main agent's client-side optimistic message echo, avoiding duplicate messages and cross-session interleaving under pausing, failure, or concurrent activity.

### Constraints

#### New features

- The goal-decomposition stage now extracts operational constraints (allow/deny) before decomposing goals, and the main agent can add to them at runtime; constraints are injected into the planner's and worker's system prompts at the highest priority, so that diversity- and breadth-seeking exploration explicitly obeys them even after debiasing.
- Added a “Constraint management” card and create/read/update/delete endpoints to the overview; constraint injection scope can be toggled separately for the planner / worker (both on by default, read every round, taking effect immediately). The new `task_constraints` table uses `CREATE TABLE IF NOT EXISTS`, automatically created on existing databases.

### Interception

#### New features

- Command interception gained model-fallback approval alongside regex/string rules: when not a single rule matches, the model makes an `ALLOW`/`ASK`/`DENY` semantic judgment, with a configurable failure fallback action and approval-timeout action; the interception page was split into “Intercept rules / Model configuration” tabs, and a model's verdict is labeled with a `[Model]` prefix and its reasoning.

#### Bug fixes

- Hardened parsing of the judge's output, preventing a correct `DENY` verdict from being mistakenly treated as an allow.

### Traffic

#### Changes

- Traffic recording now stores each whole exchange in SQLite (with large bodies spilling into a hash-deduplicated blob bucket), with a trigram full-text index built over text bodies, supporting arbitrary substring and Chinese-text search; deletion is now reduced to a single SQL transaction, dropping from hours to milliseconds while no longer stalling recording during the process. Added streaming download for large bodies and a full-text parameter for `traffic_search`; old file-tree data needs no migration and remains readable, searchable and deletable.

### Tasks and assets

#### New features

- Added an “Asset coverage” toggle to task creation (on by default): turning it off stops computing and displaying coverage, makes the state graph show only assets, stops automatically accumulating `task_scope`, and removes the related tools from the planner / main agent; company linking is unaffected by this toggle.
- `insert_assets` gained a `related` flag per asset (default `true`): it only has an effect when coverage is enabled, and `false` means the asset only goes into the shared asset store without counting toward this task's coverage (for example, an incidentally discovered neighboring site or an unrelated asset).
- Company scope and task test assets both now support recognizing domains, URLs, IPs, CIDRs, ICP numbers and keyword text uniformly; domains/IPs can create or reuse a global asset, while CIDR/ICP/keywords are kept as task-scope context.

### Build

- `build.sh --release` now builds Linux amd64/arm64, macOS amd64/arm64 and Windows amd64 in one go, producing a zip release package with `skills/`, a config example and the README.
- Debug info is stripped with the Go linker when producing the zip release package; UPX was made explicitly opt-in, avoiding its self-extracting ELF segfaulting on startup in some Linux environments.
- The release workflow now runs a startup smoke test on the Linux amd64 binary, and ships `SHA256SUMS` with the release package.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.2] - 2026-08-20

### Added

- Creating a task now supports linking multiple existing tasks, inheriting the directly-linked source tasks' facts, findings, completed intents, asset scope and blackboard context in real time, read-only; the new task still uses its own independent execution queue, working directory and conversation history.
- Tasks support an ordered LLM configuration chain. When a provider's quota being insufficient is explicitly recognized, it automatically switches to the next configuration, persisting the current configuration, the exhausted state, and structured audit activity.
- Running and paused tasks support editing the LLM configuration chain, reordering it, and manually switching the current configuration; automatic switches, manual switches, and the whole chain being exhausted are shown on the task detail page.
- A running worker now supports pause, resume and cancel. Pausing preserves blackboard data, while canceling transactionally cleans up the intent's facts, findings and execution records after the worker stops writing.
- Deleting a task can optionally also clean up its linked assets, traffic, findings and task-landed files, and gained a deletion fence, concurrency protection, and auditable deletion statistics.
- Added an optional per-task concurrency cap; new tasks that hit the cap are queued FIFO and started automatically once a running slot frees up.
- Added pagination for task assets, pagination for company assets, task-category statistics, and a paginated-intent mock contract.
- Added the `build.sh` single-binary build script, supporting static frontend export, asset embedding, cross-platform targets, and build-version injection.

### Changed

- A task-level explicit LLM configuration chain now coexists with the existing global LLM pool; the explicit chain continues to use strict quota-failover semantics, while without an explicit chain the agent-binding and global-configuration rules still apply.
- The main agent's input box supports auto-growing multi-line input, `Enter` to send, `Shift+Enter` for a new line, and avoids sending accidentally while composing with an IME.
- The planner, worker and main agent share a task-level LLM runtime; the task chain uses the smallest context window among its candidate models as the safe compaction threshold.
- The task detail page now shows running, idle and paused states based on real LLM call state; a source task's facts, findings, intents, asset references and graph nodes are all uniformly marked with their source and kept read-only.
- Agent preview, task reports and related detail views now all use a shared Markdown rendering component.
- LLM model configuration now uses a card-and-drawer interaction, with the model list independently scrollable within the drawer.
- Pausing a task no longer blocks the main agent conversation: a main agent orchestration session is now independent of task pausing, and new messages can still be sent while paused (pausing only terminates the round currently in progress).
- The default wall-clock duration for a single worker run was changed from 600 seconds to 1200 seconds.

### Fixed

- Removed the optimistic echo from the main agent console in favor of pure server-side data rendering, fixing message mix-ups and cross-session bleed-through in scenarios such as pausing or a failed send.
- Opening a main agent session now scrolls to the bottom by default and fully shows the last reply: the last reply's full content auto-scrolls to the bottom once it lazy-loads and expands, instead of being pushed off-screen.
- Fixed the main agent session continuing to run after a task was paused, and the orchestration agent not stopping the main agent in sync when it paused the task.
- Fixed the task status badge and action-button state getting out of sync with whether the task was actually done, or the planner, worker or main agent was actually running.
- Fixed a possible late blackboard write or leftover file arising between worker cancellation, task deletion, and concurrent writes.
- Fixed the Markdown preview breaking, long text overflowing the delete confirmation, mobile width issues, and some misaligned buttons on the task detail page.
- Added a named termination reason to every agent cancellation path; activity detail can now show who canceled it, the final state, the number of rounds run, elapsed time, token usage, and any tool calls that never returned.
- Fixed a race where the parent context could overwrite the named `shutdown` reason first during backend shutdown, and fixed garbled text caused by truncating a Chinese activity summary by byte count.
- Fixed a cancellation event with partial streamed output losing its real termination reason, while still preserving the content already generated before cancellation.

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
