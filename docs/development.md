# Development

## Building and checking

```bash
task build-local      # dashboard (Bun) + binary for this machine, in bin/
task check            # the CI gate: gofmt, vet, golangci-lint, tests under -race
task vuln             # govulncheck
task docker           # the image, from source
task release:check    # goreleaser check + a snapshot release in dist/, nothing published
```

`Taskfile.yml` pins `GOTOOLCHAIN` to go.mod's go line, the Go CI, the
Dockerfile and releases build with. Keep go.mod on the latest patch release.

## CI

`.github/workflows/ci.yml` runs on every pull request and every push to
main:

- **Go**: the dashboard build (the tests check the embedded bundle is a real
  one), gofmt, `go mod tidy` with no diff, `go mod verify`, vet,
  golangci-lint, the tests and govulncheck;
- **race**: the tests under the race detector;
- **cross**: a build for each release platform besides linux/amd64;
- **docker**: the image builds and runs `--version`, and `goreleaser check`
  accepts `.goreleaser.yml`.

Actions are pinned to commit hashes, with the version in a trailing comment:
a tag can be moved to other code. `build/ci_test.go` fails on an unpinned
one. Dependabot (`.github/dependabot.yml`) proposes the pin updates weekly,
together with Go modules, the dashboard's packages and the base images.
Tools (Task, golangci-lint, govulncheck, Bun) are installed at the versions
at the top of the workflow.

## Releasing

1. Under `## [Unreleased]` in `CHANGELOG.md`, list what the release
   changes; rename the heading to the version and date
   (`## [0.21.0] — 2026-11-10`) and start a new empty `[Unreleased]` above
   it. Note anything an operator must do in an **Upgrade** paragraph (the
   server goes first: workers follow it by themselves).
2. Commit that and push it to main.
3. `task release -- v0.21.0`. It refuses a version with no changelog
   section, uncommitted changes, or a `HEAD` that is not `origin/main`; it
   waits for CI's run on that commit (`scripts/wait-for-ci.sh`, which needs
   `gh`) and tags only if it passed. CI has run the whole suite there, race
   detector included, so the release does not run it again.

The tag starts `.github/workflows/release.yml`. Its first job builds the
dashboard (`web/dist`) and hands it to the second, so `bun install`'s
third-party code never runs where the signing secrets are. The second runs
GoReleaser (`.goreleaser.yml`), which produces:

- archives for Linux, macOS and Windows on amd64 and arm64, each with the
  licence, README, changelog and `skopos-config.example.toml`, plus the bare
  binaries `skopos_<os>_<arch>` (what provisioning scripts download, and
  what workers self-upgrade from), `checksums.txt` and an SPDX SBOM per
  archive;
- keyless cosign signatures of the checksums and SBOMs, and a GitHub build
  provenance attestation. The workflow fetches Sigstore's trust root and
  signing config once, before GoReleaser, and every signing uses those
  files: cosign otherwise rewrites its TUF cache on each run, and signings
  running at once can collide there and fail the release;
- a multi-arch image (`linux/amd64`, `linux/arm64`) at
  `ghcr.io/martinsuchenak/skopos`, tagged with the version, the minor
  version and `latest` (not for pre-releases), built from the release
  binaries by `Dockerfile.release` and signed with cosign;
- the macOS binaries signed with a Developer ID certificate and notarized by
  Apple, when the `MACOS_*` secrets are set (see below); unsigned otherwise;
- a Homebrew cask in `martinsuchenak/homebrew-tap`, which needs the
  `TAP_GITHUB_TOKEN` secret (a token that can push to that repository);
- the GitHub release, with its changelog section as the notes.

Verify a download with `cosign verify-blob --bundle checksums.txt.sigstore.json
checksums.txt` (with `--certificate-identity-regexp
'https://github.com/martinsuchenak/skopos/'` and
`--certificate-oidc-issuer https://token.actions.githubusercontent.com`), then
`sha256sum --check --ignore-missing checksums.txt`; or
`gh attestation verify <file> --repo martinsuchenak/skopos`.

### macOS signing and notarization

The release signs the macOS binaries with a Developer ID certificate and
notarizes them with Apple, on the Linux runner (GoReleaser's built-in
signer). It needs an Apple Developer Program membership and these
repository secrets; while `MACOS_SIGN_P12` is unset, the step is skipped and
the binaries ship unsigned (the Homebrew cask still lifts the quarantine on
install).

| Secret | What it is |
|--------|-----------|
| `MACOS_SIGN_P12` | The Developer ID Application certificate and its private key, as a base64 `.p12` |
| `MACOS_SIGN_PASSWORD` | The password the `.p12` was exported with |
| `MACOS_NOTARY_ISSUER_ID` | The App Store Connect API issuer ID (a UUID) |
| `MACOS_NOTARY_KEY_ID` | The API key's ID |
| `MACOS_NOTARY_KEY` | The API key (`AuthKey_<id>.p8`), base64 |

One certificate and key serve every project of the same Apple team, so the
ones another project already uses work here; GitHub secrets are per
repository, so set them again on this one. To create them:

1. **Certificate.** In [Certificates, Identifiers & Profiles](https://developer.apple.com/account/resources/certificates/list),
   add a **Developer ID Application** certificate (only the account holder
   can). Install it, then in Keychain Access export the certificate together
   with its private key as a `.p12`, with a password.
2. **API key.** In [App Store Connect](https://appstoreconnect.apple.com/access/integrations/api),
   under Users and Access → Integrations → App Store Connect API, create a
   team key with the Developer role. Note the issuer ID and the key ID, and
   download the `.p8` (it can only be downloaded once).
3. **Secrets.** From the files:

   ```bash
   base64 -i DeveloperID.p12 | gh secret set MACOS_SIGN_P12 -R martinsuchenak/skopos
   gh secret set MACOS_SIGN_PASSWORD -R martinsuchenak/skopos
   gh secret set MACOS_NOTARY_ISSUER_ID -R martinsuchenak/skopos
   gh secret set MACOS_NOTARY_KEY_ID -R martinsuchenak/skopos
   base64 -i AuthKey_XXXXXXXXXX.p8 | gh secret set MACOS_NOTARY_KEY -R martinsuchenak/skopos
   ```

   Then delete the local `.p12` and `.p8` copies you no longer need.

The binaries are notarized but not stapled (a bare binary can't carry a
ticket), so the first launch of a downloaded copy checks with Apple online.
To check a downloaded binary:

```bash
codesign --verify --verbose=2 skopos   # "valid on disk" and "satisfies its Designated Requirement"
spctl --assess --type open --context context:primary-signature --verbose=2 skopos   # "accepted", "source=Notarized Developer ID"
```

A worker that upgrades itself replaces its binary with the signed one from
the same release; nothing changes for it.
