# Release Provenance

This document defines the ClaraCore Server local release builder identity and build type used by `BUILD_PROVENANCE.intoto.json`.

## Build type v1

Type URI: `https://github.com/xiaomao361/claracore-server/blob/main/docs/09-release-provenance.md#build-type-v1`

The external interface is `scripts/package-release.sh <version> <amd64|arm64> <output-dir> [ed25519-private-key]`. It cross-compiles `./cmd/server` for Linux twice with Go 1.26.6, `CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false`, a linked release version and the SHA-256 of `SOURCE_MANIFEST.sha256`. Each compilation uses a separate newly created `GOCACHE`, with `GOENV=off`, an empty `GOFLAGS`, `GOWORK=off` and no external `GOCACHEPROG`. The builder requires both binaries to be byte-for-byte identical before it runs the pinned vulnerability, license, SBOM and Prometheus rule gates, assembles the documented deployment files and operational scripts, writes an inner manifest, creates a deterministic PAX tar/gzip archive, and optionally signs the outer checksum with Ed25519.

`externalParameters` records the caller-controlled version and target. `internalParameters` records the fixed toolchain, target-independent build settings and linked source digest. `resolvedDependencies` binds the source and packaging-input manifests. The builder snapshots both sets before compiling, re-reads all application source after the long-running gates, and recomputes the repository-owned packaging input manifest during provenance generation; any drift aborts the release instead of attesting to a mixed invocation. `byproducts` binds the build information, bounded reproducibility result and each supply-chain review receipt that exists before provenance generation.

The statement intentionally omits invocation identifiers and timestamps so identical release inputs remain byte-for-byte deterministic. The double build proves a narrower property on the release host; it does not claim that dependencies were fetched hermetically or that independent hosts will produce identical binaries.

## Reproducibility receipt v1

Schema URI: `https://github.com/xiaomao361/claracore-server/blob/main/docs/09-release-provenance.md#reproducibility-receipt-v1`

`BUILD_REPRODUCIBILITY.json` records the fixed version, source digest, Linux target, Go 1.26.6 toolchain and the required settings `CGO_ENABLED=0`, `trimpath=true`, `buildVCS=false`. Both `primary` and `rebuild` descriptors must contain the same SHA-256 as the packaged `claracore-server`. The fixed scope is `same-host-separate-empty-go-build-cache`.

The second binary is deliberately not packaged. Generation inspects both binaries' Go build information and compares their complete bytes; offline verification later rechecks the receipt schema, identity, settings and both recorded digests against the authenticated packaged binary. The receipt has no timestamp, invocation ID, cache path or host path, so it remains deterministic.

This is a same-source, same-host, same-toolchain repeat-build gate with isolated Go build caches. The module download cache, toolchain distribution, host kernel/filesystem and maintainer environment remain inside the local builder trust boundary. The receipt is therefore evidence against incidental compiler/cache/time/path drift, not a hermetic-build, independent-rebuilder or cross-host reproducibility claim.

## Local builder v1

Builder ID: `https://github.com/xiaomao361/claracore-server/blob/main/docs/09-release-provenance.md#local-builder-v1`

This ID describes the repository-owned local shell workflow executed on a maintainer-controlled host. The trusted computing base includes that host, its OS, Go toolchain and module cache, network and live vulnerability database, the invoked repository scripts and the maintainer operating them. Separate empty Go build caches reduce one source of accidental drift but do not isolate that host. It is not a hosted or isolated build service.

The emitted document is an in-toto Statement v1 with the SLSA provenance v1 predicate. It is included in `MANIFEST.sha256`; the deterministic archive is bound by the outer checksum and, for production releases, the maintainer's Ed25519 signature. This chained authentication is not a DSSE envelope and this project does not claim a SLSA Build Level. A higher-level claim would require separately evidenced hosted or hardened build infrastructure and its own threat model.
