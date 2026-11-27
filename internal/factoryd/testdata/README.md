# Recorded histories

`devserver-*.json` are runs recorded on a local dev server under stub activities; `TestWorkflowReplaysDevServerHistories` replays each one against `TaskWorkflow`, and the tests of single changes read the ones they name. Their identity fields hold a placeholder instead of the recording machine, such as `recorder@devserver`, `1@devserver@` or `1@host@`. The corpus holds no live recording beyond `unmarked-*.json`: a live run exported from Temporal that holds no marker of the cases change, which the one test that pins its refusal with the version error reads. The replay test also reads `history-*.json` when one exists, a live run exported after a roll with every marker; none is kept now.

Record a run again:

1. Use the Temporal CLI 1.10.0 (server 1.32.2), started by `testsuite.StartDevServer` with `ExistingPath` set to the binary.
2. In a throwaway test file, never committed: register `TaskWorkflow` (with `testWorkflowConfig`) on a worker for task queue `factory`, and register the activity stubs of `newWorkflowHarness` (the map in `workflow_test.go`) with the ones the run needs replaced, for example `TaskConfig` returning a maintenance task with the config's stages, `OpenPullRequest` returning a pull request number and `RunSession` returning a pushed result.
3. Start `TaskWorkflow` with workflow id `task-t1` and input `"t1"`, send the signal that ends the run (for example `SignalIssueClosed` with `TaskSignal{Author: "sebastian"}`) and wait for its result.
4. Read the history with `GetWorkflowHistory`, marshal `historypb.History{Events: ...}` with `protojson`, and write it to `testdata/devserver-<name>.json`.
5. Replace the machine's identity (`<pid>@<host>@`, the sticky queue name) with `recorder@devserver`. Run `go test -run ReplaysDevServerHistories ./internal/factoryd/`.
