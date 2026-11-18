#!/bin/bash
# Stands in for the Claude CLI: performs the file and command operations
# Claude's tools perform, through the same paths, and reports timings.
if [ "$1" = "--version" ]; then echo "2.1.292 (Claude Code)"; exit 0; fi
now() { date +%s%N; }
ms() { echo $(( ($(now) - $1) / 1000000 )); }
# Claude's Bash tool wraps every command like this before SHELL_PREFIX.
bash_tool() {
  local q="'${1//\'/\'\\\'\'}'"
  "$CLAUDE_CODE_SHELL_PREFIX" "source $HOME/.claude/shell-snapshots/snapshot-bash-1.sh 2>/dev/null || true && eval $q < /dev/null && pwd -P >| /tmp/claude-e2e$$-cwd"
}
case " $* " in *" --print "*) mode=scenario;; *) mode=echo;; esac
if [ "$mode" = echo ]; then
  # Interactive stand-in: echo lines until "quit"; survives reattach.
  echo "FAKE-CLAUDE READY pid=$$"
  while IFS= read -r line; do
    [ "$line" = quit ] && exit 0
    case "$line" in
      run:*) bash_tool "${line#run:}";;
      size) echo "size:$(stty size)";;
      *) echo "echo:$line";;
    esac
  done
  exit 0
fi
declare -A t
s=$(now); for f in src/f*.go; do cat "$f" >/dev/null; done; t[read_100_files]=$(ms $s)
s=$(now); n=$(find . -name '*.go' | wc -l); t[walk_tree]=$(ms $s)
s=$(now); echo "// edited by claude $$" >> src/f1.go; t[edit_file]=$(ms $s)
s=$(now); bash_tool 'true'; t[bash_true_1]=$(ms $s)
s=$(now); bash_tool 'true'; t[bash_true_2]=$(ms $s)
s=$(now); seen=$(bash_tool 'tail -1 src/f1.go'); t[bash_reads_edit]=$(ms $s)
ok_before=false; [ "$seen" = "// edited by claude $$" ] && ok_before=true
s=$(now); bash_tool "echo generated-$$ > gen.txt"; t[bash_writes_file]=$(ms $s)
ok_after=false; [ "$(cat gen.txt 2>/dev/null)" = "generated-$$" ] && ok_after=true
s=$(now); git status --porcelain >/dev/null; t[git_status]=$(ms $s)
s=$(now); hits=$(rg -l needle | wc -l); t[grep]=$(ms $s)
s=$(now); rg -l --no-ignore needle >/dev/null; t[grep_no_ignore]=$(ms $s)
s=$(now); cat node_modules/pkg/index.js >/dev/null; t[read_ignored_file]=$(ms $s)
printf '{"grep_hits":%s,"files_walked":%s,"command_sees_claude_edit":%s,"claude_sees_command_write":%s' "$hits" "$n" "$ok_before" "$ok_after"
for k in "${!t[@]}"; do printf ',"%s":%s' "$k" "${t[$k]}"; done
echo '}'
