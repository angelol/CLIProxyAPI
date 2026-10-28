# Quota deadline routing

`quota-deadline` is an experimental, opt-in strategy for pools of Claude
subscription accounts whose weekly limits reset at different times. Round-robin
can spend capacity on a later-reset account while earlier capacity expires
unused. This strategy favors the account with the highest required spending rate:

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
5. With session affinity enabled, keep an existing binding only if its urgency
   is at least 80% of the best candidate's. This applies to explicit sessions,
   inferred conversation matches, and inherited parent/subagent bindings. It
   re-evaluates at request boundaries, including within long-lived sessions.

Only shared subscription windows enter the score. `7d_oi`/overage claims are not
treated as a shared weekly budget. Existing model-specific cooldowns remain in
force. The strategy does not independently estimate remaining model-specific
capacity, request cost, or five-hour spending pace. It can exhaust a five-hour
window and then fail over; the purpose is to reduce unused weekly quota.

## Freshness and discovery

No new credentials, polling endpoints, or background requests are needed. The
proxy already captures quota headers from real upstream responses, including
streaming responses. Utilization is a fraction (0–1), and resets are Unix seconds.
Missing or invalid weekly utilization/reset data gives an unknown urgency score.
Non-finite usage, future-dated observations, expired weekly windows, and
observations at least 15 minutes old also give an unknown score. Reset timestamps
more than eight days ahead are ignored. A valid unexpired rejection can still
block an account when utilization is missing.

If some accounts have usable observations, every twentieth selection with
unknown candidates probes one unknown account, rotating those probes separately
from ranked traffic. If all are unknown, selection uses round-robin until
observations arrive. These probes are real user requests and can break affinity.
They still respect eligibility and the selected priority tier. Under low traffic
or a large pool, discovery may take a while. An account that never supplies quota
headers remains on probe traffic when measured peers exist.

Quota observations are in memory; a restart starts discovery again. A banked
usage reset can be recognized through refreshed response headers only after
the account becomes eligible to send a request. This feature does **not** clear
an existing runtime cooldown or poll for manual resets. The previous manual
cooldown intervention may still be necessary.

## Tradeoffs and evaluation

This is a heuristic, not an optimal scheduler or a guarantee that every quota
will be consumed. Requests have different costs, utilization reports can lag,
and concurrent requests may pick the same account before feedback arrives.
Rebalancing can lose prompt-cache locality. Separate proxy processes make
independent decisions from their own observations; they do not coordinate.

Test one isolated instance before a production rollout. Compare unused weekly
capacity at resets, five-hour throttling, account selection, latency, and prompt
cache usage against the current strategy under comparable traffic. The existing
management account observations and request logs can show which account was
selected and what quota headers were last seen. Do not interpret probe traffic
or an old observation as proof of a provider's current available quota.

Automated tests cover the staggered-reset example, reset transitions, stale and
invalid data, cold-start exploration, priorities/weights, cooldowns, aliases,
explicit/inferred/subagent affinity, hot reload, concurrent selections, and
quota feedback through selection, normal execution, and streaming. A deterministic
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
