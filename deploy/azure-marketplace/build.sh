#!/usr/bin/env bash
# The Azure Marketplace managed application's package (spec 0020 phase 4b): mainTemplate.json built
# from the Container Apps recipe, with the image pinned by digest and the built-in signer role, the
# customer usage attribution deployment, and createUiDefinition.json; zipped reproducibly.
#
#   deploy/azure-marketplace/build.sh <image@sha256:…> <out.zip>
#
# Optional: KISTA_MARKETPLACE_PID, the plan's customer usage attribution ID exactly as Partner
# Center shows it on the plan's technical configuration (pid-…): the template has nested
# deployments, so Partner Center cannot add it itself. Without it the package is for service
# catalog only. Needs bicep (or az bicep), jq and zip; a byte-identical rebuild needs bicep 0.47.16.
set -euo pipefail

image=${1:?usage: build.sh <image@sha256:…> <out.zip>}
out=${2:?usage: build.sh <image@sha256:…> <out.zip>}
out=$(cd "$(dirname "$out")" && pwd)/$(basename "$out")
here=$(cd "$(dirname "$0")" && pwd)
recipe=$here/../azure-container-apps
pid=${KISTA_MARKETPLACE_PID:-}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail() { echo "build.sh: $*" >&2; exit 1; }

# the image by digest: a tag could change what every customer runs
[[ "$image" =~ ^[a-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$ ]] || fail "the image must be pinned by digest: $image"
[ -z "$pid" ] || [[ "$pid" =~ ^pid-[a-z0-9-]{1,80}$ ]] || fail "KISTA_MARKETPLACE_PID is not a tracking ID (pid-…, as Partner Center shows it)"

if command -v bicep >/dev/null; then
	bicep build "$recipe/main.bicep" --outfile "$work/built.json"
else
	az bicep build -f "$recipe/main.bicep" --outfile "$work/built.json"
fi

# managed applications take languageVersion 1.0 only
lv=$(jq -r '.languageVersion // "1.0"' "$work/built.json")
[ "$lv" = "1.0" ] || fail "the template is languageVersion $lv: managed applications take 1.0 only (no user-defined types)"

# fixed values, never asked: the image and the signer role (the package makes no role definition);
# the bootstrap always comes from the UI
jq --arg image "$image" --arg pid "$pid" '
	.parameters.image.defaultValue = $image
	| .parameters.image.allowedValues = [$image]
	| .parameters.signerRole.defaultValue = "builtin"
	| .parameters.signerRole.allowedValues = ["builtin"]
	| del(.parameters.bootstrap.defaultValue)
	| if $pid == "" then . else .resources += [{
		type: "Microsoft.Resources/deployments",
		apiVersion: "2025-04-01",
		name: $pid,
		properties: {mode: "Incremental", template: {
			"$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentTemplate.json#",
			contentVersion: "1.0.0.0", resources: []}}
	}] end
' "$work/built.json" >"$work/mainTemplate.json"

# every UI output is a parameter, and every parameter without a default is an output
outputs=$(jq -r '.parameters.outputs | keys[]' "$here/createUiDefinition.json" | sort)
params=$(jq -r '.parameters | keys[]' "$work/mainTemplate.json" | sort)
required=$(jq -r '.parameters | to_entries[] | select(.value.defaultValue == null) | .key' "$work/mainTemplate.json" | sort)
extra=$(comm -23 <(echo "$outputs") <(echo "$params"))
[ -z "$extra" ] || fail "createUiDefinition outputs that are no template parameter: $extra"
unmapped=$(comm -23 <(echo "$required") <(echo "$outputs"))
[ -z "$unmapped" ] || fail "template parameters without a default that no output sets: $unmapped"
for fixed in image signerRole; do
	! grep -qx "$fixed" <<<"$outputs" || fail "createUiDefinition must not set $fixed: the package fixes it"
done

cp "$here/createUiDefinition.json" "$work/createUiDefinition.json"
chmod 0644 "$work/mainTemplate.json" "$work/createUiDefinition.json"
TZ=UTC touch -t 202601010000 "$work/mainTemplate.json" "$work/createUiDefinition.json"
rm -f "$out"
(cd "$work" && TZ=UTC zip -X -q -D "$out" mainTemplate.json createUiDefinition.json)
if command -v sha256sum >/dev/null; then sum=$(sha256sum "$out"); else sum=$(shasum -a 256 "$out"); fi
echo "build.sh: $out (${sum%% *})"
