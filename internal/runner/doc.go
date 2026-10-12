// Package runner executes jobs on behalf of the engine. It registers with a
// runner token and advertises its adapters, leases jobs over a long-poll,
// materializes its own workspace by fetching the task's repo URL into a
// per-task worktree, runs the agent CLI headless through an adapter, runs the
// task's check itself once the worker reports done, and pushes the result
// branch to the remote. It heartbeats progress and completes with a fencing
// token, so lease expiry plus fencing make crashes safe.
package runner
