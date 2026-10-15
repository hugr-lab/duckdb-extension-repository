#!/usr/bin/env bash
# The chart's static checks (spec 0013): helm lint --strict and kubeconform on every values file in
# tests/pass and kind/values.yaml (also with a release name at Helm's 53-character limit), and helm
# template failing with the expected message on every file in tests/fail (its first line:
# "# want: <text>"). Needs helm and kubeconform.
#   deploy/helm/test.sh
set -euo pipefail
cd "$(dirname "$0")"
chart=kista
k8s=${KUBERNETES_VERSION:-1.30.0}
fails=0

for f in tests/pass/*.yaml kind/values.yaml; do
	helm lint --strict "$chart" -f "$f" >/dev/null || { echo "FAIL lint $f"; helm lint --strict "$chart" -f "$f"; fails=$((fails + 1)); continue; }
	for name in t a-release-name-at-the-limit-of-fifty-three-characters; do
		if ! helm template "$name" "$chart" -f "$f" --kube-version "$k8s" |
			kubeconform -strict -summary=false -kubernetes-version "$k8s" -skip BackendConfig,ManagedCertificate,FrontendConfig; then
			echo "FAIL kubeconform $f ($name)"
			fails=$((fails + 1))
		fi
	done
	echo "ok   $f"
done

for f in tests/fail/*.yaml; do
	want=$(sed -n '1s/^# want: //p' "$f")
	[ -n "$want" ] || { echo "FAIL $f has no '# want:' line"; fails=$((fails + 1)); continue; }
	if out=$(helm template t "$chart" -f "$f" 2>&1); then
		echo "FAIL $f rendered; want an error with: $want"
		fails=$((fails + 1))
	elif [[ "$out" != *"$want"* ]]; then
		echo "FAIL $f: want \"$want\", got: $out"
		fails=$((fails + 1))
	else
		echo "ok   $f"
	fi
done

[ "$fails" = 0 ] || { echo "$fails failed"; exit 1; }
