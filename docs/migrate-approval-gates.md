# Migrating manual-approval gates to approvals

Before, an approval gate was a plain RolloutGate. It had no owner, no
`gate.kuberik.com/*-name` label, and a list of `allowedVersions`.
Now a policy owns the gate and a RolloutApproval object records each approval.

The migration is opt-in per Rollout. You can undo it until step 5.

Every command below takes `--context <ctx>` if you do not want the current kubectl context.

## 1. Upgrade

Upgrade the controller and the chart to a release that has `ApprovalPolicy`,
`ClusterApprovalPolicy` and `RolloutApproval`.
Nothing changes for existing Rollouts. Legacy gates keep working.

## 2. Use a dashboard that writes both forms

Upgrade the dashboard too.
While legacy gates exist, an approval in the dashboard creates the RolloutApproval and also patches the legacy gate.
New approvals then need no backfill later.

## 3. Back up the old approvals

Start with a dry run. It changes nothing.

```sh
hack/migrate-approval-gates.sh
```

Use `-n <namespace>` to look at one namespace.
The output lists each legacy gate and the approvals it would create.

Read the list. A kill-switch gate can have the same shape as an approval gate.
If a gate on the list is not an approval gate, do not migrate it.
Run with `-n` on the others, or remove the object from the cluster first.

A gate with `allowedVersions: []` gets no approvals. Nothing was approved, so nothing is created.

Then create the approvals. Pick one:

```sh
# Create them in the cluster
hack/migrate-approval-gates.sh --apply

# Or write YAML for a GitOps repo, one file per approval
hack/migrate-approval-gates.sh --emit-manifests ./approvals
```

The files land in `./approvals/<namespace>/<name>.yaml`. Commit them to the repo that holds the Rollout.

The script is safe to run again. The names are fixed, so a second run creates nothing.
If a name already exists with a different spec, the script reports `CONFLICT` and exits non-zero at the end.
Fix that object by hand and run the script again.

The approvals have no `by`, because nobody approved them through the API.

## 4. Add the policy

Add an `ApprovalPolicy` whose selector matches the Rollout.

```yaml
apiVersion: kuberik.com/v1alpha1
kind: ApprovalPolicy
metadata:
  name: prod-needs-approval
  namespace: shop
spec:
  rolloutSelector:
    matchLabels:
      env: prod
```

The controller creates a second gate, owned by the policy.
Both gates hold the same list now, and both must allow a build.
The Rollout behaves as before.

Check that the policy gate lists the same versions as the legacy gate:

```sh
kubectl -n shop get rolloutgates -o custom-columns=NAME:.metadata.name,ALLOWED:.spec.allowedVersions
kubectl -n shop get rollout checkout -o jsonpath='{.status.gates}'
```

To roll back, delete the policy. The legacy gate is still there and behaves as before.
The approvals stay and have no effect without a policy.

## 5. Delete the legacy gate

```sh
kubectl -n shop delete rolloutgate manual-approval
```

In a GitOps repo, steps 3 to 5 are one pull request:
the policy, the approval manifests from `--emit-manifests`, and the removal of the gate's manifest.

After this step the dashboard approves through RolloutApprovals alone.
New deploys record the approval in `status.history[].approval`.

## 6. Clean up

After one minor release the dashboard drops its legacy patching.
Run the script once more in dry run. It must list no gates.

```sh
hack/migrate-approval-gates.sh
```
