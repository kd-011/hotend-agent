#!/bin/sh
# Keeps exactly one open issue per (label, title).
#   sync-issue.sh LABEL TITLE BODYFILE   open the issue, or update it if the body changed
#   sync-issue.sh LABEL TITLE            close the issue if one is open (the problem is gone)
# Needs GH_TOKEN with issues: write. Nothing is created when there is nothing to report.
set -eu

label=$1
title=$2
body=${3:-}

gh label create "$label" --color BFD4F2 --description "Opened and closed by the weekly upkeep job" 2>/dev/null || true

number=$(gh issue list --label "$label" --state open --limit 100 --json number,title |
  jq -r --arg t "$title" 'map(select(.title == $t)) | .[0].number // empty')

if [ -n "$body" ]; then
  if [ -z "$number" ]; then
    gh issue create --title "$title" --label "$label" --body-file "$body"
  elif [ "$(gh issue view "$number" --json body --jq .body)" != "$(cat "$body")" ]; then
    gh issue edit "$number" --body-file "$body"
  else
    echo "issue #$number is already up to date"
  fi
elif [ -n "$number" ]; then
  gh issue close "$number" --comment "Clean on the latest weekly run, closing."
else
  echo "clean, nothing to open or close"
fi
