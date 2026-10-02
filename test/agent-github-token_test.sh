#!/bin/bash
#
# Tests for bin/agent-github-token.
#
# Each test runs a configured copy of the wrapper with stub versions of the
# token helper, `gh`, `git` and `uname`, so no GitHub App, Keychain or macOS is
# needed. The wrapper's PATH contains only the stubs, so the real `gh` and
# `git` are never called.
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
#   bin/agent-github-token      the wrapper, configured with client ID Iv23liTest
#   bin/gh-app-token-generator  stub helper: records its args, prints a token for 'token'
#   path/                       the wrapper's entire PATH: dirname, cat, uname (Darwin), gh, git
setup() {
  SANDBOX="$(mktemp -d "${TMP_ROOT}/case.XXXXXX")"
  export SANDBOX
  mkdir -p "${SANDBOX}/bin" "${SANDBOX}/path"

  sed -e 's/<Your-Client-ID>/Iv23liTest/' "${WRAPPER}" > "${SANDBOX}/bin/agent-github-token"
  chmod +x "${SANDBOX}/bin/agent-github-token"

  ln -s "$(command -v dirname)" "${SANDBOX}/path/dirname"
  ln -s "$(command -v cat)" "${SANDBOX}/path/cat"
  stub uname 'echo Darwin'
  stub gh 'printf "%s\n" "$@" > "${SANDBOX}/gh.args"; printf "%s" "${GH_TOKEN:-}" > "${SANDBOX}/gh.token"'
  stub git '
printf "[%s]" "$@" >> "${SANDBOX}/git.log"; echo >> "${SANDBOX}/git.log"
case "$1 $2 $3" in
  "rev-parse --git-dir "*) [[ ! -e "${SANDBOX}/not-a-repo" ]] ;;
  "config --local --get-regexp") cat "${SANDBOX}/remotes" 2>/dev/null ;;
  "config --local --unset-all") exit 5 ;;
esac'
  helper 'echo "$*" >> "${SANDBOX}/helper.args"; if [[ "$1" == token ]]; then printf ghu_stub_token; fi'
}

# run_wrapper ARGS...: run the wrapper, capturing stdout, stderr and STATUS.
run_wrapper() {
  set +e
  PATH="${SANDBOX}/path" "${SANDBOX}/bin/agent-github-token" "$@" \
    > "${SANDBOX}/stdout" 2> "${SANDBOX}/stderr"
  STATUS=$?
  set -e
}

fail() {
  echo "    $*" >&2
  case_failed=1
}

assert_status() {
  [[ "${STATUS}" == "$1" ]] || fail "exit status ${STATUS}, want $1; stderr: $(cat "${SANDBOX}/stderr")"
}

# assert_contains FILE TEXT
assert_contains() {
  grep -qF -- "$2" "${SANDBOX}/$1" 2>/dev/null ||
    fail "$1 does not contain '$2'; got: $(cat "${SANDBOX}/$1" 2>/dev/null)"
}

# assert_not_contains FILE TEXT
assert_not_contains() {
  ! grep -qF -- "$2" "${SANDBOX}/$1" 2>/dev/null || fail "$1 unexpectedly contains '$2'"
}

# assert_file FILE CONTENT (trailing newlines ignored)
assert_file() {
  if [[ ! -f "${SANDBOX}/$1" ]]; then
    fail "$1 was not written"
  elif [[ "$(cat "${SANDBOX}/$1")" != "$2" ]]; then
    fail "$1 is '$(cat "${SANDBOX}/$1")', want '$2'"
  fi
}

# assert_not_called NAME: the stub NAME (helper, gh or git) was never run.
assert_not_called() {
  local log
  for log in "${SANDBOX}/$1.args" "${SANDBOX}/$1.log"; do
    [[ ! -e "${log}" ]] || fail "$1 was called: $(cat "${log}")"
  done
}

# --- Tests -------------------------------------------------------------------

test_help_prints_usage() {
  run_wrapper --help
  assert_status 0
  assert_contains stdout "Usage: agent-github-token <command>"
  assert_not_called helper
}

test_no_command_prints_usage() {
  run_wrapper
  assert_status 1
  assert_contains stderr "Usage:"
  assert_not_called helper
}

test_unknown_command() {
  run_wrapper frobnicate
  assert_status 1
  assert_contains stderr "Unknown command 'frobnicate'"
  assert_not_called helper
}

test_login_passes_client_id() {
  run_wrapper login
  assert_status 0
  assert_file helper.args "login Iv23liTest"
}

test_login_client_id_from_environment() {
  set +e
  GH_APP_CLIENT_ID=Iv23liFromEnv PATH="${SANDBOX}/path" "${SANDBOX}/bin/agent-github-token" login \
    > "${SANDBOX}/stdout" 2> "${SANDBOX}/stderr"
  STATUS=$?
  set -e
  assert_status 0
  assert_file helper.args "login Iv23liFromEnv"
}

test_login_requires_client_id() {
  cp "${WRAPPER}" "${SANDBOX}/bin/agent-github-token"
  run_wrapper login
  assert_status 1
  assert_contains stderr "Set GH_APP_CLIENT_ID"
  assert_not_called helper
}

test_passes_status_token_and_logout_to_helper() {
  run_wrapper status
  assert_status 0
  run_wrapper logout
  assert_status 0
  run_wrapper token
  assert_status 0
  assert_file stdout "ghu_stub_token"
  assert_file helper.args "$(printf 'status\nlogout\ntoken --client-id Iv23liTest')"
}

test_token_without_client_id_is_not_pinned() {
  cp "${WRAPPER}" "${SANDBOX}/bin/agent-github-token"
  run_wrapper token
  assert_status 0
  assert_file helper.args "token"
}

test_claude_settings_passes_client_id() {
  run_wrapper claude-settings
  assert_status 0
  assert_file helper.args "claude-settings --client-id Iv23liTest"
}

test_claude_settings_requires_client_id() {
  cp "${WRAPPER}" "${SANDBOX}/bin/agent-github-token"
  run_wrapper claude-settings
  assert_status 1
  assert_contains stderr "Set GH_APP_CLIENT_ID"
  assert_not_called helper
}

test_propagates_helper_exit_status() {
  helper 'echo "Error: not logged in" >&2; exit 3'
  run_wrapper status
  assert_status 3
  assert_contains stderr "not logged in"
}

test_gh_runs_with_fresh_token() {
  run_wrapper gh pr list --repo "owner/repo name"
  assert_status 0
  assert_file helper.args "token --client-id Iv23liTest"
  assert_file gh.token "ghu_stub_token"
  assert_file gh.args "$(printf 'pr\nlist\n--repo\nowner/repo name')"
  assert_not_contains stdout "ghu_stub_token"
  assert_not_contains stderr "ghu_stub_token"
}

test_gh_not_run_without_token() {
  helper 'echo "Error: login expired; run agent-github-token login" >&2; exit 1'
  run_wrapper gh pr list
  assert_status 1
  assert_contains stderr "login expired"
  assert_not_called gh
}

test_gh_requires_gh() {
  rm "${SANDBOX}/path/gh"
  run_wrapper gh pr list
  assert_status 1
  assert_contains stderr "The GitHub CLI ('gh') is not installed"
  assert_not_called helper
}

test_setup_git_configures_credential_helper() {
  run_wrapper setup-git
  assert_status 0
  assert_contains git.log "[config][--local][--unset-all][credential.https://github.com.helper]"
  assert_contains git.log "[config][--local][--add][credential.https://github.com.helper][]"
  assert_contains git.log "[config][--local][--add][credential.https://github.com.helper][!'${SANDBOX}/bin/gh-app-token-generator' git-credential --client-id 'Iv23liTest']"
  assert_contains stdout "git now uses agent-github-token tokens"
  assert_not_contains stderr "Warning"
}

test_setup_git_warns_about_ssh_remotes() {
  printf 'remote.origin.url git@github.com:owner/repo.git\nremote.mirror.url https://github.com/owner/repo.git\n' \
    > "${SANDBOX}/remotes"
  run_wrapper setup-git
  assert_status 0
  assert_contains stderr "Remote 'origin' uses SSH"
  assert_contains stderr "git remote set-url origin https://github.com/OWNER/REPO.git"
  assert_not_contains stderr "'mirror'"
}

test_setup_git_works_with_real_git() {
  # Use the real git, but with a throwaway HOME and global config so the
  # user's own git configuration and credential helpers are never involved.
  local real_git saved_home="${HOME}"
  real_git="$(command -v git)"
  rm "${SANDBOX}/path/git"
  ln -s "${real_git}" "${SANDBOX}/path/git"
  export HOME="${SANDBOX}" GIT_CONFIG_GLOBAL="${SANDBOX}/gitconfig" GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0
  # A global helper that setup-git must override for github.com only.
  git config --global credential.helper '!f() { echo username=global; echo password=global-secret; }; f'
  git init -q "${SANDBOX}/repo"
  helper '
if [[ "$1" == git-credential && "${*: -1}" == get ]]; then
  while read -r line && [[ -n "${line}" ]]; do :; done
  echo username=x-access-token
  echo password=ghu_stub_token
fi'

  cd "${SANDBOX}/repo"
  run_wrapper setup-git
  printf 'protocol=https\nhost=github.com\n\n' | git credential fill > "${SANDBOX}/github.creds" 2>&1 || true
  printf 'protocol=https\nhost=gitlab.com\n\n' | git credential fill > "${SANDBOX}/gitlab.creds" 2>&1 || true
  cd - > /dev/null
  export HOME="${saved_home}"
  unset GIT_CONFIG_GLOBAL GIT_CONFIG_NOSYSTEM GIT_TERMINAL_PROMPT

  assert_status 0
  assert_contains github.creds "password=ghu_stub_token"
  assert_not_contains github.creds "global-secret"
  assert_contains gitlab.creds "password=global-secret"
}

test_setup_git_outside_a_repository() {
  touch "${SANDBOX}/not-a-repo"
  run_wrapper setup-git
  assert_status 1
  assert_contains stderr "inside the git repository"
  assert_not_contains git.log "[config]"
}

test_refuses_to_run_outside_macos() {
  stub uname 'echo Linux'
  run_wrapper token
  assert_status 1
  assert_contains stderr "designed for macOS"
  assert_not_called helper
}

test_requires_helper_next_to_script() {
  rm "${SANDBOX}/bin/gh-app-token-generator"
  run_wrapper token
  assert_status 1
  assert_contains stderr "was not found in the same directory"
  assert_contains stderr "go build -o ${SANDBOX}/bin/gh-app-token-generator ./cmd/gh-app-token-generator"
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
