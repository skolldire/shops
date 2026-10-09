## What this PR does

_One or two sentences: the problem it solves or what it adds._

Closes #

## Phase and scope

- [ ] Phase 1: platform and runnable skeleton
- [ ] Phase 2: catalog
- [ ] Phase 3: CSV import
- [ ] Phase 4: cart, checkout and payment
- [ ] Phase 5: documentation

## Type of change

- [ ] :sparkles: new functionality
- [ ] :bug: bug fix
- [ ] :recycle: refactor with no behavior change
- [ ] :lock: security
- [ ] :white_check_mark: tests only
- [ ] :memo: documentation only
- [ ] :wrench: / :whale: / :hammer: configuration, containers or tooling

## Key changes

_List the packages or files that matter and what changed in each. One commit per cohesive unit: repository, use case and handler go in separate commits._

-

## Breaking changes

_Public API, configuration keys, HTTP contracts or database schema. Write "None" if not applicable._

## How to test

```sh
make lint
make test
make test-integration
make up
curl -i localhost:8080/health/ready
```

## Checklist

- [ ] `make lint` passes: golangci-lint, no comments in code or config, `go mod tidy` clean
- [ ] `make test` and `make test-integration` pass
- [ ] New behavior has tests, including failure paths
- [ ] Module boundaries respected: modules talk only through their public `api.go`, `internal/platform` knows no business code, no cross-module foreign keys
- [ ] No secrets, credentials or configuration values in code, logs, errors or HTTP responses
- [ ] Schema changes in `db/init` are reflected in integration tests
- [ ] README or `docs/` updated when behavior, configuration or endpoints changed
- [ ] CodeRabbit findings addressed or answered
