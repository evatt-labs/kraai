#!/usr/bin/env bash
# PreToolUse hook for the Bash tool. Denies commands that mutate real cloud
# infrastructure unless KRAAI_ALLOW_MUTATE=1 is set for the session.
#
# AGENTS.md: never run kraai apply or destroy on your own initiative, and
# never create, update or delete real infrastructure unless the task says so.
# This is that rule as code. Read-only calls (plan, describe-*, get-*, list-*,
# sts) pass through untouched.
set -euo pipefail

cmd=$(jq -r '.tool_input.command // empty')
[ -z "$cmd" ] && exit 0
[ "${KRAAI_ALLOW_MUTATE:-}" = "1" ] && exit 0

deny() {
	jq -n --arg reason "$1" '{
		hookSpecificOutput: {
			hookEventName: "PreToolUse",
			permissionDecision: "deny",
			permissionDecisionReason: $reason
		}
	}'
	exit 0
}

# A command position: start of line, or after ; & | ( or $(.
at='(^|[;&|(]|\$\()[[:space:]]*'

kraai_re="${at}(go[[:space:]]+run[[:space:]]+\./cmd/kraai|kraai)[[:space:]]+(-[^[:space:]]+[[:space:]]+)*(apply|destroy)([[:space:]]|$)"
aws_re="${at}aws[[:space:]]+([a-z0-9-]+[[:space:]]+)+(create|delete|put|update|terminate|modify|attach|detach|deregister|register|remove|start|stop|reboot|run-instances|associate|disassociate|revoke|authorize|tag|untag|enable|disable|import|restore|reset|replace|cancel|purge|cp|mv|rm|sync|mb|rb)([a-z-]*)([[:space:]]|$)"

if grep -Eq "$kraai_re" <<<"$cmd"; then
	deny "kraai apply/destroy mutates real infrastructure. AGENTS.md forbids it on the agent's own initiative; set KRAAI_ALLOW_MUTATE=1 for a task that explicitly calls for it."
fi
if grep -Eq "$aws_re" <<<"$cmd"; then
	deny "mutating aws CLI verb. Read-only calls (describe-*, get-*, list-*, sts) are fine; set KRAAI_ALLOW_MUTATE=1 for a task that explicitly calls for a mutation."
fi
exit 0
