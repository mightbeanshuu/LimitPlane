// LimitPlane public surface — what "using it for your site" imports.
//
//   import { createLimitPlane } from "limitplane";
//   const lp = createLimitPlane({ policy: myPolicy });
//   app.use(lp.middleware);            // Express: that's the whole install
//
// Everything else exported here is for people who want the raw parts.

export { createLimitPlane } from "./gateway/limitPlane.js";
export { PolicyEngine, classifyCost, COST_CLASSES } from "./gateway/policyEngine.js";
export { AuditLog } from "./gateway/auditLog.js";
export { createBilling, TenantStore, PLANS } from "./billing/stripeBilling.js";
export { MonthlyQuotaLimiter } from "./algorithms/monthlyQuotaLimiter.js";

// The raw muscles, for direct use or benchmarks:
export { TokenBucketLimiter } from "./algorithms/tokenBucketLimiter.js";
export { FixedWindowLimiter } from "./algorithms/fixedWindowLimiter.js";
export { SlidingWindowCounterLimiter } from "./algorithms/slidingWindowLimiter.js";
export { SlidingWindowLogLimiter } from "./algorithms/slidingWindowLogLimiter.js";
export { LeakyBucketLimiter } from "./algorithms/leakyBucketLimiter.js";
