# DeskBox Use Cases

The core design — contracts enforced by the kernel, durable idempotent job store, per-job resource isolation — solves a broader set of problems than just "run tools safely." Here are the scenarios you actually hit in production.

## Scenario: Race conditions from concurrent agents

**Problem.** Multiple agents (or multiple invocations of the same agent) call the same tool at the same time. If the tool isn't idempotent, you get divergent state.

**DeskBox solves it.** Each tool runs in its own sandbox with resource limits (memory, process count). One can't starve the others. More importantly, jobs are durable and queryable — you *see* what ran, when, and in what order. And:

```bash
curl -X POST localhost:8080/tools/commit-findings \
  -d '{"input":{...},"idempotency_key":"analysis-run-20260914-1630"}'
```

If the same analysis runs twice (network hiccup, retry in the agent, whatever), passing the same `idempotency_key` returns the *same job result* — no double-commit. The first agent to submit with that key "wins," and every subsequent caller gets its result.

**Result:** Concurrent tool invocations become safe without special-casing logic in the tool itself.

---

## Scenario: Transient network failures in distributed tool calls

**Problem.** Agent submits a tool request. Network drops before the response lands. Agent retries. You don't know if the tool ran once, twice, or not at all. If it's a mutation (write to a database, file, external API), double-runs are catastrophic.

**DeskBox solves it.** The desk is always-on and persists every job. Submit with `idempotency_key`:

```bash
# First try
curl -X POST localhost:8080/tools/create-file \
  -d '{"input":{"name":"report.md"},"idempotency_key":"user-request-abc"}'
# Network timeout. Agent retries.

# Second try (same key)
curl -X POST localhost:8080/tools/create-file \
  -d '{"input":{"name":"report.md"},"idempotency_key":"user-request-abc"}'
# Returns the SAME job result. File created once.
```

No special protocol, no claim/TTL, no distributed transactions. Just: "here's a key that means 'this work', run it once if you haven't already, tell me the result."

**Result:** Tolerate network flakiness without mutating state multiple times.

---

## Scenario: Exactly-once execution guarantees in a microservice

**Problem.** You have a job queue (Kafka, RabbitMQ, SQS, whatever). Consumer reads an item, calls a tool, publishes the result. Consumer crashes mid-execution. When another consumer picks up the same item, you can't tell if the tool already ran.

**DeskBox solves it.** Use the job id (or any natural key from your queue item) as the `idempotency_key`:

```bash
# Consumer 1 picks up item "process-order-12345"
curl -X POST localhost:8080/tools/process-order \
  -d '{"input":{...},"idempotency_key":"order-12345"}'
# Job runs, consumer dies before publishing result.

# Consumer 2 picks up the same item
curl -X POST localhost:8080/tools/process-order \
  -d '{"input":{...},"idempotency_key":"order-12345"}'
# Returns the result from Consumer 1's run. No double-processing.
```

You can also query the desk directly:

```bash
# Before consuming an item, check if it's already done
curl localhost:8080/jobs | jq '.[] | select(.idempotency_key=="order-12345")'
```

**Result:** Exactly-once semantics even when consumers restart, without a distributed consensus protocol.

---

## Scenario: Rollback and audit in an agentic loop

**Problem.** An agent makes a sequence of tool calls to accomplish a task. Halfway through, something breaks. You need to know exactly what happened (which tools ran, in what order, with what inputs and outputs) to recover or retry.

**DeskBox solves it.** Every job is durable. Query the full history anytime:

```bash
curl localhost:8080/jobs | jq '.[] | {id, tool, status, attempt, result}' > audit-trail.json
```

Or poll in real time:

```bash
curl localhost:8080/jobs/wait-any  # blocks until a job finishes
curl localhost:8080/jobs/{id}      # check specific result
curl -X POST localhost:8080/jobs/{id}/ack  # mark it seen
```

Your agent never has to guess whether a tool ran. And you have the full input/output for every call — audit trail for compliance, replay for debugging.

**Result:** Reproducible agentic workflows with a forensic record of what went wrong.

---

## Scenario: Backpressure and concurrency control

**Problem.** You have an agent that could call 1000 tools in parallel, but your database or external API can only handle 10 concurrent requests. If the tool doesn't throttle itself, you'll overwhelm the downstream.

**DeskBox solves it.** Workers and per-job resource caps. Set `-workers 10`:

```bash
./agent-desk -workers 10  # at most 10 jobs running concurrently
```

Plus per-job limits on memory and process count. A tool that tries to fork bomb itself hits `TasksMax` and the kernel kills the excess processes, not the entire job.

Your agent can fire 1000 requests at the desk as fast as it wants. The desk queues them, runs 10 at a time, and persists every result. No congestion control logic in the agent, no backpressure semantics to implement.

**Result:** Built-in concurrency limiting and resource fairness.

---

## Scenario: A/B testing and canary rollouts

**Problem.** You have two versions of a tool and want to A/B test them. Some agents should call v1, others v2, or a percentage split. You need repeatability so the same input always routes the same way.

**DeskBox doesn't solve this directly**, but it's easy to layer on top:

```bash
# In your agent/orchestrator:
# Hash the idempotency_key to pick a variant deterministically
variant=$(echo -n "$idempotency_key" | sha256sum | cut -c1-1)
if [ $((0x$variant % 2)) -eq 0 ]; then
  tool="my-tool-v1"
else
  tool="my-tool-v2"
fi

curl -X POST "localhost:8080/tools/$tool" \
  -d "{...,"idempotency_key":"$idempotency_key"}"
```

Same key always hashes the same way, so the same user/request always sees the same variant. No state to maintain.

**Result:** Reproducible splitting logic, deterministic per-key routing.

---

## Scenario: Resilience to tool crashes and restarts

**Problem.** A tool has a bug that crashes it mid-execution. The desk restarts. Did the tool run? Is the job still queued?

**DeskBox solves it.** On startup the desk reloads every job left `queued` or `running` and re-enqueues it:

```bash
# Job submits, tool crashes mid-run
# Desk is killed hard (kill -9)
# You restart the desk

# The job is re-enqueued and tried again up to max_retries
# If it fails, the error is durable — you can see it with GET /jobs/{id}
```

No orphaned work, no silent data loss. The desk is *designed* to be killed hard and restarted; jobs survive it.

(Verified with a 500-item batch: kill the desk mid-flight, restart, all items completed. Zero orphans.)

**Result:** Resilient to process restarts and operator mistakes.

---

## Summary

The patterns add up:
- **Idempotency** (same key = same result) + **durability** (every job is persisted) = exactly-once semantics
- **Resource isolation** (per-job caps) + **concurrency limiting** (-workers) = fair sharing
- **Contract enforcement** (input/output validation) + **audit trail** (queryable job history) = reproducible, debuggable workflows

These aren't new ideas individually. DeskBox is just the first tool that puts them all in one place for the specific job of "run tool contracts safely." You don't need a message queue, a state machine, or a distributed transaction protocol to get these guarantees — you need a desk that persists jobs and respects idempotency keys.
