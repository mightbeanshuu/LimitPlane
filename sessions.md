# Tech Learning Sessions

## Current State
- Target project: LimitPlane
- Target technology/topic: Distributed multi-tenant rate limiter
- Active tutor skill: tech-learning-tutor
- Current session: 1 - HTTP/API Basics
- Current level: Beginner-to-Interview (Route selected)
- Last completed: Repository and tutor setup (Session 0)
- Active learner gaps: HTTP internals (status codes, headers, request lifecycle) — CRUD known, async/await known
- Current blocker: None
- Learning route: Basic to Interview
- Prerequisite check: Completed (see below)
- Recurring difficult topics: None yet
- Next action: Teach HTTP request/response lifecycle, then map to rate limiting

## Learner Adaptation Profile
- Grasp speed: Unknown; calibrate during Session 1
- Confidence level: Medium — knows JS fundamentals, async/await, promises, CRUD concepts
- Strong areas: async/await, promises, basic JS functions, CRUD operations (conceptual)
- Weak areas: HTTP internals, rate limiting implementation, Redis, distributed systems
- Best explanation style: Unknown; try analogy + tiny code example first
- Repetition needed: Unknown
- Spaced-repetition items: None yet
- Recurring difficult topics: None yet
- Latest calibration note: Skip JS syntax basics; spend time on HTTP internals and why they matter for rate limiting

## Learning Route Options
- Basic to Interview: Start from fundamentals, explain every prerequisite and small code term, then reach project and interview level.
- Medium to Interview: Skip only confirmed-known basics and focus on Redis, Lua, algorithms, distributed systems, tests, and tradeoffs.
- Interview Prep: Use rapid diagnostics, mock interviews, coding drills, system-design prompts, tradeoff practice, and concise revision PDFs.

## Prerequisite Check — Session 1
- What the learner already knows: CRUD operations (conceptual), basic JS, async/await, Promises, writing functions
- Route selected: Basic to Interview
- Known terms: GET, POST, PUT, DELETE, async, await, Promise, function, CRUD
- Unknown terms: HTTP request lifecycle, headers, status codes, middleware, rate limiting internals
- Topics to simplify first: HTTP as a "conversation" before introducing headers/status codes
- Small code terms to explain: req, res, next, middleware — when first encountered in code
- Recurring difficult topics to monitor: None yet

## Session Plan
- [x] Session 1: HTTP/API basics - request lifecycle, status codes, headers, clients, servers (COMPLETE)
- [x] Session 2: Why rate limiting exists - abuse, fairness, reliability, cost, noisy neighbors (COMPLETE)
- [ ] Session 3: Fixed-window in-memory limiter
- [ ] Session 4: Sliding window log and sliding window counter
- [ ] Session 5: Token bucket and leaky bucket
- [ ] Session 6: Data structures behind each algorithm
- [ ] Session 7: Concurrency, race conditions, and atomicity
- [ ] Session 8: Redis fundamentals for rate limiting
- [ ] Session 9: Redis Lua scripts for atomic decisions
- [ ] Session 10: Distributed gateway, app replica, and Redis architecture
- [ ] Session 11: Multi-tenant policies - free/pro/enterprise and route/user/API-key dimensions
- [ ] Session 12: Dynamic config reload and policy management
- [ ] Session 13: Graceful degradation during Redis outage
- [ ] Session 14: Metrics and observability
- [ ] Session 15: Docker Compose and local development environment
- [ ] Session 16: Load testing and result interpretation
- [ ] Session 17: Abuse detection and suspicious-client tracking
- [ ] Session 18: Admin API/dashboard concepts
- [ ] Session 19: Testing strategy - unit, integration, concurrency, failure, load
- [ ] Session 20: Documentation and architecture diagrams
- [ ] Session 21: Interview-level system design
- [ ] Session 22: Resume, GitHub polish, and demo script

## Session 0 - Tutor Setup
- Status: Complete
- Concepts taught: None
- Examples used: None
- Project mapping: Agent tutoring workflow created for LimitPlane
- Doubts asked: None
- Answers given: None
- Learner gaps noticed: Unknown
- Grasp-speed signal: Not assessed yet
- Adaptation used: Adaptive-learning profile added for future sessions
- Learning route: Not selected
- Prerequisite check: Not completed
- Small terms explained: None
- Recurring difficult topics: None yet
- Commands run: See `progress.md`
- Files changed:
  - `progress.md`
  - `sessions.md`
  - `CLAUDE.md`
  - `.claude/skills/tech-learning-tutor/SKILL.md`
  - `/Users/mac/.codex/skills/tech-learning-tutor/SKILL.md`
- Stuck points: None
- Revision PDF: Not required for setup
- Next session: Session 1 - HTTP/API basics
