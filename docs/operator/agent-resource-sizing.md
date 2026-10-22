# Agent CPU and memory sizing

Kyber accepts CPU from 100m in 50m steps and memory from 128Mi in 128Mi steps. These are main container requests and limits. The create wizard and Agent detail dialog request machine-specific choices from the API; the API validates submitted values again because available capacity can change.

Before an increase, Kyber checks the Machine's declared and observed capacity, existing Agent allocations, a conservative sidecar allowance, live node pod requests when a node is known, and readable namespace LimitRange and ResourceQuota constraints. It serializes Kyber API allocations per Machine. Kubernetes admission and scheduling still make the final decision if an unrelated workload or provider policy changes in the meantime. A rejected preflight leaves the Agent allocation and desired phase unchanged.

A Running Agent restarts with the new CPU and memory on the same persistent claims. A Stopped Agent remains stopped and applies its new allocation on the next explicit Start. Failed and MemoryExhausted edits can make one recovery attempt; NeedsAuth and DiskExhausted retain their safety gates. Transient lifecycle phases reject edits until they settle. A decrease can be stored while the Machine is offline, but is only applied when a replacement pod actually starts.

The restart preserves the existing PVCs and files. Node-bound local storage does not provide recovery from a lost node; resource editing does not change that storage behavior.

`GET /api/v1/machines/{name}/agent-resource-options` serves creation choices. `GET /api/v1/agents/{name}/resource-options` includes an existing Agent's current allocation. `POST /api/v1/agents/{name}/set-resources` accepts optional `cpu` and `memory` quantities; `PATCH /api/v1/agents/{name}` uses the same validation for resource changes. A successful response means the change was accepted, not that the replacement pod is already running. Check Agent phase and the new pod's requests and limits before treating the resize as complete.
