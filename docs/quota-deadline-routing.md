# Quota deadline routing

`quota-deadline` is an experimental, opt-in strategy for pools of Claude
subscription accounts whose weekly limits reset at different times. Round-robin
can spend capacity on a later-reset account while earlier capacity expires
unused. This strategy places new sessions on the account with the highest required
spending rate, then keeps each ongoing session on its account to preserve caches:

```
urgency = capacity_weight × remaining_weekly_fraction / hours_until_reset
```

For equal-capacity accounts, 79% remaining over 67 hours outranks 36% over 98
hours and 22% over 81 hours. An earlier reset alone is not enough: an account
with almost no remaining capacity can rank below a later-reset account.

This fork is based on upstream 7.3.15, commit
`673131f57484517c3a1eae7e36c4cfa7b9bb4efc`. It does not require the v8 upgrade.

## Enable

Build the fork with Go 1.26 or newer. In an isolated test instance, set:

```yaml
routing:
  strategy: "quota-deadline"
  session-affinity: true
  session-affinity-ttl: "1h"
  session-affinity-subagents: true
```

The existing routing management endpoint accepts `quota-deadline` too. Changing
the strategy hot-reloads the selector. Returning to `round-robin` rolls back the
routing behavior without changing credentials. The default remains round-robin.

This implementation changes the local credential selector. CLIProxyAPIHome and
plugins that take over scheduling bypass it. Other providers and heterogeneous
provider pools retain round-robin behavior. A pool containing only Claude
credentials uses quota routing even when the execution path calls it `mixed`.

## Selection behavior

1. Keep the manager's normal eligibility checks: disabled accounts, token
   failures, supported models, aliases, pinned credentials, retry exclusions,
   and live account/model cooldowns still apply.
2. For Claude, exclude nonpositive credential weights. A missing weight means
   1. Set weights in proportion to usable subscription capacity when plans
   differ; the selector cannot infer token budgets from percentages. Reusing
   arbitrary traffic-share weights will distort the urgency estimate.
3. Read the latest observed shared `5h` and `7d` headers. An unexpired window
   explicitly rejected or at least 100% utilized makes the account unavailable
   until that reset or the observation's freshness deadline, whichever is first.
   When both windows block, both must recover. Choose the highest priority tier
   with surviving candidates, then rank that tier by urgency.
4. Choose maximum urgency, rotating exact ties. The time denominator has a
   one-hour floor to keep the final seconds before reset from dominating.
5. With session affinity enabled, retain an existing usable binding regardless
   of changes in urgency, credential priority, or observation freshness. This
   applies to explicit sessions, inferred conversation matches, and inherited
   parent/subagent bindings. Exhausted windows, disabled accounts, zero weights,
   and normal runtime eligibility checks still cause failover. Once switched,
   the session stays on its new account even if the original account recovers.
   There are no periodic discovery requests and no 80% urgency threshold.

Only shared subscription windows enter the score. `7d_oi`/overage claims are not
treated as a shared weekly budget. Existing model-specific cooldowns remain in
force. The strategy does not independently estimate remaining model-specific
capacity, request cost, or five-hour spending pace. It can exhaust a five-hour
window and then fail over; the purpose is to reduce unused weekly quota.

## Freshness and discovery

The service refreshes Claude OAuth quota at startup and every five minutes while
`quota-deadline` is enabled, using `GET https://api.anthropic.com/api/oauth/usage`
(the endpoint used by the management dashboard). These calls send no prompts,
create no inference cache, and do not execute a model. Existing OAuth credentials
and configured proxies are used; redirects are rejected. Disabled accounts,
API-key credentials, custom upstreams, other providers, and Home scheduling are
excluded. Config changes are checked on each cycle and before each account.
The worker stops with the service. Failed calls are retried on the next cycle,
without using real requests as a fallback probe. The endpoint is provider-specific
and may change; failures retain the last observation until it becomes stale.

The shared `five_hour` and `seven_day` percentages and reset times are converted
to the same quota observations as inference response headers. No model-specific
or overage allowance is treated as a shared weekly budget. A slow poll cannot
replace feedback recorded after that poll started. Polling does not replace
credentials, clear runtime cooldowns, or move session bindings. A manual usage
reset can refresh observed availability, but an existing runtime cooldown still
requires its normal recovery or manual intervention.

Real inference responses continue to refresh observations. Missing or invalid
weekly utilization/reset data gives an unknown urgency score. Non-finite usage,
future-dated observations, expired weekly windows, and observations at least
15 minutes old also give an unknown score. Reset timestamps more than eight days
ahead are ignored. A valid unexpired rejection can still block an account when
utilization is missing.

For new placements, measured accounts are preferred over unknown accounts in the
highest eligible priority tier. If all are unknown, use round-robin for that new
placement. An existing session does not move just because its observations are
unknown or stale. If background refresh is unavailable, an unused account may
remain undiscovered while measured peers remain usable; no coding request is
sacrificed to discover it. Quota observations and affinity bindings are in memory,
so a restart starts placement afresh. Embedded users of the selector alone must
provide observations themselves; the polling worker belongs to the service.

## Tradeoffs and evaluation

This is a heuristic, not an optimal scheduler or a guarantee that every quota
will be consumed. Requests have different costs, utilization reports can lag,
and concurrent requests may pick the same account before feedback arrives.
Strong affinity deliberately trades some quota utilization for prompt-cache locality.
Requests with no recognized session identity are ranked separately. Separate proxy processes make
independent decisions from their own observations; they do not coordinate.

Test one isolated instance before a production rollout. Compare unused weekly
capacity at resets, five-hour throttling, account selection, latency, and prompt
cache usage against the current strategy under comparable traffic. The existing
management account observations and request logs can show which account was
selected and what quota was last observed. New placement logs include urgency
and whether it was known; affinity logs show reuse. Background refresh logs do
not contain tokens or response bodies. Old observations are not proof of current
available quota.

Automated tests cover the staggered-reset example, reset transitions, stale and
invalid data, cold-start placement, priorities/weights, cooldowns, aliases,
explicit/inferred/subagent affinity, hot reload, concurrent selections, and
quota feedback through selection, normal execution, and streaming. Additional
regressions cover 200-turn warm sessions, background refresh conversion and
failures, stale responses, credential replacement, and cancellation. A deterministic
equal-demand simulation compares unused early-expiring capacity against
round-robin; its result illustrates the policy rather than forecasting real
Claude usage.

```sh
go test ./...
go test -race ./sdk/cliproxy/auth ./sdk/cliproxy ./internal/api/handlers/management
go test -v ./sdk/cliproxy/auth -run TestQuotaDeadlineSimulation
go build -o test-output ./cmd/server
rm test-output
```
