export class SlidingWindowCounterLimiter {
  constructor({ now = () => Date.now() } = {}) {
    this.now = now; // function that gives current time
    this.boxes = new Map(); // current + previous box counts per key
  }

  check({ key, limit, windowMs }) {
    const currentTime = this.now(); // time right now
    const boxIndex = Math.floor(currentTime / windowMs); // which fixed box we're in
    const boxStart = boxIndex * windowMs; // when this box began

    let entry = this.boxes.get(key); // find this key's box data

    if (!entry || entry.boxIndex !== boxIndex) {
      const previousBox = entry && entry.boxIndex === boxIndex - 1 ? entry.currentBox : 0; // carry old count forward if adjacent
      entry = { boxIndex, currentBox: 0, previousBox }; // start a new box
      this.boxes.set(key, entry); // save it
    }

    const elapsedInBox = currentTime - boxStart; // how far into this box we are
    const overlapRatio = Math.max(1 - elapsedInBox / windowMs, 0); // how much of previous box still counts
    const estimatedCount = entry.currentBox + entry.previousBox * overlapRatio; // blended guess

    const allowed = estimatedCount < limit; // still under the limit?

    if (allowed) {
      entry.currentBox += 1; // spend the quota
    }

    return {
      allowed, // true = let it through
      remaining: Math.max(Math.floor(limit - estimatedCount), 0) // quota left
    };
  }
}
