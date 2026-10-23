# Local development and installation

This fork uses `dev` for ongoing development and local installations. `main`
is the upstream reference. The older `codex/*` feature branches are historical
work; their verified changes belong on `dev` before they are installed.

## Source identity

1. Continue work on `dev`, or integrate an isolated change into it.
2. Run the checks required by [code standards](code-standards.md). Record
   their results in the commit body. Documentation-only changes can reuse
   existing code validation when the compiled source and assets are identical;
   state that scope explicitly.
3. Commit and publish `dev`. Verify that local `HEAD` equals `origin/dev` and
   that the checkout is clean before building. Do not build first and assign
   a commit to the package afterwards.
4. Set the package version to `dev.<12-character commit SHA>`. Build with
   `-buildvcs=true` and check that `go version -m` reports that exact revision
   with `vcs.modified=false`.

The `dev` Git branch is separate from `make dev`, which starts a development
server. An installed desktop package uses the `production` build tag.

## Build and install

The [Makefile](../Makefile) defines the production build and macOS bundle.
Pass the commit-based version through `VERSION`. Keep Go's module and build
caches when using a temporary HOME. For macOS, build for the deployment
target in [Info.plist](../build/darwin/Info.plist), sign the complete bundle,
and verify it with `codesign --verify --deep --strict` before installation.

Keep a rollback copy of the installed executable, Info.plist and bundle
signature. Replace them together, restart the app and gateway, then verify
the running version and the installed executable's hash. Resolve the CLI
symlink and the LaunchAgent's command to confirm they use that executable.
Verify the affected runtime behavior after a functional change.

Write the local installation receipt to
`~/.local/state/magpie/installation.json`. Include the repository, `dev`
branch, full commit and tree SHAs, version, build tags, toolchain, app path,
signed executable SHA-256, validation evidence and verification time. Keep
machine-specific receipts and rollback artifacts outside Git. Provider
plugins and runtime configuration are recorded separately from the source
commit; the commit alone does not identify them.
