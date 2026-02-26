# Contributing to Koshi

Thank you for your interest in contributing to Koshi.

## Getting Started

### Prerequisites

- Go 1.25+
- Make
- Helm 3 (for chart validation)

### Local Development

```bash
# Build
make build

# Run tests with race detector
make test-race

# Lint
make lint

# GenOps spec compliance check
make check-genops-spec

# Helm chart validation
helm lint deploy/helm/koshi
```

### Running Locally

```bash
KOSHI_CONFIG_PATH=examples/config.yaml bin/koshi
```

## Submitting Changes

1. Fork the repository and create a feature branch from `main`.
2. Make your changes. Ensure all tests pass (`make test-race`).
3. Run `make lint` and fix any issues.
4. Open a pull request against `main`.

### PR Expectations

- Keep PRs focused — one logical change per PR.
- Include tests for new functionality.
- Maintain the existing correctness invariants (see `docs/design/koshi-v1-deterministic-accounting-invariants.md`).
- `make test-race` must pass with zero failures.

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`).
- Use structured logging via `slog`.
- No silent error swallowing — all errors must be logged or returned.

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).
