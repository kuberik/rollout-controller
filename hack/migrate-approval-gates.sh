#!/usr/bin/env bash
# Backfill RolloutApprovals from legacy approval gates.
#
# A legacy gate is a RolloutGate with no ownerReferences, no gate.kuberik.com/*-name
# label, and a non-nil spec.allowedVersions. For every version in that list the script
# creates one RolloutApproval (without the approval.kuberik.com/by annotation).
#
# Dry run by default. Names follow the RolloutApproval naming rule, which the dashboard
# approve endpoint shares byte for byte. Running the script twice changes nothing.
#
# Needs: bash, kubectl, jq, sha256sum.
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: migrate-approval-gates.sh [--apply | --emit-manifests DIR] [-n NAMESPACE] [--context CONTEXT]

  (default)               dry run: print each legacy gate and the approvals it would create
  --apply                 create the RolloutApprovals
  --emit-manifests DIR    write one YAML file per approval to DIR/<namespace>/<name>.yaml
  -n, --namespace NS      only look at gates in NS (default: all namespaces)
  --context CTX           kubectl context to use
  -h, --help              show this help

Set KUBECTL to use a different kubectl binary.
Exit status is non-zero when an approval name already exists with a different spec,
or when a create fails. The script still handles every other gate first.
USAGE
}

mode=dry-run
emit_dir=""
namespace=""
kube_context=""

while [ $# -gt 0 ]; do
  case "$1" in
    --apply) mode=apply ;;
    --emit-manifests)
      [ $# -ge 2 ] || { echo "error: --emit-manifests needs a directory" >&2; exit 2; }
      mode=emit
      emit_dir="$2"
      shift
      ;;
    --emit-manifests=*) mode=emit; emit_dir="${1#*=}" ;;
    -n | --namespace)
      [ $# -ge 2 ] || { echo "error: $1 needs a value" >&2; exit 2; }
      namespace="$2"
      shift
      ;;
    --namespace=*) namespace="${1#*=}" ;;
    --context)
      [ $# -ge 2 ] || { echo "error: --context needs a value" >&2; exit 2; }
      kube_context="$2"
      shift
      ;;
    --context=*) kube_context="${1#*=}" ;;
    -h | --help) usage; exit 0 ;;
    *) echo "error: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

if [ "$mode" = emit ] && [ -z "$emit_dir" ]; then
  echo "error: --emit-manifests needs a directory" >&2
  exit 2
fi

for tool in jq sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || { echo "error: $tool not found" >&2; exit 2; }
done
kubectl_bin="${KUBECTL:-kubectl}"
command -v "$kubectl_bin" >/dev/null 2>&1 || { echo "error: $kubectl_bin not found" >&2; exit 2; }

kc() {
  if [ -n "$kube_context" ]; then
    "$kubectl_bin" --context "$kube_context" "$@"
  else
    "$kubectl_bin" "$@"
  fi
}

ns_args=(-A)
if [ -n "$namespace" ]; then
  ns_args=(-n "$namespace")
fi

# approval_name ROLLOUT VERSION: the RolloutApproval naming rule.
approval_name() {
  local r="$1" v="$2" s n h prefix
  s="$(printf '%s' "$v" | LC_ALL=C tr '[:upper:]' '[:lower:]' | LC_ALL=C sed 's/[^a-z0-9-]/-/g')"
  n="${r}-${s}"
  n="${n%"${n##*[!-]}"}" # strip trailing '-'
  if [ "${#n}" -le 63 ]; then
    printf '%s' "$n"
    return
  fi
  h="$(printf '%s' "$v" | sha256sum | cut -c1-8)"
  prefix="${n:0:54}"
  prefix="${prefix%"${prefix##*[!-.]}"}" # strip trailing '-' and '.'
  printf '%s-%s' "$prefix" "$h"
}

# manifest NAME NAMESPACE ROLLOUT VERSION: YAML for one approval. JSON strings are
# valid YAML double-quoted scalars, so jq does the quoting.
manifest() {
  jq -rn --arg name "$1" --arg ns "$2" --arg rollout "$3" --arg version "$4" '
    "apiVersion: kuberik.com/v1alpha1\n" +
    "kind: RolloutApproval\n" +
    "metadata:\n" +
    "  name: \($name | @json)\n" +
    "  namespace: \($ns | @json)\n" +
    "spec:\n" +
    "  rolloutRef:\n" +
    "    name: \($rollout | @json)\n" +
    "  version: \($version | @json)"'
}

gates_json="$(kc get rolloutgates.kuberik.com "${ns_args[@]}" -o json)"
approvals_json="$(kc get rolloutapprovals.kuberik.com "${ns_args[@]}" -o json)"

# One line per (gate, version): namespace, gate, rollout, version (tab separated, JSON-encoded
# so that odd characters cannot break the line).
rows="$(printf '%s' "$gates_json" | jq -r '
  .items[]
  | select((.metadata.ownerReferences // []) | length == 0)
  | select([(.metadata.labels // {}) | keys[] | select(test("^gate\\.kuberik\\.com/.*-name$"))] | length == 0)
  | select((.spec.allowedVersions | type) == "array")
  | . as $g
  | ($g.spec.rolloutRef.name // "") as $rollout
  | if ($g.spec.allowedVersions | length) == 0
    then [$g.metadata.namespace, $g.metadata.name, $rollout, null] | @json
    else ($g.spec.allowedVersions | unique[] | [$g.metadata.namespace, $g.metadata.name, $rollout, .] | @json)
    end')"

declare -A seen=()  # "<ns>/<name>" -> "<rollout>\t<version>" for names handled in this run
last_gate=""
legacy_gates=0
created=0
unchanged=0
conflicts=0
failures=0

if [ -n "$rows" ]; then
  while IFS= read -r row; do
    ns="$(printf '%s' "$row" | jq -r '.[0]')"
    gate="$(printf '%s' "$row" | jq -r '.[1]')"
    rollout="$(printf '%s' "$row" | jq -r '.[2]')"
    version="$(printf '%s' "$row" | jq -r '.[3] // empty')"
    has_version="$(printf '%s' "$row" | jq -r '.[3] != null')"

    if [ "$last_gate" != "$ns/$gate" ]; then
      last_gate="$ns/$gate"
      legacy_gates=$((legacy_gates + 1))
      echo "legacy gate $ns/$gate (rollout: ${rollout:-<none>})"
      if [ -z "$rollout" ]; then
        echo "  skipped: gate has no rolloutRef"
      fi
    fi
    [ -n "$rollout" ] || continue
    if [ "$has_version" != true ]; then
      echo "  allowedVersions is empty: nothing to create"
      continue
    fi

    name="$(approval_name "$rollout" "$version")"
    key="$ns/$name"
    want="$rollout"$'\t'"$version"

    # Where is the object now? Earlier in this run, else in the cluster.
    have=""
    if [ -n "${seen[$key]+x}" ]; then
      have="${seen[$key]}"
    else
      have="$(printf '%s' "$approvals_json" | jq -r --arg ns "$ns" --arg name "$name" '
        .items[] | select(.metadata.namespace == $ns and .metadata.name == $name)
        | "\(.spec.rolloutRef.name)\t\(.spec.version)"')"
    fi

    if [ -n "$have" ] && [ "$have" != "$want" ]; then
      echo "  CONFLICT $name: exists for a different rollout or version (${have//$'\t'/ / })"
      conflicts=$((conflicts + 1))
      continue
    fi
    seen[$key]="$want"

    case "$mode" in
      emit)
        mkdir -p "$emit_dir/$ns"
        manifest "$name" "$ns" "$rollout" "$version" >"$emit_dir/$ns/$name.yaml.tmp" \
          && mv -f "$emit_dir/$ns/$name.yaml.tmp" "$emit_dir/$ns/$name.yaml"
        echo "  wrote $emit_dir/$ns/$name.yaml ($version)"
        created=$((created + 1))
        ;;
      apply)
        if [ -n "$have" ]; then
          echo "  exists $name ($version)"
          unchanged=$((unchanged + 1))
        elif manifest "$name" "$ns" "$rollout" "$version" | kc create -f - >/dev/null; then
          echo "  created $name ($version)"
          created=$((created + 1))
        else
          echo "  FAILED to create $name ($version)"
          failures=$((failures + 1))
        fi
        ;;
      *)
        if [ -n "$have" ]; then
          echo "  exists $name ($version)"
          unchanged=$((unchanged + 1))
        else
          echo "  would create $name ($version)"
          created=$((created + 1))
        fi
        ;;
    esac
  done <<<"$rows"
fi

echo
case "$mode" in
  dry-run) verb="would create" ;;
  emit) verb="wrote" ;;
  *) verb="created" ;;
esac
echo "$legacy_gates legacy gate(s); $verb $created approval(s); $unchanged already present; $conflicts conflict(s); $failures failure(s)."
if [ "$mode" = dry-run ]; then
  echo "Dry run: nothing was changed. Review the gates above, then run with --apply or --emit-manifests DIR."
  echo "A kill-switch gate can have the same shape as an approval gate. Check each one."
fi

if [ "$conflicts" -gt 0 ] || [ "$failures" -gt 0 ]; then
  exit 1
fi
