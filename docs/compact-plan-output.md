# Compact plan-output fork

## Objective

Make the Terraform MCP server return only a compact JSON description of actionable plan changes. Sensitive values must never reach the MCP client, and the existing full-plan JSON tool must not be available.

The raw HCP Terraform plan is still fetched internally, but it is parsed and reduced before it becomes an MCP result. The fork must not expose a raw-plan fallback tool or an opt-in that bypasses redaction.

## Public MCP behavior

Add a new read-only tool:

```text
get_plan_changes(plan_id: string) -> compact JSON
```

Disable `get_plan_json_output` completely:

- remove it from `pkg/toolsets/mapping.go`;
- remove its dynamic registration from `pkg/tools/dynamic_tool.go`;
- delete the old tool implementation/tests once the replacement is covered;
- update README, `cmd/terraform-mcp-server/instructions.md`, and tool-filtering documentation;
- ensure `--tools=get_plan_json_output` no longer exposes a raw-plan path.

Do not change `get_plan_details` or `get_plan_logs` in this first scope. They remain separate tools; log-size limiting can be a follow-up.

## Compact response contract

The response is custom JSON, not a valid full `terraform show -json` document. Give it an independent schema version:

```json
{
  "schema_version": 1,
  "source_format_version": "1.2",
  "terraform_version": "1.15.8",
  "summary": {
    "create": 2,
    "update": 1,
    "delete": 0,
 "replace": 0
  },
  "resource_changes": [
    {
      "address": "module.database.aws_db_instance.db_instance",
      "module_address": "module.database",
      "actions": ["update"],
      "changed_keys": ["auto_minor_version_upgrade"],
      "diffs": {
        "auto_minor_version_upgrade": {
          "before": true,
          "after": false
        }
      }
    }
  ],
 "deferred_changes": [],
  "output_changes": {},
  "truncated": false
}
```

Contract rules:

1. Include only actionable resource changes. Omit `no-op` entries and normally omit data-source-only `read` actions.
2. Preserve create, update, delete, and replacement action pairs. Count replacements separately while retaining the source action pair.
3. Recursively compare `change.before` and `change.after`. Emit deterministic leaf paths for nested objects and arrays, for example `tags.Environment` and `network[0].cidr`.
4. For creates and deletes, report the known attributes on the non-null side as changed. Preserve unknown values as `{ "unknown": true }` rather than treating them as concrete values.
5. Ignore input `resource_drift` entirely and never emit it. Include `deferred_changes` when present and non-no-op `output_changes` in a separate section. Keep actionable check status as a compact summary if available.
6. Omit bulk sections such as `planned_values`, `prior_state`, `configuration`, `variables`, and unchanged metadata.
7. Add a configurable hard output budget, such as `TF_PLAN_DIFF_MAX_BYTES`, and report `truncated`, included counts, and omitted counts when the budget is reached.

The example run used during investigation returned 193,562 characters, with 26 resource changes (23 `no-op`, two creates, one update) and two drift entries. The compact result should contain the three actionable planned entries, not the 23 no-op resources or any bulk plan sections.

## Sensitive-value filter

Terraform includes `before_sensitive` and `after_sensitive` trees alongside `before` and `after`. Those trees are metadata; they do not guarantee that the corresponding values have been removed.

Implement redaction before diffing:

1. Decode the source with `encoding/json` and `Decoder.UseNumber()`.
2. Apply `before_sensitive` recursively to `before` and `after_sensitive` recursively to `after`, including nested objects and arrays.
3. For every marked path, omit the value and emit only the path plus `"sensitive": true`, or use a fixed non-secret marker. Never copy the original value into the output or logs.
4. Apply the same rule to output changes.
5. If a path is both sensitive and unknown, preserve both facts without exposing a value.
6. Make redaction unconditional. There must be no `include_sensitive=true` option.

Put the pure transformation in a separate file, for example `pkg/tools/tfe/plan_diff.go`, and keep the MCP handler limited to fetch → transform → return. Do not add a dependency for the first implementation.

## Test plan

Add unit tests around the pure transformer for:

- update, create, delete, and replacement actions;
- nested maps, lists, nulls, numeric values, and deterministic path ordering;
- unknown after-values;
- top-level and nested sensitive values, sensitive arrays, sensitive outputs, and a secret sentinel asserting the sentinel never appears in output;
- no-op/read filtering;
- deferred changes and output changes;
- malformed JSON and missing/unsupported fields;
- output-budget truncation and valid JSON after truncation.

Add tool-registration tests asserting `get_plan_changes` is present and `get_plan_json_output` is absent. Update the HCP integration test to call `get_plan_changes`, assert the compact schema, and assert that no full-plan keys or known sensitive fixture values are returned.

Run:

```sh
go test ./...
make docker-build VERSION=1.3.0-diff.0
```

Then manually call `tools/list` and `get_plan_changes` against a representative completed HCP Terraform run. Compare raw and compact byte/token counts without storing the production plan fixture in the repository.

## Implementation sequence

1. Create a `jj` change from current `upstream/main` in the `terraform-mcp-server-fork` workspace.
2. Add the pure plan parser/redactor/diff builder and fixture tests.
3. Add `get_plan_changes`, remove all registration and documentation for `get_plan_json_output`, and update the tool descriptions.
4. Add the output budget and truncation metadata.
5. Run unit/integration tests, build the image, and exercise the MCP server with a real read-only plan.
6. Publish an immutable personal image tag containing the upstream version and fork revision; pin the MCP client to that tag or digest.

## Compatibility decision

This intentionally makes the fork a breaking change at the MCP tool-list level: callers must switch from `get_plan_json_output` to `get_plan_changes`. That is preferable here because the raw tool name must not remain discoverable or accidentally usable. If client migration becomes painful, the fallback is to retain the old name but give it only the compact contract; it must still never return the original full JSON.

## Maintenance

Keep the fork’s `main` bookmark aligned with `upstream/main`. Keep the feature on one bookmark, for example `lswindell.0000.compact-plan-diff`, so rebasing remains small:

```sh
cd /Users/lswindell/repos/terraform-mcp-server-fork

PATCH_BOOKMARK="lswindell.0000.compact-plan-diff"

jj git fetch --remote upstream
jj rebase --branch "$PATCH_BOOKMARK" --onto upstream/main
jj bookmark set main --revision upstream/main
jj git push --remote origin --bookmark main
jj git push --remote origin --bookmark "$PATCH_BOOKMARK"
```

Review upstream changes to plan JSON handling, tool registration, `go-tfe`, and MCP SDK versions before each rebase. Run the secret-redaction and schema tests before publishing every image.

## References

- [Terraform JSON output format](https://developer.hashicorp.com/terraform/internals/json-format)
- [HCP Terraform Plans API](https://developer.hashicorp.com/terraform/cloud-docs/api-docs/plans)
- [Fork compact plan tool](../pkg/tools/tfe/get_plan_changes.go)
- [Current dynamic tool registration](../pkg/tools/dynamic_tool.go)
- [Current toolset mapping](../pkg/toolsets/mapping.go)
