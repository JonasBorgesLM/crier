# Checking the commit hook

`.claude/settings.json` holds a `PreToolUse` hook that denies `git add -A`,
`git add .`, and `git add --all`. It exists because CLAUDE.md's "one subject per
commit, staged explicitly" was broken four times by the same reflex — a rule
broken the same way four times is one with no mechanism.

## Before you edit the denial message

**The hook body is interpolated into a shell.** Backticks, `$`, and backslashes
in the message are *evaluated*, not printed. The first draft used backticks
around example commands, and the test run executed them — it actually invoked
`git commit`. Nothing was damaged, but a hook that runs the text of its own
error message is worth knowing about before editing it.

Keep the message to plain characters and quotes. Then run the check below.

## The check

```bash
jq -r '.hooks.PreToolUse[0].hooks[0].command' .claude/settings.json > /tmp/hook.sh

for c in 'git add -A' 'git add .' 'git add --all' 'git status && git add -A' \
         'git add docs/ README.md' 'git add -p' 'git commit -m x'; do
  printf '%-28s ' "$c"
  out=$(printf '{"tool_input":{"command":"%s"}}' "$c" | bash /tmp/hook.sh)
  if [ -n "$out" ]; then echo "$out" | jq -r '.hookSpecificOutput.permissionDecision'; else echo allow; fi
done
```

Expected:

```
git add -A                   deny
git add .                    deny
git add --all                deny
git status && git add -A     deny
git add docs/ README.md      allow
git add -p                   allow
git commit -m x              allow
```

Run it with `bash /tmp/hook.sh`, not `bash -c "$CMD"`. The second form re-expands
the command in your own shell, which is how the backtick problem was found — and
is also a way to run it against your working tree by accident.

Also confirm the message survives intact:

```bash
printf '{"tool_input":{"command":"git add -A"}}' | bash /tmp/hook.sh \
  | jq -r '.hookSpecificOutput.permissionDecisionReason'
```
