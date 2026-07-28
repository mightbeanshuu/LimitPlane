# LimitPlane Sessions

## Current State
- Current session: 11 complete - all 7 limiters built/tested + the gateway layer is LIVE: drop-in middleware, multi-tenant policies, AI cost classes, audit log, and the protected `/v1/demo/nsfw-check` demo route
- Current level: Beginner-to-Interview
- Last completed: Sessions 10+11 (combined build, 2026-07-28) - HTTP server + universal middleware + policy engine (tiers, cost classes, tenant:tier:route keys) + audit log; 14/14 unit tests, live curl demo verified
- Active learner gaps: dynamic config reload, graceful degradation, metrics/observability; revise the new gateway files via the README "how to read this codebase" table
- Current blocker: None
- Learner adaptation profile: See section below
- Learning route: Basic to Interview
- Prerequisite check: Completed on 2026-06-30; learner knows CRUD, basic JS, async/await, Promises, and functions; now understands all limiter algorithms, Redis/Lua atomicity, and the gateway/middleware/policy layer.
- Recurring difficult topics: Noisy neighbor and "why return 429 before expensive work" need revisit.
- Next action: Session 12 (dynamic config reload), or jump to Session 23 Helper 2 (blocked-request explanations) now that audit events exist to feed it.

## Learner Adaptation Profile
- Grasp speed: Calibrating
- Confidence level: Medium
- Strong areas: CRUD concepts, basic JavaScript, async/await, Promises, functions
- Weak areas: HTTP internals, headers/status codes, middleware, rate limiting internals, Redis, distributed systems, production purpose of pre-handler blocking
- Best explanation style: Step-by-step plain English with tiny examples, then project mapping
- Repetition needed: HTTP lifecycle, noisy neighbor, rate limiter request decision flow, 429 as protection plus communication
- Spaced-repetition items: Noisy neighbor, request/response lifecycle, headers/status codes, fixed-window mental model
- Recurring difficult topics: None confirmed yet
- Latest calibration note: Learner wants clean code with minimal comments (unless explicitly requesting inline comments for a specific block, then add one-liners on each meaningful line). Explain code flow in chat after writing each full block. As of 2026-07-04, learner asked to move faster and more straightforward — use bigger steps, less repetition, keep momentum. Best explanation format confirmed by learner: real-world analogy first (jar/bucket/water), then piece-by-piece code walkthrough, then a text-based flowchart for test/output tracing, then a keyword table for new terms. Learner explicitly asked to keep using this exact format going forward.

## Session Plan
- [x] Session 1: HTTP/API basics - request lifecycle, status codes, headers, clients, servers
- [x] Session 2: Why rate limiting exists - abuse, fairness, reliability, cost, noisy neighbors
- [x] Session 3: Fixed-window in-memory limiter
- [x] Session 4: Sliding window log and sliding window counter
- [x] Session 5: Token bucket and leaky bucket
- [x] Session 6: Data structures behind each algorithm
- [x] Session 7: Concurrency, race conditions, and atomicity
- [x] Session 8: Redis basics for rate limiting - built `src/algorithms/redisFixedWindowLimiter.js` using INCR+EXPIRE, integration-tested against real local Redis
- [x] Session 9: Redis Lua scripts for atomic decisions - built `src/algorithms/redisLuaFixedWindowLimiter.js` combining INCR+EXPIRE into one atomic Lua script via `client.eval`, integration-tested including a TTL check to prove the expiry was actually set
- [x] Session 10: Distributed architecture - built the real HTTP server (`src/server.js`) + universal drop-in middleware (`src/gateway/limitPlane.js`) including the protected demo route `/v1/demo/nsfw-check` backed by a deterministic stub classifier (`src/demo/nsfwStub.js`)
- [x] Session 11: Multi-tenant policies - built `src/gateway/policyEngine.js` (free/pro/enterprise tiers, per-route AI cost classes light=1/standard=2/heavy=5, key shape `tenant:tier:route`, anon-by-IP fallback) + `src/gateway/auditLog.js` decision events; unit tests cover allowed/blocked/reset/tenant-isolation/headers/429
- [ ] Session 12: Dynamic config reload
- [ ] Session 13: Graceful degradation during Redis outage
- [ ] Session 14: Metrics and observability
- [ ] Session 15: Docker Compose and local development environment
- [ ] Session 16: Load testing and interpreting results
- [ ] Session 17: Abuse detection and suspicious-client tracking
- [ ] Session 18: Admin API/dashboard concepts
- [ ] Session 19: Testing strategy - unit, integration, concurrency, failure, and load
- [ ] Session 20: Documentation and architecture diagrams
- [ ] Session 21: Interview system design
- [ ] Session 22: Resume, GitHub polish, and demo script
- [ ] Session 23 (added later, after core project): Policy Copilot GenAI upgrade - English-to-policy-config and blocked-request explanation helpers, see "Project Upgrade - Policy Copilot" section below

## Project Upgrade - AI-Aware Rate Limit Gateway
- Status: Accepted for MVP scope
- Source reviewed: `DIP_Problem_statements_6th_sem.pdf`, parsed locally to `.firecrawl/dip_problem_statements_6th_sem.md`
- Relevant PDF idea: NSFW Website Detection API needs real-time crawling, text/image classification, JSON confidence output, and explicit rate limiting/safe crawling.
- Upgrade: Frame LimitPlane as a distributed rate limiter for normal APIs plus expensive AI APIs.
- GenAI use: Optional deterministic-first adapters for AI request cost classification, quota explanation, and policy suggestions; real LLM provider can be added later behind an interface.
- MVP features:
  - Protected demo AI route such as `/v1/demo/nsfw-check`
  - Fixed-window in-memory limiter first
  - Per-tenant and per-route policies
  - AI request cost class: `light`, `standard`, `heavy`
  - `429` response with rate-limit headers
  - Audit event explaining allowed/blocked decisions
  - Tests for allowed, blocked, reset, and tenant isolation
- Two-to-three-day plan:
  - Day 1: Node/TypeScript setup, fixed-window limiter, middleware, demo route, unit tests
  - Day 2: Redis/Lua upgrade, policies, headers, audit logs, integration tests
  - Day 3: Docker, metrics, load test, README diagrams, demo polish
- Project score estimate:
  - Current implementation score: 18/100 because scaffold and docs exist but code is not written yet
  - Proposed project idea score after upgrade: 88/100 if completed with tests, Redis/Lua, docs, and demo
  - Likely two-day MVP score: 72/100
  - Likely three-day polished score: 82/100
- Next action: Start Session 3 and then write the first fixed-window implementation.

## Project Upgrade - Policy Copilot (GenAI, planned for later)
- Status: Discussed and agreed, not yet built - to be added near the end of the project
- Idea: Two small optional GenAI helpers, both deterministic-first with a fallback that works without any LLM call.
  - Helper 1 - "English to Config": admin types a plain-English rate-limit policy (e.g. "Free tier gets 20 requests a minute, throttle the NSFW image-scan route to 3 a minute, Pro tier gets 5x that") and the LLM converts it into the actual validated policy JSON (tenant/route/limit/windowMs shape). Uses Claude's structured outputs (`output_config.format` with a schema) so the result is guaranteed valid JSON or fails loudly - never silently wrong.
  - Helper 2 - "Numbers to Explanation": when a request is blocked, instead of a static string like `"blocked: over limit"`, the LLM reads the real audit-trail facts (count, window, tenant, route, timing) and writes a specific human-readable explanation plus a recommendation (e.g. "47 requests in 8 seconds - looks like a retry-loop bug, not an attack").
- Why this over a plain classifier: a light/standard/heavy cost classifier could mostly be done with `if` statements and isn't a strong showcase. These two helpers do things plain code genuinely cannot do well: parsing free-form English into strict config, and writing a fresh correct sentence for every unique situation.
- Recommended model: Claude Haiku 4.5 (cheap/fast, plenty for short classification/generation tasks; ~$0.0004 per call at this scale).
- Design constraint: both helpers sit behind a plain function interface (e.g. `classifyCost(request, classifier = deterministicDefault)`) so the deterministic version is always the default and the LLM version is a pure swap-in - nothing breaks or requires an API key unless explicitly enabled.
- Build order agreed: Helper 2 (explanation writer) first since it is simpler and end-to-end demoable quickly; Helper 1 (English to config) after.
- When to build: near the end of the project, after the core rate limiter, Redis/Lua, server/middleware, and multi-tenant policies (Sessions 8-13) are done - this is a polish/differentiation layer, not core functionality.
- Next action: Revisit this after Session 13 (graceful degradation) or once multi-tenant policies (Session 11) exist for Helper 1 to plug into.

## Session 0 - Tutor Setup
- Status: Complete
- Concepts taught: None
- Examples used: None
- Project mapping: Tutor workflow created for LimitPlane
- Doubts asked: None
- Answers given: None
- Learner gaps noticed: Unknown
- Grasp-speed signal: Not assessed yet
- Adaptation used: Adaptive-learning profile added for future sessions
- Learning route: Not selected during setup
- Prerequisite check: Not completed during setup
- Small terms explained: None
- Recurring difficult topics: None yet
- Commands run: See `progress.md`
- Files changed: Historical setup files were later removed from this repo during repo split; current tracker reconstructed on 2026-07-04.
- Stuck points: None
- Revision PDF: Not required for setup
- Next session: Session 1 - HTTP/API basics

## Session 1 - HTTP/API Basics
- Status: Complete, reconstructed from historical `sessions.md` and `progress.md`
- Concepts taught: HTTP request/response lifecycle, clients, servers, status codes, headers, API basics
- Examples used: Request as a conversation between client and server
- Project mapping: Every rate limiter decision happens in the middle of an HTTP request before the app returns a response.
- Doubts asked: Not fully preserved in current repo
- Answers given: Not fully preserved in current repo
- Learner gaps noticed: HTTP internals were unknown before the session
- Grasp-speed signal: Learner can handle JS basics; needs slower explanation for HTTP internals
- Adaptation used: Basic-to-Interview route
- Learning route: Basic to Interview
- Prerequisite check: Learner knows CRUD, JS, async/await, Promises, functions
- Small terms explained: Planned terms include `req`, `res`, `next`, middleware, status code, header
- Recurring difficult topics: Request lifecycle should be revisited
- Commands run: `node --version` was run by learner and returned `v24.14.0`
- Files changed: `sessions.md`, `progress.md` historically
- Stuck points: None recorded
- Revision PDF: Not found in current repo
- Next session: Session 2 - Why rate limiting exists

## Session 2 - Why Rate Limiting Exists
- Status: Complete, reconstructed from `progress.md`
- Concepts taught: Abuse, fairness, reliability, cost control, noisy neighbors
- Examples used: Not fully preserved in current repo
- Project mapping: LimitPlane protects APIs by deciding whether each client/request should be allowed or blocked.
- Doubts asked: Not fully preserved in current repo
- Answers given: Not fully preserved in current repo
- Learner gaps noticed: Noisy neighbor needs a second checkpoint; learner connected `429` to telling the user, but needs the backend-protection reason repeated
- Grasp-speed signal: Learner said understood, but explain-back was not captured; latest checkpoint answer was partially correct
- Adaptation used: Mark noisy neighbor as revisit item
- Learning route: Basic to Interview
- Prerequisite check: Session 1 background applies
- Small terms explained: rate limit, abuse, fairness, reliability, cost, noisy neighbor
- Recurring difficult topics: Noisy neighbor, pre-handler blocking, `429` headers
- Commands run: None recorded for this teaching step
- Files changed: `progress.md` historically
- Stuck points: None recorded
- Revision PDF: Not found in current repo
- Next session: Session 3 - Fixed-window in-memory limiter

## 2026-07-04 - Project Scaffold
- Status: Complete
- Concepts taught: Folder responsibility and future request-flow ownership
- Examples used: None
- Project mapping: Created folders for server, middleware, algorithms, policies, Redis/Lua, metrics, config, errors, types, tests, docs, load testing, Docker, infra, and scripts.
- Doubts asked: None
- Answers given: Scaffold kept lightweight; no dependencies installed and no algorithm code written.
- Learner gaps noticed: None
- Grasp-speed signal: Not assessed in this setup step
- Adaptation used: Kept setup separate from teaching so notes can start cleanly
- Learning route: Basic to Interview
- Prerequisite check: Prior Session 1 check applies
- Small terms explained: scaffold, middleware, algorithm, policy, metrics
- Recurring difficult topics: None
- Commands run: `mkdir -p src/server src/middleware src/algorithms src/policies src/redis/lua src/metrics src/config src/errors src/types tests/unit tests/integration tests/concurrency docs/diagrams scripts load docker infra`; verification commands listed in `progress.md`
- Files changed: `README.md`, scaffold `.gitkeep` files, `sessions.md`, `progress.md`
- Stuck points: None
- Revision PDF: Not required for scaffold
- Next session: Revise Session 1 and Session 2

## Session 3 - Fixed-Window In-Memory Limiter
- Status: In progress, first code slice complete
- Concepts taught: Fixed window, request count, time window, per-key isolation, reset time, weighted AI request cost
- Examples used: 3 requests per 60 seconds, then block; AI heavy request costs more than a light request
- Project mapping: `src/algorithms/fixedWindowLimiter.js` stores request counts by tenant/route key before middleware exists.
- Doubts asked: Checkpoint: "Fixed window means ______, and it blocks when ______."
- Answers given: Learner answered that fixed window is a span of time where a user is permitted to make requests and it blocks when the count increases.
- Learner gaps noticed: Mostly correct; needs precision that normal count increases are allowed until the next request would exceed the limit.
- Grasp-speed signal: Good enough to start coding; learner understands the time-box idea.
- Adaptation used: Started with pure Node implementation and tests without external dependencies.
- Learning route: Basic to Interview
- Prerequisite check: Learner knows JS basics and can now trace a small class with a `Map`.
- Small terms explained: fixed window, count, key, limit, `Map`, cost, reset, `allowed`, `remaining`
- Recurring difficult topics: Exact block condition, per-key isolation, distributed-memory limitation
- Commands run: `npm test`, `node --version`, `date '+%Y-%m-%d %H:%M %Z'`
- Files changed: `package.json`, `src/algorithms/fixedWindowLimiter.js`, `tests/unit/fixedWindowLimiter.test.js`, `sessions.md`, `progress.md`
- Stuck points: None
- Revision PDF: Pending
- Next session: Trace the code line by line, then let the learner write the next small unit test and implementation increment.

## 2026-07-04 - Learner-Driven Coding Mode
- Status: Active
- Concepts taught: How good developers begin implementation from behavior, tests, data shape, and edge cases before typing code
- Examples used: Fixed-window limiter as behavior-first implementation
- Project mapping: Learner will write future code slices first; agent will explain, review, and only take over if learner is stuck.
- Doubts asked: User asked to explain from zero and not write code for them unless they cannot.
- Answers given: Use behavior-first workflow: define outcome, choose file, define input/output, write one test, implement minimum logic, run test, refactor.
- Learner gaps noticed: Needs code-writing process, not just concept explanation.
- Grasp-speed signal: Learner is ready to code but wants guided structure.
- Adaptation used: Switch from implementation-driver to tutor/reviewer mode.
- Learning route: Basic to Interview
- Prerequisite check: Learner knows JS basics; start with one small test or function at a time.
- Small terms explained: behavior, input, output, edge case, unit test, implementation, refactor
- Recurring difficult topics: Exact block condition, mapping concept to code
- Commands run: `sed -n '1,420p' /Users/mac/.codex/skills/limitplane-tutor/SKILL.md`; `sed -n '1,260p' sessions.md`; `tail -n 120 progress.md`; `sed -n '1,220p' src/algorithms/fixedWindowLimiter.js`; `date '+%Y-%m-%d %H:%M %Z'`
- Files changed: `sessions.md`, `progress.md`
- Stuck points: None
- Revision PDF: Pending
- Next session: Learner writes first small test or reimplements fixed-window from explanation.

## 2026-07-04 - Scaffold Cleanup
- Status: Complete
- Concepts taught: Build only the folder/file needed for the current lesson
- Examples used: Removed placeholder `.gitkeep` files for future modules
- Project mapping: Current repo now focuses on Session 3 fixed-window limiter and tests. Middleware, Redis, AI adapters, metrics, Docker, and load testing folders will be created only when those sessions begin.
- Doubts asked: User said the previous scaffold was unclear and asked to build scaffolds only when needed.
- Answers given: Keep current code small: algorithm file, unit test file, project upgrade note, trackers, README, package config.
- Learner gaps noticed: Needs the implementation flow before folder architecture.
- Grasp-speed signal: Learner wants slower, clearer sequencing.
- Adaptation used: Reduced repo surface area and switched to session-wise code explanation.
- Learning route: Basic to Interview
- Prerequisite check: Continue from fixed-window mental model.
- Small terms explained: scaffold, placeholder, current lesson file
- Recurring difficult topics: Mapping architecture to concrete code
- Commands run: `find . -maxdepth 4 -type f -not -path './.git/*' -not -path './.firecrawl/*' -not -path './.claude/*' -print | sort`; `npm test`; `date '+%Y-%m-%d %H:%M %Z'`
- Files changed: `README.md`, removed unused `.gitkeep` placeholder files, `sessions.md`, `progress.md`
- Stuck points: None
- Revision PDF: Pending
- Next session: Explain `fixedWindowLimiter.js` from zero in parts.

## 2026-07-04 - Full Session-Wise Reset
- Status: Complete
- Concepts taught: Keep only learning trackers until the learner creates the next needed folder/file.
- Examples used: Removed `README.md`, previous docs, package config, algorithm code, tests, and empty folders.
- Project mapping: Active project files are now only `.gitignore`, `sessions.md`, and `progress.md`. The first real project folder will be created by the learner during Session 3.
- Doubts asked: User said the existing files/scaffold were confusing and asked to build folder-by-folder.
- Answers given: Reset to minimal state and switch to "teach first, learner creates file, then guided code" workflow.
- Learner gaps noticed: Needs clear file/folder purpose before seeing implementation.
- Grasp-speed signal: Slow down and avoid prebuilding future architecture.
- Adaptation used: Remove premature files; teach directions first.
- Learning route: Basic to Interview
- Prerequisite check: Continue with fixed-window concept.
- Small terms explained: reset, tracker, source file, test file, folder-by-folder workflow
- Recurring difficult topics: Mapping concept to code and file structure
- Commands run: `find src tests docs -depth -type d -empty -delete`; `find . -maxdepth 4 -type f -not -path './.git/*' -not -path './.firecrawl/*' -not -path './.claude/*' -print | sort`; `git status --short --ignored`; `date '+%Y-%m-%d %H:%M %Z'`
- Files changed: Removed `README.md`, `docs/project-upgrade.md`, `package.json`, `src/algorithms/fixedWindowLimiter.js`, `tests/unit/fixedWindowLimiter.test.js`, empty `src/`, `tests/`, and `docs/` directories; updated `sessions.md` and `progress.md`.
- Stuck points: Previous scaffold/code was too much at once.
- Revision PDF: Pending
- Next session: Session 3A - manually create `src/algorithms/` and understand why this folder exists before writing code.

## 2026-07-04 - Empty Folder Cleanup
- Status: Complete
- Concepts taught: Empty future folders create confusion when the learner has not reached that topic yet.
- Examples used: Removed empty `docker`, `infra`, `load`, and `scripts` directories.
- Project mapping: The project root now has no visible future scaffolds; folders will appear only at the session where they become useful.
- Doubts asked: User asked to remove Docker, infra, load, and similar folders.
- Answers given: Removed them and confirmed only the project root remains as a visible directory.
- Learner gaps noticed: Needs importance and purpose of each folder before it exists.
- Grasp-speed signal: Learner prefers one concept/file/function at a time.
- Adaptation used: Future code will be written function by function only after explaining expected output and note-taking points.
- Learning route: Basic to Interview
- Prerequisite check: Continue with Session 3A.
- Small terms explained: empty directory, future scaffold, session-wise folder creation
- Recurring difficult topics: Folder purpose and code flow
- Commands run: `rmdir docker infra load scripts`; `find . -maxdepth 3 -type d -not -path './.git*' -not -path './.firecrawl*' -not -path './.claude*' -print | sort`; `date '+%Y-%m-%d %H:%M %Z'`
- Files changed: Removed empty directories `docker/`, `infra/`, `load/`, `scripts/`; updated `sessions.md`, `progress.md`
- Stuck points: None
- Revision PDF: Pending
- Next session: Session 3A - directions and importance before creating `src/algorithms/`.

## Session 3A - Fixed-Window File Part 1
- Status: In progress
- Concepts taught: Algorithm folder purpose, file boundary, class shell, constructor, in-memory `Map`
- Examples used: `tenant:free:route:nsfw-check` as a rate-limit key
- Project mapping: Created `src/algorithms/fixedWindowLimiter.js` because fixed-window is pure allow/block decision logic.
- Doubts asked: User asked for code function by function, expected output, inference, and notes.
- Answers given: Wrote only the first block: file comments, exported class, constructor, and `this.windows = new Map()`.
- Learner gaps noticed: Needs purpose of each file and output before implementation details.
- Grasp-speed signal: Use small blocks and wait for "write next part."
- Adaptation used: One block at a time; explain before expanding.
- Learning route: Basic to Interview
- Prerequisite check: Learner understands fixed window at a basic level.
- Small terms explained: `export`, `class`, `constructor`, `this`, `Map`, key, value
- Recurring difficult topics: Mapping algorithm concept to code memory
- Commands run: `mkdir -p src/algorithms`; `sed -n '1,120p' src/algorithms/fixedWindowLimiter.js`; `find . -maxdepth 4 -type f -not -path './.git/*' -not -path './.firecrawl/*' -not -path './.claude/*' -print | sort`; `date '+%Y-%m-%d %H:%M %Z'`
- Files changed: `src/algorithms/fixedWindowLimiter.js`, `sessions.md`, `progress.md`
- Stuck points: None
- Revision PDF: Pending
- Next session: Wait for "write next part", then add the `check()` method signature only.

## Session 3A - Fixed-Window File Part 2
- Status: Complete
- Concepts taught: `check()` method input contract and structured output
- Examples used: `{ key, limit, windowMs, cost }`
- Project mapping: `check()` will become the main allow/block decision function; currently it echoes input so the learner understands the shape first.
- Doubts asked: User asked agent to decide code parts, write each block, then explain output and notes.
- Answers given: Added `check({ key, limit, windowMs, cost = 1 })` and returned those fields.
- Learner gaps noticed: Needs to understand function input/output before internal algorithm logic.
- Grasp-speed signal: Teach by observable outputs first.
- Adaptation used: Small non-final function block before adding time and count logic.
- Learning route: Basic to Interview
- Prerequisite check: Learner understands class memory part at a high level.
- Small terms explained: method, parameter object, default value, `return`, structured output
- Recurring difficult topics: Function contracts and output inference
- Commands run: `sed -n '1,180p' src/algorithms/fixedWindowLimiter.js`; `date '+%Y-%m-%d %H:%M %Z'`
- Files changed: `src/algorithms/fixedWindowLimiter.js`, `sessions.md`, `progress.md`
- Stuck points: None
- Revision PDF: Pending
- Next session: Add current time and window lookup.

## Session 3A - Fixed-Window File Part 3
- Status: Complete
- Concepts taught: Clean code style, injectable time function, current time capture, window lookup by key
- Examples used: `this.windows.get(key)`
- Project mapping: The limiter now knows the current time and can search memory for the request key.
- Doubts asked: User asked to remove excessive comments from code and explain in chat instead.
- Answers given: Removed bulky code comments, added `now` injection, `currentTime`, and `currentWindow` lookup.
- Learner gaps noticed: Needs clean code plus separate explanation, not comments embedded everywhere.
- Grasp-speed signal: Continue in complete code blocks with chat explanation.
- Adaptation used: Minimal comments in source code; richer explanation in session response.
- Learning route: Basic to Interview
- Prerequisite check: Continue from Part 2 input contract.
- Small terms explained: injectable function, `Date.now`, `get`, current window
- Recurring difficult topics: Code flow and runtime output
- Commands run: `sed -n '1,120p' src/algorithms/fixedWindowLimiter.js`; `date '+%Y-%m-%d %H:%M %Z'`
- Files changed: `src/algorithms/fixedWindowLimiter.js`, `sessions.md`, `progress.md`
- Stuck points: None
- Revision PDF: Pending
- Next session: Add new-window creation when no current window exists.

## Session 3A - Fixed-Window File Part 4-5 Complete + Test
- Status: Complete
- Concepts taught: Window expiry check, allow/block decision (count + cost <= limit checked before mutating), remaining and resetAt output, injectable fake time for testing, Node's built-in `node:test` runner
- Examples used: limit 2 / windowMs 1000 — allow, allow, block, then reset after fakeTime advances
- Project mapping: `fixedWindowLimiter.js` is now a complete, working algorithm; first automated test exists proving the full allow/block/reset cycle without waiting in real time.
- Doubts asked: User asked to write the block with one-line comments per line this time, then run it and move faster/more straightforward going forward.
- Answers given: Added expiry check, allow/block logic, remaining/resetAt; wrote a `node:test` unit test; hit and fixed a real bug (test failed first because source file in workspace still had only the Part 3 version — chat code block was never saved to disk); added minimal package.json for `type: module` and `npm test` script; fixed a glob-vs-directory issue in the test script.
- Learner gaps noticed: None new; learner is ready for faster pacing now.
- Grasp-speed signal: Fast — moving to quicker, more direct pacing per learner's request.
- Adaptation used: Larger code blocks per step, less hand-holding, real command execution instead of just describing expected output.
- Learning route: Basic to Interview
- Prerequisite check: Continue from Part 3.
- Small terms explained: isExpired, short-circuit `&&`, `+=`, `Math.max`, glob pattern, `node:test`
- Recurring difficult topics: None new
- Commands run: `node --test tests/unit/fixedWindowLimiter.test.js` (failed, then fixed, then passed); `npm test` (failed on glob, then fixed, then passed)
- Files changed: `src/algorithms/fixedWindowLimiter.js`, `tests/unit/fixedWindowLimiter.test.js`, `package.json`, `sessions.md`, `progress.md`
- Stuck points: None
- Revision PDF: Pending
- Next session: Session 4 - sliding window log and sliding window counter (moving at a faster, more direct pace per learner request).

## Session 4 - Sliding Window Log and Sliding Window Counter
- Status: Complete
- Concepts taught: Fixed-window boundary-burst bug, sliding window as "look back windowMs from now," sliding window log (exact, array of timestamps), sliding window counter (approximate, blended previous/current box counts using overlapRatio)
- Examples used: limit 3/windowMs 1000 log walkthrough (allow, allow, allow, block, then partial slide at t=1050 leaving remaining=0 because only the t=0 timestamp aged out); counter walkthrough with overlapRatio blending previousBox and currentBox
- Project mapping: `src/algorithms/slidingWindowLogLimiter.js` and `src/algorithms/slidingWindowLimiter.js` (counter) created and tested; both fix the fixed-window seam bug, with log being exact/expensive and counter being approximate/cheap.
- Doubts asked: Learner was confused between log and counter; asked why two files exist for "the same thing"; asked what a unit test is and why `tests/unit/`; also caught and asked about a wrong test expectation (remaining assumed 2, actual was 0).
- Answers given: Explained log vs counter as "exact bookkeeping vs cheap math estimate"; explained unit test as isolated single-piece test vs integration/load tests; walked through why only the t=0 timestamp expired by t=1050 (windowStart = currentTime - windowMs = 50, so 200 and 400 survive) and fixed the test's wrong expected value from 2 to 0.
- Learner gaps noticed: Confusion between "exact" vs "approximate" sliding window variants; needed the boundary-burst bug spelled out concretely with numbers before sliding window made sense.
- Grasp-speed signal: Good once given a concrete numeric trace; abstract description alone was not enough.
- Adaptation used: Concrete step-by-step numeric traces (t=0, t=200, t=400...) instead of only formulas; comparison table for log vs counter tradeoffs.
- Learning route: Basic to Interview
- Prerequisite check: Continue from Session 3 fixed-window understanding.
- Small terms explained: `??` nullish coalescing, `.filter()`, `.push()`, `Math.floor`, `overlapRatio`, `boxIndex`, sliding vs fixed window
- Recurring difficult topics: Log vs counter distinction (revisit if confusion resurfaces)
- Commands run: `npm test` (initially failed due to a wrong test expectation in slidingWindowLogLimiter.test.js, fixed remaining assertion from 2 to 0, then passed)
- Files changed: `src/algorithms/slidingWindowLogLimiter.js`, `src/algorithms/slidingWindowLimiter.js`, `tests/unit/slidingWindowLogLimiter.test.js`, `tests/unit/slidingWindowLimiter.test.js`, `sessions.md`, `progress.md`
- Stuck points: Initial test had a wrong expected value; caught by the test itself and corrected.
- Revision PDF: Pending
- Next session: Session 5 - token bucket and leaky bucket.

## Session 5 - Token Bucket and Leaky Bucket
- Status: Complete
- Concepts taught: Token bucket (coin jar that refills over time, spent on request, allows bursts when saved up), leaky bucket (jar that drains at constant rate, filled on request, blocks on overflow, never allows bursts), the mirror-image relationship between the two (subtract-on-request-add-over-time vs add-on-request-subtract-over-time)
- Examples used: Water bottle / coin jar analogy for token bucket; leaking jar analogy for leaky bucket; capacity=3 burst-then-block-then-refill trace for both
- Project mapping: `src/algorithms/tokenBucketLimiter.js` and `src/algorithms/leakyBucketLimiter.js` created, commented (one-liners per meaningful line, added per learner request), and tested.
- Doubts asked: Learner asked to confirm `cost` = request cost (confirmed yes, e.g. heavier AI requests can cost more than 1); asked for token bucket to be broken into simpler/easier words after first pass didn't land; asked what `assert` does; asked for the test's proof walked through step by step with a flowchart.
- Answers given: Confirmed `cost` semantics; re-taught token bucket using a water-bottle analogy broken into 5 small pieces (state, refill math, cap at capacity, allow check, spend); explained `assert.equal`/`assert.ok` as a homework-checker; produced a full step-by-step ASCII flowchart tracing the leaky bucket test from empty bucket through overflow-block to time-based recovery; explained `node --test` summary fields (tests/suites/pass/fail/cancelled/skipped/todo/duration_ms), emphasizing only pass/fail matter day to day.
- Learner gaps noticed: Needed concrete analogy (water bottle / leaking jar) before the code made sense; needed test output fields explained one by one; needed flowchart-style tracing of test assertions, not just prose.
- Grasp-speed signal: Learner grasps quickly once given: analogy first, then piece-by-piece code, then flowchart trace, then keyword table — this exact sequence, confirmed explicitly by learner as the preferred format going forward.
- Adaptation used: Standardized explanation format = analogy -> piece-by-piece code -> flowchart trace of a test -> keyword table. Locked in as default going forward per explicit learner request.
- Learning route: Basic to Interview
- Prerequisite check: Continue from Session 4 sliding window understanding.
- Small terms explained: `capacity`, `refillRatePerMs`, `leakRatePerMs`, `tokens`, `level`, `Math.min`, `Math.max`, `assert.equal`, `assert.ok`, test runner summary fields
- Recurring difficult topics: None new; format preference now locked in as a standing instruction
- Commands run: `npm test` (all 5 algorithm tests passing: fixed window, sliding window log, sliding window counter, token bucket, leaky bucket)
- Files changed: `src/algorithms/tokenBucketLimiter.js`, `src/algorithms/leakyBucketLimiter.js`, `tests/unit/tokenBucketLimiter.test.js`, `tests/unit/leakyBucketLimiter.test.js`, all 5 algorithm files updated with one-line comments, `sessions.md`, `progress.md`
- Stuck points: None
- Revision PDF: Pending
- Next session: Session 6 - data structures behind each algorithm, then Session 7 - concurrency/race conditions/atomicity.

## Session 6 - Data Structures Behind Each Algorithm
- Status: Complete
- Concepts taught: Map<key, tinyObject> pattern shared by fixed window, token bucket, leaky bucket, sliding window counter (constant memory per key); Map<key, array> pattern unique to sliding window log (memory grows with traffic); the real-world memory tradeoff behind why sliding window counter exists as the "cheap" alternative to the log
- Examples used: Notebook/sticky-note analogy (one sticky note per key vs a full receipt list per key); 1000 requests/sec comparison table showing constant vs growing memory across all 5 algorithms
- Project mapping: Ties directly back to all 5 already-built files in `src/algorithms/` - no new code, pure comparison/analysis session.
- Doubts asked: None new; delivered as part of a combined "teach 6 then 7" request.
- Answers given: Structure comparison table across all 5 algorithms; explained Map's role as the outer "fast lookup by key" structure common to all; explained why array-based storage is the accuracy-vs-memory tradeoff specific to sliding window log.
- Learner gaps noticed: None new.
- Grasp-speed signal: Delivered as a comparison/analysis session using the locked-in format (analogy, structure table, flowchart-style memory comparison).
- Adaptation used: Table-first comparison across all algorithms instead of one algorithm at a time, since this session is inherently comparative.
- Learning route: Basic to Interview
- Prerequisite check: Requires all 5 algorithms already built (Sessions 3-5 complete).
- Small terms explained: constant memory, unbounded/growing memory, outer vs inner structure
- Recurring difficult topics: None new
- Commands run: None (analysis session, no code changes)
- Files changed: `sessions.md`, `progress.md` only
- Stuck points: None
- Revision PDF: Pending
- Next session: Session 7 - concurrency, race conditions, and atomicity.

## Session 7 - Concurrency, Race Conditions, and Atomicity
- Status: Complete
- Concepts taught: Race condition (two operations interleave on the same data and produce a wrong result), atomic operation (cannot be interrupted mid-way), why JavaScript's single-threaded synchronous execution currently protects all 5 algorithms, why adding `await` between read and write reintroduces the race condition, why this connects directly to the planned Session 9 (Redis Lua scripts for atomic decisions)
- Examples used: Two-people-reaching-into-one-coin-jar analogy; concrete unsafe async code example (`await db.get` then `await db.save`) contrasted with the learner's actual safe synchronous `check()` methods; ASCII flowchart contrasting safe synchronous execution vs unsafe async interleaving
- Project mapping: Explains why current in-memory `Map`-based code (all 5 algorithm files) is safe as-is today, and previews exactly why Session 8/9 (Redis, Lua scripts) will be necessary once state moves out of a single Node process.
- Doubts asked: None new; delivered as part of a combined "teach 6 then 7" request.
- Answers given: Full flowchart contrasting Request A/B interleaving with vs without `await`; interview-ready one-liner connecting this to Redis atomic operations (`INCR`, Lua scripts) covered later in the plan.
- Learner gaps noticed: None new.
- Grasp-speed signal: Delivered using locked-in format (analogy, piece-by-piece code contrast, flowchart, keyword table).
- Adaptation used: Contrasted "your real current code" against a hypothetical unsafe async version, rather than teaching concurrency in the abstract.
- Learning route: Basic to Interview
- Prerequisite check: Requires all 5 algorithms already built and understood (Sessions 3-6 complete).
- Small terms explained: single-threaded, race condition, atomic operation, await/async gap
- Recurring difficult topics: None new
- Commands run: None (conceptual session, no code changes)
- Files changed: `sessions.md`, `progress.md` only
- Stuck points: None
- Revision PDF: Pending
- Next session: Session 8 - Redis basics for rate limiting.

## Session 10+11 - Gateway Layer: Server, Middleware, Multi-Tenant Policies, Cost Classes
- Status: Complete (built as one combined build session, 2026-07-28)
- Concepts taught: API gateway as a layer in front of routes; why a middleware returns a decision object so the SAME function serves Express (`app.use`) and plain `node:http`; AI cost classes as token prices (light=1, standard=2, heavy=5) so expensive inference spends the shared budget faster; key shape `tenant:tier:route` giving every tenant a separate jar per route (heavy traffic on one route cannot starve light traffic on another); anonymous fallback identity (`anon:<ip>`, free tier) so strangers are limited too; `Retry-After` estimation as missing-tokens / refill-rate; audit log as a ring buffer of decision facts
- Examples used: Free tier = 10-token jar, NSFW scan = 5 tokens -> exactly 2 scans then 429, countable by eye in curl; "the server knows nothing about rate limiting - one `await lp.middleware(req,res)` line is the whole install"
- Project mapping: `src/gateway/policyEngine.js` (brain), `src/gateway/limitPlane.js` (layer), `src/gateway/auditLog.js` (diary), `src/demo/nsfwStub.js` + `src/demo/policy.demo.js` + `src/server.js` (demo site wearing the layer), `src/index.js` (public import surface)
- Doubts asked: None during build; README has a "how to read this codebase" table for later revision
- Answers given: See README design notes (injectable clocks, fail-loudly config, deterministic-first AI seams)
- Learner gaps noticed: Not assessed (build session)
- Grasp-speed signal: Not assessed
- Adaptation used: Every file written in the locked-in teaching-comment style (coin-jar analogies, step-numbered pipeline comments)
- Learning route: Basic to Interview
- Prerequisite check: Sessions 3-9 complete (limiters + Redis/Lua)
- Small terms explained: middleware, ring buffer, cost class, wildcard route, 429/Retry-After
- Recurring difficult topics: None new
- Commands run: `npm test` (14/14 pass), live curl demo (2x200 then 429 on free tier, pro unaffected, audit endpoint verified)
- Files changed: `src/gateway/{policyEngine,limitPlane,auditLog}.js`, `src/demo/{nsfwStub,policy.demo}.js`, `src/server.js`, `src/index.js`, `tests/unit/{policyEngine,limitPlane}.test.js`, `README.md`, `package.json`, `sessions.md`, `progress.md`
- Stuck points: None
- Revision PDF: Pending
- Next session: Session 12 - dynamic config reload (or Session 23 Policy Copilot Helper 2 now that audit events exist to feed it)
