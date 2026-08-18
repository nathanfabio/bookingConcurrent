#!/usr/bin/env bash
# Repo hygiene guards (run by `make lint` and CI).
#
# These encode rules from CLAUDE.md that static Go linters express poorly:
#   §10 — no connection strings or secrets in Go source
#   §3  — no password/secret values in log calls
#   §1  — adapters do not import sibling adapters (interfaces come from the
#         application layer that consumes them)
set -euo pipefail

cd "$(dirname "$0")/.."

fail=0

# --- Guard 1: connection string literals in Go source ------------------------
# Config is env-driven; a literal scheme:// URL in source is either a
# hardcoded address or a leaked credential.
#
# Two legitimate exceptions are filtered out:
#   - internal/platform/config/ validates URL *shape* (scheme prefixes) and
#     necessarily names the schemes; it holds no addresses or credentials.
#   - test files build fixture URLs from variables.
hits=$(grep -RInE '(redis|rediss|postgres|postgresql|amqp|amqps)://' --include='*.go' . \
  | grep -vE '^\./internal/platform/config/|_test\.go:' || true)
if [ -n "$hits" ]; then
  echo "$hits"
  echo "FAIL: connection string literal found in Go source (CLAUDE.md §10 — all addresses are env-driven)" >&2
  fail=1
fi

# --- Guard 2: secret-looking values passed to structured log calls -----------
# Catches patterns like slog.String("password", pw). Logging hashes is also
# disallowed: it helps no one and narrows an attacker's options.
if grep -RInE '\b(String|Any|Attr)\(\s*"(password|password_hash|secret|token|authorization)"' --include='*.go' internal/ cmd/; then
  echo "FAIL: possible secret value passed to a log call (CLAUDE.md §3)" >&2
  fail=1
fi

# --- Guard 3: adapters importing sibling adapters ----------------------------
# Each adapter implements an application-owned interface; adapters never
# compose each other directly. (Self-imports are fine and skipped.)
for dir in internal/adapters/*/; do
  [ -d "$dir" ] || continue
  name=$(basename "$dir")
  for other in internal/adapters/*/; do
    othername=$(basename "$other")
    [ "$name" = "$othername" ] && continue
    if grep -RIn "bookingConcurrent/internal/adapters/$othername" --include='*.go' "$dir"; then
      echo "FAIL: adapter '$name' imports sibling adapter '$othername' (CLAUDE.md §1)" >&2
      fail=1
    fi
  done
done

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "lint-guards: OK"
