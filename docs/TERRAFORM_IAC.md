# Terraform & Infrastructure as Code

> Owner: SKYBRIDGE Engineering
> Status: accepted
> Version: 2
> Last updated: 2026-09-25

Pinned versions: see `.tool-versions` / `mise.toml` (Terraform 1.9.x, AWS provider ~> 5.x, azurerm ~> 4.x). CI enforces `fmt -check`, `validate`, `plan`. Verify backend behavior against official docs before implementation (links at bottom).

## Why Terraform

Infrastructure should be reproducible and reviewable.

```text
Terraform
   |
+-- AWS (EKS, RDS PG, S3, VPC, IAM roles)
+-- Azure (AKS, Azure PG, Blob, VNet, Front Door Standard, identities)
```

Problem → reason → trade-off → burden → test: Terraform gives reviewable diffs and state, at the cost of state management and provider drift; burden is backend/locking/rotation; test is `plan` on every PR + ephemeral apply per milestone.

## State is sensitive

Terraform state can contain sensitive values. Never commit state files to Git. State backends are encrypted (see below).

## Backend strategy

### AWS

S3 backend with bucket-level versioning + SSE-KMS encryption + lockfile. Bucket properties (versioning/encryption) are configured on the bucket resource/policy, not inside the `backend` block.

```hcl
terraform {
  required_version = "~> 1.9.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.0" }
  }
  backend "s3" {
    bucket       = "skybridge-tfstate-<ACCOUNT>-<REGION>"
    key          = "aws/dev/network.tfstate"
    region       = "us-east-1"
    encrypt      = true
    # kms_key_id supplied via -backend-config or CI env (never committed)
    use_lockfile = true
  }
}
```

Required bucket properties (managed in a bootstrap module, reviewed once): `versioning = Enabled`, `sse = aws:kms (cmk skybridge/<env>)`, `public access blocked`, OIDC-only write. DynamoDB locking is deprecated for new configs — do not use it.

Backend auth: CI assumes `arn:aws:iam::<ACCOUNT>:role/SKYBRIDGE-<ENV>-terraform` via OIDC (repo/branch/environment-restricted). No static keys.

### Azure

`azurerm` backend with Blob Storage + Entra (OIDC/federated) auth. No long-lived SAS as the default path.

```hcl
terraform {
  required_version = "~> 1.9.0"
  required_providers {
    azurerm = { source = "hashicorp/azurerm", version = "~> 4.0" }
  }
  backend "azurerm" {
    storage_account_name = "stskybridge<env>tfstate"
    container_name       = "tfstate"
    key                  = "azure/dev/network.tfstate"
    use_azuread_auth     = true
  }
}
```

Subscription/tenant/client IDs via CI environment (`ARM_*`), never committed. Container has versioning + encryption (platform defaults) + append-only policy for audit exports where used.

Sensitive backend configuration is supplied through the CI environment (OIDC-assumed identity), never committed secrets.

## State separation

Separate state per provider × environment × bounded scope:

```text
aws/dev/network
aws/dev/eks
aws/dev/data (rds+s3)
aws/staging/...
azure/dev/network
azure/dev/aks
azure/dev/data (pg+blob+frontdoor)
```

One blast radius per state file. Cross-scope references via `terraform_remote_state` (read-only) or explicit outputs, never shared write.

## Module versioning

Reusable modules in `infrastructure/terraform/modules/` with explicit `version` (git tag, e.g. `network-v1.2.0`). Pin every consumer (`version = "1.2.0"`). Breaking module changes require a major version bump + migration note.

## Plan vs apply

Pull request (all envs):

```text
fmt -check
validate
plan (per changed scope; plan artifact attached to PR)
security/secret scan
```

Do not apply infrastructure from an unreviewed PR. Plans are bound to commit SHA; never reuse a plan if code/config changed (CI enforces SHA match).

Protected environments (`staging`, `production-like`, any real-cloud milestone):

```text
merge -> reviewed plan (SHA-pinned) -> required reviewers (>=1, approver role) -> approval record -> apply in protected env with OIDC identity -> audit record
```

No automatic production-like apply from an ordinary PR.

## Drift

Terraform `refresh/plan` shows drift for Terraform-managed resources. SKYBRIDGE semantic drift (`docs/DRIFT_RECONCILIATION.md`) is authoritative for the migration gate. Both run: Terraform drift is informational for IaC hygiene; semantic `blocking`/`security_critical` bars cutover.

## Import

Existing resources may be imported (`terraform import` + `plan` showing no diff), but every imported resource must be reviewed (PR with plan output) before becoming desired state. Record import in the experiment log.

## Secret handling

No hardcoded secrets/variables. Prefer OIDC + environment injection + Secrets Manager/Key Vault data sources. Remember values can still reach state — mark sensitive (`sensitive = true`), restrict state access to the Terraform role, and rotate after exposure.

## Destroy safety

Production-like destroy requires explicit approval (`risk: critical`, dual approver). The source environment is NEVER automatically destroyed by v1 migration completion. `terraform destroy` on source scope is denied by policy unless a signed exception exists.

## Versioned modules (provisioning skeleton)

`infrastructure/terraform/modules/` holds one directory per plan component (`azure-*` target set, `aws-*` source-reference parity), each `v0.1.0` with typed variables and no environment-specific defaults. Promotion rule: local path modules become registry/tagged versions at the real-cloud milestone; consumers then pin versions. Verified: `fmt -check` clean, `validate` green on all 14 modules + both roots (Terraform 1.9.8). `plan` requires provider credentials and runs only in CI with OIDC — never `apply`/`destroy` locally.

## Real-cloud preparation boundary (documented, NOT activated)

When (not in this slice) real provisioning begins, all of the following are required first — none are active now:

- AWS OIDC role + Azure federated identity per `docs/SECURITY_MODEL.md` (no static keys, ever)
- isolated state backends per provider × environment × scope (above)
- protected environments with required reviewers + approval records
- reviewed, SHA-pinned `plan` output before any `apply`
- explicit operator approval bound to a policy decision (see Approval workflow)
- `apply` executed by CI identity only, audited; `destroy` as above

## Reference

- https://developer.hashicorp.com/terraform/language/backend/s3
- https://developer.hashicorp.com/terraform/language/backend/azurerm

Evidence: pending implementation validation.
