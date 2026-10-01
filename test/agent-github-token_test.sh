#!/bin/bash
#
# Tests for bin/agent-github-token.
#
# Each test runs a configured copy of the wrapper with stub versions of the
# token helper, `gh` and `uname`, so no GitHub App, Keychain or macOS is needed.
# The wrapper's PATH contains only the stubs, so a real `gh` is never called.
#
# Usage: test/agent-github-token_test.sh

# Stub bodies are single-quoted on purpose: they expand when the stub runs.
# shellcheck disable=SC2016

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WRAPPER="${REPO_ROOT}/bin/agent-github-token"

TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/agent-github-token-test.XXXXXX")"
trap 'rm -rf "${TMP_ROOT}"' EXIT

tests=0
failures=0

# --- Helpers -----------------------------------------------------------------

# write_script FILE BODY: write an executable bash script. Stubs get a normal
# PATH so they can use cat etc.; only the wrapper runs with the restricted one.
write_script() {
  printf '#!/bin/bash\nPATH=/usr/bin:/bin\n%s\n' "$2" > "$1"
  chmod +x "$1"
}

# stub NAME BODY: put a command on the wrapper's PATH.
stub() { write_script "${SANDBOX}/path/$1" "$2"; }

# helper BODY: replace the token helper that sits next to the wrapper.
helper() { write_script "${SANDBOX}/bin/gh-app-token-generator" "$1"; }

# setup creates a fresh sandbox for one test:
#   bin/agent-github-token      the wrapper, configured with app 12345 / installation 67890
#   bin/gh-app-token-generator  stub helper that records its args and prints a token
#   path/                       the wrapper's entire PATH: dirname, uname (Darwin), gh
setup() {
  SANDBOX="$(mktemp -d "${TMP_ROOT}/case.XXXXXX")"
  export SANDBOX
  mkdir -p "${SANDBOX}/bin" "${SANDBOX}/path"

  sed -e 's/<Your-App-ID>/12345/' -e 's/<Your-Installation-ID>/67890/' \
    "${WRAPPER}" > "${SANDBOX}/bin/agent-github-token"
  chmod +x "${SANDBOX}/bin/agent-github-token"

  ln -s "$(command -v dirname)" "${SANDBOX}/path/dirname"
  stub uname 'echo Darwin'
  stub gh 'echo "$*" > "${SANDBOX}/gh.args"; cat > "${SANDBOX}/gh.stdin"'
  helper 'echo "$*" > "${SANDBOX}/helper.args"; printf ghs_stub_token'
}

run_wrapper() {
  set +e
  PATH="${SANDBOX}/path" "${SANDBOX}/bin/agent-github-token" \
    > "${SANDBOX}/stdout" 2> "${SANDBOX}/stderr"
  STATUS=$?
  set -e
}

fail() {
  echo "    $*" >&2
  case_failed=1
}

assert_status() {
  [[ "${STATUS}" == "$1" ]] || fail "exit status ${STATUS}, want $1"
}

# assert_contains FILE TEXT
assert_contains() {
  grep -qF -- "$2" "${SANDBOX}/$1" ||
    fail "$1 does not contain '$2'; got: $(cat "${SANDBOX}/$1")"
}

# assert_not_contains FILE TEXT
assert_not_contains() {
  ! grep -qF -- "$2" "${SANDBOX}/$1" || fail "$1 unexpectedly contains '$2'"
}

# assert_file FILE CONTENT (trailing newlines ignored)
assert_file() {
  if [[ ! -f "${SANDBOX}/$1" ]]; then
    fail "$1 was not written"
  elif [[ "$(cat "${SANDBOX}/$1")" != "$2" ]]; then
    fail "$1 is '$(cat "${SANDBOX}/$1")', want '$2'"
  fi
}

assert_not_called() {
  [[ ! -e "${SANDBOX}/$1.args" ]] || fail "$1 was called with: $(cat "${SANDBOX}/$1.args")"
}

# --- Tests -------------------------------------------------------------------

test_logs_gh_in_with_generated_token() {
  run_wrapper
  assert_status 0
  assert_file helper.args "12345 67890"
  assert_file gh.args "auth login --hostname github.com --with-token"
  assert_file gh.stdin "ghs_stub_token"
  assert_contains stdout "Successfully authenticated"
  assert_not_contains stdout "ghs_stub_token"
  assert_not_contains stderr "ghs_stub_token"
}

test_refuses_to_run_outside_macos() {
  stub uname 'echo Linux'
  run_wrapper
  assert_status 1
  assert_contains stderr "designed for macOS"
  assert_not_called helper
  assert_not_called gh
}

test_requires_gh() {
  rm "${SANDBOX}/path/gh"
  run_wrapper
  assert_status 1
  assert_contains stderr "The GitHub CLI ('gh') is not installed"
  assert_not_called helper
}

test_requires_helper_next_to_script() {
  rm "${SANDBOX}/bin/gh-app-token-generator"
  run_wrapper
  assert_status 1
  assert_contains stderr "was not found in the same directory"
  assert_contains stderr "go build -o ${SANDBOX}/bin/gh-app-token-generator ./cmd/gh-app-token-generator"
  assert_not_called gh
}

test_helper_failure_stops_before_login() {
  helper 'echo "Error:  invalid app ID: x" >&2; exit 1'
  run_wrapper
  assert_status 1
  assert_contains stderr "invalid app ID"
  assert_not_called gh
}

test_empty_token_stops_before_login() {
  helper 'exit 0'
  run_wrapper
  assert_status 1
  assert_contains stderr "produced no output"
  assert_not_called gh
}

test_reports_gh_login_failure() {
  stub gh 'cat > /dev/null; exit 1'
  run_wrapper
  assert_status 1
  assert_contains stderr "'gh auth login' failed"
  assert_not_contains stdout "Successfully authenticated"
}

# --- Runner ------------------------------------------------------------------

for test_name in $(compgen -A function test_); do
  tests=$((tests + 1))
  case_failed=0
  setup
  "${test_name}"
  if [[ "${case_failed}" == 0 ]]; then
    echo "ok   ${test_name}"
  else
    echo "FAIL ${test_name}"
    failures=$((failures + 1))
  fi
done

echo "${tests} tests, ${failures} failed"
[[ "${failures}" == 0 ]]
