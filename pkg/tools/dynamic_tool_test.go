// Copyright IBM Corp. 2025
// SPDX-License-Identifier: MPL-2.0

package tools

import (
	"io"
	"os"
	"testing"

	"github.com/hashicorp/terraform-mcp-server/pkg/toolsets"
	"github.com/mark3labs/mcp-go/server"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

func TestPlanToolRegistration(t *testing.T) {
	logger := log.New()
	logger.SetOutput(io.Discard)
	mcpServer := server.NewMCPServer("test", "test")

	registerDynamicTools(mcpServer, logger, []string{toolsets.Terraform})
	GetDynamicToolRegistry().RegisterSessionWithTFE("test-session")

	registeredTools := mcpServer.ListTools()
	assert.Contains(t, registeredTools, "get_plan_changes")
	assert.NotContains(t, registeredTools, "get_plan_json_output")
}

func TestIsTerraformOperationsEnabled(t *testing.T) {
	// Save original env var
	originalValue := os.Getenv("ENABLE_TF_OPERATIONS")
	defer os.Setenv("ENABLE_TF_OPERATIONS", originalValue)

	tests := []struct {
		name     string
		envValue string
		expected bool
	}{
		{"unset", "", false},
		{"false", "false", false},
		{"true", "true", true},
		{"TRUE", "TRUE", true},
		{"True", "True", true},
		{"invalid", "invalid", false},
		{"1", "1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue == "" {
				os.Unsetenv("ENABLE_TF_OPERATIONS")
			} else {
				os.Setenv("ENABLE_TF_OPERATIONS", tt.envValue)
			}
			assert.Equal(t, tt.expected, isTerraformOperationsEnabled())
		})
	}
}
