# Automated Rollback: Conceptual Safety Analysis & Proposal

## 1. The Core Risk: When is Rollback Unsafe?
Automating rollback effectively means automating the decision to revert state. This is unsafe when the **old code** cannot operate correctly with the **current state** of the world (database, external APIs, etc.).

### Critical "Unsafe" Scenarios
1.  **Destructive Database Changes (The #1 Killer)**
    *   **Scenario:** v2 drops a column `email` and moves data to `user_email`. v1 expects `email`.
    *   **Result:** Rolling back to v1 causes immediate crash (Start-up failure: "Unknown column 'email'").
    *   **Mitigation:** Rollbacks are *impossible* here without data restoration.

2.  **Forward-Only Migrations**
    *   **Scenario:** v2 runs a migration that changes the format of a shared lock file or status field.
    *   **Result:** v1 reads the field, sees unknown enum value, panics.
    *   **Mitigation:** Application code must be written to be tolerant of "future" values (Forward Compatibility).

3.  **Broken External Contracts**
    *   **Scenario:** v2 changes the payload format sent to an external billing service. The billing service updates its schema to match.
    *   **Result:** Rolling back to v1 sends the old format, which the billing service now rejects.

## 2. Industry Standard Safety Mechanisms

How do mature organizations automate this safely? They don't just "revert"; they enforce **Contracts** and **Gates**.

### A. The "Expand-Contract" Pattern (Database)
To support safe rollbacks, database changes **MUST** be decoupled from code changes.
1.  **Expand (v2):** Add new column `user_email`. Code writes to BOTH `email` and `user_email`. (Safe to rollback to v1).
2.  **Migrate:** Backfill data.
3.  **Contract (v3):** Remove code usage of `email`. (Safe to rollback to v2).
4.  **Cleanup (v4):** Drop column `email`. (**Unsafe** to rollback to v3).

**Implication for Controller:** The controller cannot know if the user followed this pattern. It must rely on user signals.

### B. "Rollback Gates" / Pre-Conditions
Before triggering a rollback, systems (like Spinnaker or custom Operators) check:
*   **Time Threshold:** "Only rollback if the failure happened < 10 mins after deploy." (Assumption: State hasn't drifted too far).
*   **Schema Version Check:** Check if the DB schema version matches what the old binary expects.
*   **Safety Annotation:** Explicitly marking a release as `rollback-safe: "true"`.

## 3. Proposal for Rollout Controller

We cannot enforce database patterns on users, but we can give them tools to communicate safety.

### Proposed Mechanism: The "Safe Rollback Contract"

We introduce a "Handshake" between the User and the Controller. The Controller will **ONLY** automate rollback if the user explicitly opts-in for that specific version range.

#### 1. Explicit Safety Signals (Annotations)
The `VersionInfo` (derived from OCI images) should support an annotation:
*   `rollout.kuberik.com/rollback-barrier: "true"`
    *   **Meaning:** "Do not automatically roll back *past* this version."
    *   **Use Case:** This version introduced a destructive DB change.

#### 2. The "Stability Window"
*   **Concept:** Most "bad" deployments fail quickly (crash loop, bad config). These are usually safe to rollback because they likely didn't run long enough to corrupt state.
*   **Rule:** If failure occurs within `X` minutes (e.g., Bake Time), rollback is allowed. If failure occurs after 24 hours (Day 2 op), disable auto-rollback because the state might have drifted comfortably to the new version.

#### 3. "Rollback" vs "Roll Forward"
*   **Smart Decision:** If the automated rollback fails (e.g., v1 also crashes), the controller must **Stop**. Do not try v0. Stop and alert ("Panic Mode").

### Revised Configuration Design

```yaml
spec:
  rollback:
    strategy: Automatic
    rules:
      - on: "HealthCheckFailed"
        allow: "Always" # Or "WithinBakeTime"
      - on: "CrashLoopBackOff"
        allow: "Within10m"
    safety:
      # If true, controller looks for OCI annotation 'rollback-barrier'
      respectBarriers: true
```

## 4. Summary of Recommendations
1.  **Default to Safe:** Automated rollback should be **disabled** by default.
2.  **User Responsibility:** Clearly document that "Safe Rollbacks require Backward Compatible Database Changes".
3.  **Barrier Mechanism:** Implement the `rollback-barrier` annotation logic. This gives advanced users a way to prevent disasters during major version upgrades.
4.  **Limit Scope:** Only automate rollback for "Immediate Failures" (during Bake/Verification). Long-term failures (Day 2) should require human intervention.
