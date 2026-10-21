# The functions check.sh / test.sh / release.sh share. A caller cds to the repository root first and
# then . scripts/lib.sh
#
# What is looked at are the repository's files: the checked-in ones plus the newly created ones that
# have not been git added. Not find / grep -r: a copy .gitignore keeps out (a worktree under
# .claude/worktrees, say) does not count and must not be checked.
# One deleted but not staged has to be left out -- the index still holds it while the disk no longer
# does, and handing it to gofmt is a failure hiding inside a $(...) that has set -e exit the script
# without a word
files() {
  git ls-files -co --exclude-standard -- "$@" | while IFS= read -r f; do
    if [ -e "$f" ]; then echo "$f"; fi
  done
}

# Every Go module directory in the repository (the root module being .), taken as files takes them:
# check.sh is already checking a newly added module before it is git added, so test.sh has to run its
# tests as well
modules() {
  files go.mod '*/go.mod' | xargs -n1 dirname | sort
}

# hooks <event> <log file>: lists, in the order they appear, the packages of the hooks that ran this
# event (starting / stopping), one per line.
# release.sh's --smoke / --verify compare the start and stop orders with it; it lives here so that
# check.sh can test it against a fixed sample log -- when the log format changes, what goes red is
# check.sh and not the --verify after the tags have been pushed.
#
# The framework writes one starting / stopping line per hook, with the field hook=<package>.<func>.
# The first half of the log is slog's default format from before xlog was installed
# (... INFO starting hook=xlog.initXLog) and the second half is xlog's JSON
# ("msg":"stopping",…,"hook":"xredis.closeXRedis"); both are recognized.
# In the JSON, msg and hook need not be adjacent: the hostname and pid xlog puts on every line sit
# between them, and requiring them to be adjacent would have --verify recognize not one hook. The
# [^}]* keeps the two inside one log line.
# Only the package name is taken: one package may register several start hooks, and the unit of
# pairing is the package
hooks() {
  sed -n -e "s/.*\"msg\":\"$1\"[^}]*\"hook\":\"\([^\"]*\)\".*/\1/p" \
         -e "s/.* INFO $1 hook=\([^ ]*\).*/\1/p" "$2" | cut -d. -f1 | uniq
}
