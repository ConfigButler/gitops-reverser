#!/usr/bin/env bash
# PreToolUse(Bash) hook: refuse `task … | something` when pipefail is not set.
#
# Why: a pipeline's exit status is the LAST command's status, so
#
#     task prepare-e2e | tail -15 && task test-e2e
#
# reports `tail`'s success even when `prepare-e2e` failed — and the suite then
# runs against a cluster that was never created, failing much later in
# SynchronizedBeforeSuite with an unrelated-looking error. This has cost real
# debugging cycles more than once (see docs/design/support-boundary/
# render-fidelity.md and kustomize-token-writeback-explained.md), which is why
# it is enforced mechanically here rather than written down a third time.
#
# The root Taskfile sets `pipefail` for commands *inside* tasks; that does
# nothing for a pipeline typed at the shell, which is what this guards.
#
# Reads the hook payload on stdin, emits a PreToolUse deny decision on stdout.
set -uo pipefail

payload="$(cat)"
cmd="$(printf '%s' "${payload}" | jq -r '.tool_input.command // ""')"

# Already guarded — the caller opted into pipefail semantics explicitly.
case "${cmd}" in
*pipefail*) exit 0 ;;
esac

# Match a `task` invocation whose stdout is piped onward:
#   (^|[;&(|])       start of command, or after a separator (covers && and ||)
#   task[[:space:]]  the `task` binary, not a word ending in "task"
#   ([^|;&]|&[^&])*  its arguments — a bare & is allowed so `2>&1` still counts,
#                    but && ends the command and stops the match
#   \|[^|]           a real pipe, not ||
pipe_re='(^|[;&(|])[[:space:]]*task[[:space:]]([^|;&]|&[^&])*\|[^|]'

if ! printf '%s' "${cmd}" | grep -Eq "${pipe_re}"; then
  exit 0
fi

reason='Piping `task` into another command masks its exit status: the pipeline reports the LAST command'\''s status, so a failed task looks like success. This has caused a suite to run against a cluster that was never created. Either drop the pipe and let the output stream, or capture it and check the status yourself:

    task <target> >/tmp/task.log 2>&1; rc=$?; tail -20 /tmp/task.log; exit $rc

If you genuinely want the pipe, prefix the command with `set -o pipefail;`.'

jq -nc --arg r "${reason}" '{
  hookSpecificOutput: {
    hookEventName: "PreToolUse",
    permissionDecision: "deny",
    permissionDecisionReason: $r
  }
}'
