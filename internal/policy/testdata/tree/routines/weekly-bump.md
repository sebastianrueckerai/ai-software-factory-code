The factory's lookup (`upstream-bump`) opened this card's pull request. It moved every pinned outside input to the newest release of its line and kept the places that name a pin in step: `upstream-images.txt`, `upstream-files.txt`, the three Dockerfiles, `go.mod` and `go.sum`, `factory/images.yaml` and the plugin's package files. Its text lists what moved, the lines its rules hold, the newer lines its rules leave, and each pin it could not resolve. Before the checks the factory copied into the store every line the head adds; a line it could not copy is a finding on this card with what its URL served.

You work on this card only because a check of the pull request failed. Fix what the check names, in the smallest change:

- A test or a build that a new version breaks: adapt the code to the new version. Do not move a pin back to make a check pass.
- A finding of govulncheck with the new database: fix the code, or move the module it names with `go get <module>@<fixed version>`.
- A failure of `go test ./internal/reproducible` names two places that disagree on one pin: bring them in step.
- A pin the lookup could not resolve stays as it was. Leave it, unless a check fails on it.
- Your sandbox reaches no registry and no release page, so you cannot look up a version, a digest or a sha256. When a fix needs one, say so in your summary and do not guess.

A line of `upstream-images.txt` may carry a word after its kind: `patch` keeps it in its minor, `hold` never moves it, `with <name>` gives it the version of that image's line, and `hold with <name>` never moves it but is named when that line moves. Change a word only when a check shows the rule is wrong for that line, and say why in your summary.

The infra repository's references move to the new lines after the merge, as their own work; that is not this card's.
