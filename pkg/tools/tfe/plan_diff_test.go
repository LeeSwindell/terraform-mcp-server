// Copyright IBM Corp. 2025
// SPDX-License-Identifier: MPL-2.0

package tools

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildPlanDiff(t *testing.T) {
	t.Run("keeps actionable nested leaf differences and filters no-op/read changes", func(t *testing.T) {
		raw := `{
			"format_version": "1.2",
			"terraform_version": "1.15.8",
			"resource_changes": [
				{"address":"data.example.read","mode":"data","change":{"actions":["read"],"before":null,"after":{"id":"read"}}},
				{"address":"z.noop","change":{"actions":["no-op"],"before":{"value":1},"after":{"value":1}}},
				{"address":"module.app.aws_instance.web","module_address":"module.app","change":{"actions":["update"],"before":{"count":1,"nullable":null,"network":[{"cidr":"10.0.0.0/24"}],"tags":{"Environment":"prod","Unchanged":"same"}},"after":{"count":2,"nullable":null,"network":[{"cidr":"10.0.1.0/24"}],"tags":{"Environment":"stage","Unchanged":"same"}}}}
			]
		}`

		result := decodeCompactPlan(t, raw, 64*1024)
		assert.Equal(t, float64(0), result["summary"].(map[string]any)["create"])
		assert.Equal(t, float64(1), result["summary"].(map[string]any)["update"])

		changes := result["resource_changes"].([]any)
		require.Len(t, changes, 1)
		change := changes[0].(map[string]any)
		assert.Equal(t, "module.app.aws_instance.web", change["address"])
		assert.Equal(t, "module.app", change["module_address"])
		assert.Equal(t, []any{"count", "network[0].cidr", "tags.Environment"}, change["changed_keys"])

		diffs := change["diffs"].(map[string]any)
		assert.Equal(t, float64(1), diffs["count"].(map[string]any)["before"])
		assert.Equal(t, float64(2), diffs["count"].(map[string]any)["after"])
		assert.Equal(t, "10.0.0.0/24", diffs["network[0].cidr"].(map[string]any)["before"])
		assert.Equal(t, "10.0.1.0/24", diffs["network[0].cidr"].(map[string]any)["after"])
	})

	t.Run("reports create delete and replacement actions", func(t *testing.T) {
		raw := `{
			"resource_changes": [
				{"address":"resource.create","change":{"actions":["create"],"before":null,"after":{"id":"new","nested":{"enabled":true}}}},
				{"address":"resource.delete","change":{"actions":["delete"],"before":{"id":"old","nested":{"enabled":true}},"after":null}},
				{"address":"resource.replace","change":{"actions":["delete","create"],"before":{"id":"old"},"after":{"id":"new"}}}
			]
		}`

		result := decodeCompactPlan(t, raw, 64*1024)
		summary := result["summary"].(map[string]any)
		assert.Equal(t, float64(1), summary["create"])
		assert.Equal(t, float64(1), summary["delete"])
		assert.Equal(t, float64(1), summary["replace"])

		changes := result["resource_changes"].([]any)
		require.Len(t, changes, 3)
		create := changes[0].(map[string]any)
		assert.Equal(t, []any{"id", "nested.enabled"}, create["changed_keys"])
		assert.NotContains(t, create["diffs"].(map[string]any), "$")
		replace := changes[2].(map[string]any)
		assert.Equal(t, []any{"delete", "create"}, replace["actions"])
	})

	t.Run("redacts sensitive values in nested fields arrays and outputs", func(t *testing.T) {
		secret := "super-secret-sentinel"
		raw := `{
			"resource_changes": [{
				"address":"aws_db_instance.example",
				"change":{
					"actions":["update"],
					"before":{"password":"super-secret-sentinel","nested":{"token":"super-secret-sentinel"},"items":[{"token":"super-secret-sentinel","name":"old"}]},
					"after":{"password":"super-secret-sentinel-new","nested":{"token":"super-secret-sentinel-new"},"items":[{"token":"super-secret-sentinel-new","name":"new"}]},
					"before_sensitive":{"password":true,"nested":{"token":true},"items":[{"token":true}]},
					"after_sensitive":{"password":true,"nested":{"token":true},"items":[{"token":true}]}
				}
			}],
			"output_changes":{"db_password":{"actions":["update"],"before":"super-secret-sentinel","after":"super-secret-sentinel-new","before_sensitive":true,"after_sensitive":true}}
		}`

		encoded, err := buildPlanDiff([]byte(raw), 64*1024)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), secret)
		assert.NotContains(t, string(encoded), "super-secret-sentinel-new")

		result := decodeJSON(t, encoded)
		change := result["resource_changes"].([]any)[0].(map[string]any)
		diffs := change["diffs"].(map[string]any)
		for _, key := range []string{"password", "nested.token", "items[0].token"} {
			field := diffs[key].(map[string]any)
			assert.Equal(t, redactedPlanValue, field["before"])
			assert.Equal(t, redactedPlanValue, field["after"])
			assert.True(t, field["sensitive"].(bool))
		}
		output := result["output_changes"].(map[string]any)["db_password"].(map[string]any)
		outputDiff := output["diffs"].(map[string]any)["$"].(map[string]any)
		assert.Equal(t, redactedPlanValue, outputDiff["before"])
		assert.Equal(t, redactedPlanValue, outputDiff["after"])
	})

	t.Run("preserves unknown values as unknown markers", func(t *testing.T) {
		raw := `{
			"resource_changes": [{"address":"aws_instance.example","change":{"actions":["update"],"before":{"id":"old","nested":{"value":"old"},"list":["old"]},"after":{"id":"known-secret-that-must-not-be-used","nested":{"value":"ignored"},"list":["ignored"]},"after_unknown":{"id":true,"nested":{"value":true},"list":[true]}}}]
		}`

		result := decodeCompactPlan(t, raw, 64*1024)
		change := result["resource_changes"].([]any)[0].(map[string]any)
		assert.Equal(t, []any{"id", "list[0]", "nested.value"}, change["changed_keys"])
		diffs := change["diffs"].(map[string]any)
		for _, key := range []string{"id", "list[0]", "nested.value"} {
			assert.Equal(t, map[string]any{"unknown": true}, diffs[key].(map[string]any)["after"])
		}
		assert.NotContains(t, mustMarshalJSON(t, result), "known-secret-that-must-not-be-used")
	})

	t.Run("redacts metadata paths even when the value is omitted", func(t *testing.T) {
		raw := `{"resource_changes":[{"address":"aws_instance.example","change":{"actions":["update"],"before":{"password":"super-secret-omitted","id":"old"},"after":{"id":"new"},"before_sensitive":{"password":true},"after_unknown":{"id":true}}}]}`

		encoded, err := buildPlanDiff([]byte(raw), 64*1024)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "super-secret-omitted")
		result := decodeJSON(t, encoded)
		diffs := result["resource_changes"].([]any)[0].(map[string]any)["diffs"].(map[string]any)
		assert.Equal(t, redactedPlanValue, diffs["password"].(map[string]any)["before"])
		assert.Equal(t, map[string]any{"unknown": true}, diffs["id"].(map[string]any)["after"])
	})

	t.Run("ignores drift and projects deferred output and check status", func(t *testing.T) {
		raw := `{
			"resource_drift":[{"address":"aws_instance.drift","change":{"actions":["update"],"before":{"size":1},"after":{"size":2}}}],
			"deferred_changes":[{"reason":"instance_count_unknown","change":{"resource":{"addr":"aws_instance.deferred"},"action":"create"}}],
			"output_changes":{"endpoint":{"actions":["update"],"before":"old","after":"new"},"unchanged":{"actions":["no-op"],"before":"same","after":"same"}},
			"checks":[{"address":{"to_display":"check.example"},"status":"pass","instances":[{"status":"pass"},{"status":"fail"}]}]
		}`

		result := decodeCompactPlan(t, raw, 64*1024)
		assert.NotContains(t, result["summary"].(map[string]any), "drift")
		assert.NotContains(t, result, "resource_drift")
		assert.Len(t, result["deferred_changes"], 1)
		deferred := result["deferred_changes"].([]any)[0].(map[string]any)
		assert.Equal(t, "instance_count_unknown", deferred["reason"])
		assert.Equal(t, "aws_instance.deferred", deferred["address"])
		assert.Contains(t, result["output_changes"].(map[string]any), "endpoint")
		assert.NotContains(t, result["output_changes"].(map[string]any), "unchanged")
		assert.Equal(t, "pass", result["checks"].([]any)[0].(map[string]any)["status"])
	})

	t.Run("ignores malformed drift without affecting output", func(t *testing.T) {
		raw := `{
			"resource_drift":{"unexpected":"shape"},
			"resource_changes":[{"address":"aws_instance.example","change":{"actions":["update"],"before":{"size":1},"after":{"size":2}}}]
		}`

		result := decodeCompactPlan(t, raw, 64*1024)

		assert.NotContains(t, result, "resource_drift")
		assert.NotContains(t, result["summary"].(map[string]any), "drift")
		assert.Len(t, result["resource_changes"], 1)
	})
}

func TestBuildPlanDiffValidation(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "malformed JSON", raw: `{"`},
		{name: "top-level array", raw: `[]`},
		{name: "resource changes object", raw: `{"resource_changes":{}}`},
		{name: "actions are not an array", raw: `{"resource_changes":[{"change":{"actions":"update"}}]}`},
		{name: "sensitivity metadata has unsupported value", raw: `{"resource_changes":[{"change":{"actions":["update"],"before_sensitive":"secret"}}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildPlanDiff([]byte(tt.raw), 64*1024)
			require.Error(t, err)
		})
	}
}

func TestBuildPlanDiffSupportsCurrentPlanFormatVersionField(t *testing.T) {
	result := decodeCompactPlan(t, `{"plan_format_version":"1.1","resource_changes":[]}`, 64*1024)
	assert.Equal(t, "1.1", result["source_format_version"])
}

func TestBuildPlanDiffOutputBudget(t *testing.T) {
	plan := map[string]any{
		"format_version":    "1.2",
		"terraform_version": "1.15.8",
		"resource_changes":  []any{},
	}
	for index := 0; index < 20; index++ {
		plan["resource_changes"] = append(plan["resource_changes"].([]any), map[string]any{
			"address": "aws_instance.example[" + strconv.Itoa(index) + "]",
			"change": map[string]any{
				"actions": []any{"update"},
				"before":  map[string]any{"value": strings.Repeat("before", 3)},
				"after":   map[string]any{"value": strings.Repeat("after", 3)},
			},
		})
	}
	raw, err := json.Marshal(plan)
	require.NoError(t, err)

	const maxBytes = 1200
	encoded, err := buildPlanDiff(raw, maxBytes)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(encoded), maxBytes)
	result := decodeJSON(t, encoded)
	assert.True(t, result["truncated"].(bool))
	truncation := result["truncation"].(map[string]any)
	assert.Greater(t, truncation["omitted"].(map[string]any)["resource_changes"], float64(0))
}

func TestConfiguredPlanDiffMaxBytes(t *testing.T) {
	t.Setenv(planDiffMaxBytesEnv, "1234")
	assert.Equal(t, 1234, configuredPlanDiffMaxBytes())

	t.Setenv(planDiffMaxBytesEnv, "not-a-number")
	assert.Equal(t, defaultPlanDiffMaxByte, configuredPlanDiffMaxBytes())

	t.Setenv(planDiffMaxBytesEnv, "0")
	assert.Equal(t, defaultPlanDiffMaxByte, configuredPlanDiffMaxBytes())
}

func decodeCompactPlan(t *testing.T, raw string, maxBytes int) map[string]any {
	t.Helper()
	encoded, err := buildPlanDiff([]byte(raw), maxBytes)
	require.NoError(t, err)
	return decodeJSON(t, encoded)
}

func decodeJSON(t *testing.T, encoded []byte) map[string]any {
	t.Helper()
	var result map[string]any
	require.NoError(t, json.Unmarshal(encoded, &result))
	return result
}

func mustMarshalJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}
