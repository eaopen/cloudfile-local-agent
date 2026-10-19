# CloudFile Local Agent

> [!CAUTION]
> **Rust Agent2 development has MOVED — `agent-rust` is frozen (2026-10-20).**
> **Do not make Rust Agent2 code changes or open development PRs against this branch.**
> Continue in **[eaopen/cloudfile-local-agent2](https://github.com/eaopen/cloudfile-local-agent2)**, using its `dev` branch and feature PRs.
> The old `agent22/` directory is **historical reference only**, not the active source of truth.
> The outstanding migration review is [cloudfile-local-agent2 PR #7](https://github.com/eaopen/cloudfile-local-agent2/pull/7); check its merge status instead of assuming those changes are already in `dev`.
> See [migration rules](AGENTS.md) before starting any automated changes.

[English](README.md) | [简体中文](README.zh-CN.md)

[Product home](https://github.com/eaopen/cloudfile) · [Capabilities and roadmap](https://github.com/eaopen/cloudfile/blob/main/docs/en/README.md)

A local application companion that connects CloudFile to installed professional software through controlled sessions and Native Messaging. File access, submission and version publication remain subject to server authorization. Check support for each platform and release package separately.

## Technical Documentation

These references are currently maintained in Chinese; English translations are not yet available.

| Reference | Contents |
| --- | --- |
| [Technical reference](docs/technical-reference.md) | Builds, configuration, application selection, security boundaries, Host routing, versions and development status |
| [macOS](docs/macos.md) | Installation, Keychain, updates and platform boundaries |
| [Device upload contract](docs/device-upload-contract.md) | Device authorization, submission receipts, recovery and unavailable operations |

## Verify From Source

```bash
go test ./...
go build -trimpath -ldflags="-s -w" -o dist/cloudfile-local-agent ./cmd/cloudfile-local-agent
```

These commands verify and build the public Go Core; they do not produce every platform's complete release package. The full Windows .NET + Go package still depends on private build assets. An independently reproducible public build is a migration goal; the current public repository cannot reproduce that complete package on its own. Use this repository's platform guide and scripts for macOS.

A local save becomes a server version only after successful submission. Determine completion from package capabilities and server receipts. Editing paths without full acceptance are not declared supported releases.

## License And Maintenance

The general-purpose Agent follows a complete open-source product direction, with customer-specific business adapters maintained separately. CloudFile-owned code uses [Apache 2.0](LICENSE); dependencies retain their licenses, listed in [NOTICE](NOTICE). Product plans live in the product home; protocols and support boundaries follow this repository's code.
