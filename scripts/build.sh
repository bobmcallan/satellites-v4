#!/usr/bin/env bash
# Build and maintenance helper for satellites-v4. Each subcommand is a thin
# wrapper over the Go toolchain. The non-obvious piece is `stamp_ldflags`,
# which parses the requested section of .version for the semver and combines
# it with a build-time UTC timestamp and the short git commit — the v3
# reference pattern (ledger artifact ldg_4754ee2e) carried forward.
set -euo pipefail

cd "$(dirname "$0")/.."

module_path() {
  go list -m
}

# read_kv <section> <key> — extracts `key = value` from inside the named
# `[section]` block of .version. Section-scoped: a subsequent section header
# terminates scope; keys outside the requested section are never read.
read_kv() {
  local section="$1" key="$2"
  awk -F '=' -v section="$section" -v key="$key" '
    BEGIN { in_section = 0 }
    /^[[:space:]]*\[.*\][[:space:]]*$/ {
      header = $0
      sub(/^[[:space:]]*\[/, "", header)
      sub(/\][[:space:]]*$/, "", header)
      in_section = (header == section)
      next
    }
    in_section && $1 ~ "^[[:space:]]*"key"[[:space:]]*$" {
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", $2)
      print $2
      exit
    }
  ' .version
}

build_timestamp() {
  date -u +"%Y-%m-%d-%H-%M-%S"
}

git_commit() {
  git rev-parse --short HEAD 2>/dev/null || echo "unknown"
}

stamp_ldflags() {
  local section="$1"
  local version build commit mod
  version="$(read_kv "$section" version)"
  [ -z "$version" ] && version="dev"
  build="$(build_timestamp)"
  commit="$(git_commit)"
  mod="$(module_path)"
  printf -- '-X %s/internal/config.Version=%s -X %s/internal/config.Build=%s -X %s/internal/config.GitCommit=%s' \
    "$mod" "$version" "$mod" "$build" "$mod" "$commit"
}

cmd_server() {
  go build -ldflags "$(stamp_ldflags satellites)" -o satellites-server ./cmd/satellites-server
}

cmd_agent() {
  go build -trimpath -ldflags "$(stamp_ldflags satellites-agent)" -o satellites-agent ./cmd/satellites-agent
}

cmd_client() {
  go build -ldflags "$(stamp_ldflags satellites)" -o satellites-client ./cmd/satellites-client
}

cmd_build() {
  cmd_server
  cmd_agent
  cmd_client
}

cmd_fmt() {
  gofmt -s -w .
}

cmd_vet() {
  go vet ./...
}

cmd_lint() {
  if ! command -v golangci-lint >/dev/null 2>&1; then
    echo "golangci-lint not installed; skipping" >&2
    return 0
  fi
  golangci-lint run
}

cmd_test() {
  go test ./...
}

cmd_clean() {
  rm -f satellites satellites-server satellites-agent satellites-client
}

cmd_docker() {
  local version build commit
  version="$(read_kv satellites version)"
  [ -z "$version" ] && version="dev"
  build="$(build_timestamp)"
  commit="$(git_commit)"
  docker build \
    -f docker/Dockerfile \
    -t "satellites:${version}" \
    -t "satellites:local" \
    --build-arg "VERSION=${version}" \
    --build-arg "BUILD=${build}" \
    --build-arg "GIT_COMMIT=${commit}" \
    .
}

usage() {
  cat >&2 <<'EOF'
usage: script/build.sh <command>

Commands:
  build    Build satellites-server, satellites-agent, satellites-client
           (default; each binary stamped from its .version section)
  server   Build satellites-server only (reads [satellites])
  agent    Build satellites-agent only (reads [satellites-agent])
  client   Build satellites-client only (reads [satellites])
  fmt      Run gofmt -s -w .
  vet      Run go vet ./...
  lint     Run golangci-lint run (skipped if not installed)
  test     Run go test ./...
  clean    Remove built binaries from repo root
  docker   Build a local docker image tagged satellites:<version> + satellites:local
           (stamps VERSION/BUILD/GIT_COMMIT as build args)
EOF
}

main() {
  local sub="${1:-build}"
  case "$sub" in
    build)  cmd_build ;;
    server) cmd_server ;;
    agent)  cmd_agent ;;
    client) cmd_client ;;
    fmt)    cmd_fmt ;;
    vet)    cmd_vet ;;
    lint)   cmd_lint ;;
    test)   cmd_test ;;
    clean)  cmd_clean ;;
    docker) cmd_docker ;;
    -h|--help|help) usage; exit 0 ;;
    *)      echo "unknown subcommand: $sub" >&2; usage; exit 2 ;;
  esac
}

main "$@"
