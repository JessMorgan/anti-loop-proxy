#!/usr/bin/env bash
# Runs the exact same checks as .github/workflows/tests.yml so a green local
# run is equivalent to a green GitHub Actions run. The jobs here mirror the
# workflow's jobs in order and use the same commands and tool versions.
#
# Usage:
#   bash scripts/local-ci.sh
#
# Install prerequisites (once):
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1
#   go install mvdan.cc/gofumpt@latest
#   go install honnef.co/go/tools/cmd/staticcheck@latest
#   go install golang.org/x/vuln/cmd/govulncheck@latest

set -euo pipefail

GOPATH_BIN="$(go env GOPATH)/bin"
GO_VERSION_OK='1.24.x'
# CI installs tools with setup-go's check-latest 1.24 toolchain. With
# GOTOOLCHAIN=auto, `go install` can silently select a lower toolchain; pin it
# so locally-built tools are the same (or newer) than the project requires.
CURRENT_MAJOR_MINOR="$(go version | sed -nE 's/.*go([0-9]+\.[0-9]+).*/\1/p')"
TOOLCHAIN_GO="go${CURRENT_MAJOR_MINOR}.0"

PASSED=0
FAILED=0

pass() { echo -e "\n\033[1;32mPASS:\033[0m $1"; PASSED=$((PASSED + 1)); }
fail() { echo -e "\n\033[1;31mFAIL:\033[0m $1"; FAILED=$((FAILED + 1)); }

# ---------------------------------------------------------------------------
# Determine active Go version family (matches workflow GO_VERSION: '1.24.x').
# ---------------------------------------------------------------------------
go_version="$(go version | sed -nE 's/.*go([0-9]+\.[0-9]+).*/\1/p')"
expected="${GO_VERSION_OK%.*}"
if [ "$go_version" != "$expected" ]; then
  echo "\033[1;33mWARN:\033[0m active Go is $go_version; CI runs Go $GO_VERSION_OK (check-latest: true)."
fi

# ---------------------------------------------------------------------------
# Job: source-tracking
# ---------------------------------------------------------------------------
echo -e "\n──────────────────────────────────────────────────────────"
echo "Job: Source tracking"
if bash scripts/check-ignored-sources.sh; then
  pass "source tracking"
else
  fail "source tracking"
fi

# ---------------------------------------------------------------------------
# Job: format
# ---------------------------------------------------------------------------
echo -e "\n──────────────────────────────────────────────────────────"
echo "Job: Formatting"
# Only project (tracked) Go files are formatted-checked, mirroring what CI
# validates. Running over "." would also recurse into untracked scratch dirs,
# adding noise that can mask real violations.
TRACKED_GO="$(git ls-files '*.go')"
fmt_ok=1
if [ -n "$TRACKED_GO" ] && [ -n "$(gofmt -l $TRACKED_GO)" ]; then
  echo "gofmt reports unformatted files:"; gofmt -l $TRACKED_GO
  fmt_ok=0
fi
GF="$(command -v gofumpt || echo "$GOPATH_BIN/gofumpt")"
if [ ! -x "$GF" ]; then
  echo "gofumpt not found; installing mvdan.cc/gofumpt@latest"
  GOTOOLCHAIN=${TOOLCHAIN_GO} go install mvdan.cc/gofumpt@latest
  GF="$GOPATH_BIN/gofumpt"
fi
# -modpath keeps local imports (anti-loop-proxy/...) out of the stdlib import
# group; the module name has no slash.
if [ -n "$TRACKED_GO" ] && [ -n "$("$GF" -l -modpath=anti-loop-proxy $TRACKED_GO)" ]; then
  echo "gofumpt reports unformatted files:"; "$GF" -l -modpath=anti-loop-proxy $TRACKED_GO
  fmt_ok=0
fi
# CI checks "go mod tidy && git diff --exit-code" on a clean tree; locally the
# tree may have uncommitted changes, so compare go.mod/go.sum before and after
# tidy instead.
tidy_dir="$(mktemp -d)"
trap 'rm -rf "$tidy_dir"' EXIT
cp go.mod go.sum "$tidy_dir/"
go mod tidy >/dev/null
if diff -q "$tidy_dir/go.mod" go.mod >/dev/null && diff -q "$tidy_dir/go.sum" go.sum >/dev/null; then
  :
else
  echo "go.mod/go.sum not tidy:"
  diff "$tidy_dir/go.mod" go.mod
  diff "$tidy_dir/go.sum" go.sum
  fmt_ok=0
fi
if [ "$fmt_ok" = "1" ]; then pass "formatting"; else fail "formatting"; fi

# ---------------------------------------------------------------------------
# Job: lint
# ---------------------------------------------------------------------------
echo -e "\n──────────────────────────────────────────────────────────"
echo "Job: Linting (golangci-lint v2.13.1)"
GL="$(command -v golangci-lint || echo "$GOPATH_BIN/golangci-lint")"
if [ ! -x "$GL" ]; then
  echo "golangci-lint not found; installing v2.13.1"
  GOTOOLCHAIN=${TOOLCHAIN_GO} go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1
  GL="$GOPATH_BIN/golangci-lint"
fi
installed_ver="$("$GL" version 2>/dev/null | sed -nE 's/golangci-lint has version ([0-9.]+).*/\1/p')"
if [ "$installed_ver" != "2.13.1" ]; then
  echo "\033[1;33mWARN:\033[0m local golangci-lint is $installed_ver; CI pins v2.13.1."
fi
if "$GL" run --timeout=5m; then
  pass "golangci-lint"
else
  fail "golangci-lint"
fi

# ---------------------------------------------------------------------------
# Job: test (coverage threshold + race)
# ---------------------------------------------------------------------------
echo -e "\n──────────────────────────────────────────────────────────"
echo "Job: Unit tests"
if go test -count=1 -covermode=atomic -coverprofile=coverage.out ./...; then
  COVERAGE="$(go tool cover -func=coverage.out | tail -1 | awk '{print $3}' | tr -d '%')"
  if [ "$(echo "$COVERAGE < 70" | bc -l)" = "1" ]; then
    echo "FAIL: coverage ${COVERAGE}% below 70% threshold"
    fail "unit tests (coverage ${COVERAGE}%)"
  else
    pass "unit tests (coverage ${COVERAGE}%)"
  fi
else
  fail "unit tests"
fi
rm -f coverage.out

echo "Job: Race detector"
if go test -count=1 -race ./...; then
  pass "race detector"
else
  fail "race detector"
fi

# ---------------------------------------------------------------------------
# Job: static-analysis
# ---------------------------------------------------------------------------
echo -e "\n──────────────────────────────────────────────────────────"
echo "Job: Static analysis"
sa_ok=1
if ! go vet ./...; then sa_ok=0; fi
SC="$(command -v staticcheck || echo "$GOPATH_BIN/staticcheck")"
if [ ! -x "$SC" ]; then
  echo "staticcheck not found; installing honnef.co/go/tools/cmd/staticcheck@latest"
  GOTOOLCHAIN=${TOOLCHAIN_GO} go install honnef.co/go/tools/cmd/staticcheck@latest
  SC="$GOPATH_BIN/staticcheck"
fi
if ! "$SC" ./...; then sa_ok=0; fi
if [ "$sa_ok" = "1" ]; then pass "go vet + staticcheck"; else fail "go vet + staticcheck"; fi

echo "Job: Go vulnerability check"
VN="$(command -v govulncheck || echo "$GOPATH_BIN/govulncheck")"
if [ ! -x "$VN" ] || "$VN" -version 2>/dev/null | grep -q "Scanner: govulncheck@v0"; then
  echo "govulncheck missing or old; installing golang.org/x/vuln/cmd/govulncheck@latest"
  GOTOOLCHAIN=${TOOLCHAIN_GO} go install golang.org/x/vuln/cmd/govulncheck@latest
  VN="$GOPATH_BIN/govulncheck"
fi
if "$VN" ./...; then
  pass "govulncheck"
else
  fail "govulncheck"
fi

# ---------------------------------------------------------------------------
# Job: build (strict compile)
# ---------------------------------------------------------------------------
echo -e "\n──────────────────────────────────────────────────────────"
echo "Job: Strict build"
if env CGO_ENABLED=0 GOFLAGS='-trimpath' go build -buildvcs=true -ldflags='-s -w' ./... && go list -m all >/dev/null; then
  pass "strict build"
else
  fail "strict build"
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo -e "\n──────────────────────────────────────────────────────────"
echo "Local CI: ${PASSED} passed, ${FAILED} failed."
[ "$FAILED" = "0" ]
