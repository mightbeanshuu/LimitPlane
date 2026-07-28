// AuditLog — the gateway's diary.
//
// Every allow/block decision gets written down as one event, so you can
// answer "WHY was tenant X blocked at 14:32?" with facts, not guesses.
// Later (Session 23, Policy Copilot Helper 2) an LLM reads these same facts
// and writes a human explanation — the data shape here is what feeds it.
//
// It's a ring buffer: a fixed-size notebook where the oldest page gets
// torn out when a new one is written. Memory can never grow forever.

export class AuditLog {
  constructor({ maxEvents = 1000, now = () => Date.now() } = {}) {
    this.maxEvents = maxEvents; // notebook size
    this.now = now; // injectable clock, same trick as the limiters
    this.events = []; // the pages
  }

  // Write one decision down. Returns the event so callers can also emit it.
  record({ allowed, key, route, tenantId, tier, costClass, cost, remaining }) {
    const event = {
      at: this.now(), // when it happened
      allowed, // did it get through?
      key, // which jar was charged
      route,
      tenantId,
      tier,
      costClass,
      cost, // tokens it tried to spend
      remaining, // tokens left in the jar after the decision
    };

    this.events.push(event);
    if (this.events.length > this.maxEvents) {
      this.events.shift(); // tear out the oldest page
    }
    return event;
  }

  // Read the last n pages, newest first (what an admin wants to see).
  recent(n = 50) {
    return this.events.slice(-n).reverse();
  }
}
