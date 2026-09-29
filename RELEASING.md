# Releasing

A release is a git tag. Nothing in the tree carries a version number, so there is nothing to bump.

## Cut a release

1. Make sure `main` is green and has everything the release should include.
2. Tag it and push the tag:

   ```sh
   git checkout main
   git pull
   git tag v0.2.0
   git push origin v0.2.0
   ```

Use semantic versions with a leading `v`, like `v0.2.0` or `v1.0.0`. The release workflow only fires for tags that start with `v`.

## What the tag triggers

The Go module needs nothing else. As soon as the tag exists, `go get github.com/outis-auth/outis-go@v0.2.0` works through the module proxy.

Pushing the tag also runs `.github/workflows/release.yml`, which:

1. Runs `go vet` and the tests once more.
2. Builds the `outis` CLI for linux/amd64, linux/arm64, darwin/amd64, darwin/arm64 and windows/amd64, statically (`CGO_ENABLED=0`) with `-trimpath -ldflags="-s -w"`.
3. Packages each build as `outis_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows), where `<version>` is the tag without its `v`.
4. Writes a `SHA256SUMS` file covering every archive.
5. Creates a GitHub release for the tag with generated release notes and attaches the archives and the checksums.

If the tests fail, no release is created. Fix the problem on `main`, then tag the next version. Don't move or reuse a tag that has been pushed; the module proxy has already seen it.

## How people install it

The library:

```sh
go get github.com/outis-auth/outis-go@latest
```

The CLI, built from source:

```sh
go install github.com/outis-auth/outis-go/cmd/outis@latest
```

Or download an archive from the [releases page](https://github.com/outis-auth/outis-go/releases), check it against `SHA256SUMS`, and put the `outis` binary on your `PATH`.
