# Recorded histories

`history-*.json` are live runs exported from Temporal. `devserver-*.json` are runs recorded on a local dev server under stub activities. Their identity fields hold the placeholder `recorder@devserver`.

Record a run again:

1. Use the Temporal CLI 1.10.0 (server 1.32.2), started by `testsuite.StartDevServer` with `ExistingPath` set to the binary.
2. In a throwaway test file, never committed: register `TaskWorkflow` (with `testWorkflowConfig`) on a worker for task queue `factory`, and register the activity stubs of `newWorkflowHarness` (the map in `workflow_test.go`) with the ones the run needs replaced, for example `TaskConfig` returning a maintenance task with the config's stages, `OpenPullRequest` returning a pull request number and `RunSession` returning a pushed result.
3. Start `TaskWorkflow` with workflow id `task-t1` and input `"t1"`, send the signal that ends the run (for example `SignalIssueClosed` with `TaskSignal{Author: "sebastian"}`) and wait for its result.
4. Read the history with `GetWorkflowHistory`, marshal `historypb.History{Events: ...}` with `protojson`, and write it to `testdata/devserver-<name>.json`.
5. Replace the machine's identity (`<pid>@<host>@`, the sticky queue name) with `recorder@devserver`. Run `go test -run ReplaysDevServerHistories ./internal/factoryd/`.
