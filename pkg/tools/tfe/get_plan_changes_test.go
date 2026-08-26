// Copyright IBM Corp. 2025
// SPDX-License-Identifier: MPL-2.0

package tools

import (
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

func TestGetPlanChanges(t *testing.T) {
	logger := log.New()
	logger.SetLevel(log.ErrorLevel) // Reduce noise in tests

	t.Run("tool creation", func(t *testing.T) {
		tool := GetPlanChanges(logger)

		assert.Equal(t, "get_plan_changes", tool.Tool.Name)
		assert.Contains(t, tool.Tool.Description, "compact JSON")
		assert.Contains(t, tool.Tool.Description, "Sensitive values are redacted")
		assert.NotNil(t, tool.Handler)

		// Verify it's marked as read-only
		assert.NotNil(t, tool.Tool.Annotations.ReadOnlyHint)
		assert.True(t, *tool.Tool.Annotations.ReadOnlyHint)
		assert.NotNil(t, tool.Tool.Annotations.DestructiveHint)
		assert.False(t, *tool.Tool.Annotations.DestructiveHint)

		// Check required parameters are defined
		assert.Contains(t, tool.Tool.InputSchema.Required, "plan_id")
	})
}
