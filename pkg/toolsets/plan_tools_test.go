// Copyright IBM Corp. 2025
// SPDX-License-Identifier: MPL-2.0

package toolsets

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPlanToolMapping(t *testing.T) {
	assert.Equal(t, Terraform, ToolToToolset["get_plan_changes"])
	_, rawPlanJSONOutputEnabled := ToolToToolset["get_plan_json_output"]
	assert.False(t, rawPlanJSONOutputEnabled)

	validTools := GetAllValidToolNames()
	assert.True(t, validTools["get_plan_changes"])
	assert.False(t, validTools["get_plan_json_output"])

	valid, invalid := ParseIndividualTools([]string{"get_plan_changes", "get_plan_json_output"})
	assert.Equal(t, []string{"get_plan_changes"}, valid)
	assert.Equal(t, []string{"get_plan_json_output"}, invalid)
}
