#!/usr/bin/env bash
# Removes a stack up.sh made, by deleting its project: the keys, the bucket and its objects, the
# service account, the role and the federation go with it. GCP keeps a deleted project recoverable
# for 30 days (gcloud projects undelete), then purges it; its KMS key versions are destroyed then.
#
#   deploy/gcp/down.sh <project-id> [--yes]
set -euo pipefail

project=${1:?usage: down.sh <project-id> [--yes]}
case $project in
kista-*) ;;
*) echo "down.sh: deletes only kista- projects (made by up.sh), not $project" >&2; exit 2 ;;
esac
labels=$(gcloud projects describe "$project" --format 'value(labels.purpose)')
if [ "$labels" != kista-test ]; then
	echo "down.sh: $project is not labelled purpose=kista-test; not deleting it" >&2
	exit 2
fi
if [ "${2:-}" != --yes ]; then
	read -r -p "Delete project $project and everything in it? Type its id: " answer
	[ "$answer" = "$project" ] || { echo "down.sh: not deleted"; exit 1; }
fi
gcloud projects delete "$project" --quiet
rm -f "$(cd "$(dirname "$0")" && pwd)/.state-$project"
echo "down.sh: $project deleted (recoverable for 30 days: gcloud projects undelete $project)"
