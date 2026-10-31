# Contributing to GPUBay

Thanks for your interest in GPUBay! Issues and pull requests in English or Chinese are both welcome.
欢迎用中文或英文提 Issue 和 Pull Request。

## Before you start

- **Bugs:** search existing issues first, then open one with the bug report form.
- **Features / larger changes:** open an issue (or a Discussion) to agree on the design before writing code.
  Design docs for existing features live in [`docs/superpowers/specs/`](docs/superpowers/specs/).
- **Security issues:** do not open a public issue; see [SECURITY.md](SECURITY.md).

## Development setup

See [Quick start](README.md#quick-start-local-development). In short:

```bash
make web-install && make web
make test          # must pass before you open a PR
make e2e           # run when you touch the console / admin UI
```

## Pull requests

1. Fork the repo and create a branch from `main` (`feat/...`, `fix/...`, `docs/...`).
2. Keep each PR focused on one change. Add or update tests for behaviour changes.
3. Run `make test` (and `make e2e` for UI changes). Go code must pass `go vet` and be `gofmt`-formatted.
4. Database changes go in a new migration under `migrations/` with the next free number; never edit a released migration.
5. Write a clear PR description: what changed, why, and how you tested it.

Commit messages follow a short conventional style, e.g. `feat(rental): ...`, `fix(web): ...`, `docs: ...`.

## License

By contributing, you agree that your contributions are licensed under the [Apache License 2.0](LICENSE).
