// Copyright IBM Corp. 2025
// SPDX-License-Identifier: MPL-2.0

package tools

import (
	"context"

	"github.com/hashicorp/terraform-mcp-server/pkg/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	log "github.com/sirupsen/logrus"
)

// GetPlanChanges creates a tool that returns only actionable, compact changes
// from a Terraform plan. Sensitive values are redacted before the result is
// built and the raw Terraform plan is never returned to the MCP client.
func GetPlanChanges(logger *log.Logger) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("get_plan_changes",
			mcp.WithDescription(`Retrieves a compact JSON summary of actionable changes in a Terraform plan. No-op resources and Terraform plan bulk sections are omitted. Sensitive values are redacted, and the response is bounded by TF_PLAN_DIFF_MAX_BYTES.`),
			mcp.WithTitleAnnotation("Get compact changes for a Terraform plan"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithString("plan_id",
				mcp.Required(),
				mcp.Description("The ID of the plan to get compact changes for"),
			),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return getPlanChangesHandler(ctx, req, logger)
		},
	}
}

func getPlanChangesHandler(ctx context.Context, request mcp.CallToolRequest, logger *log.Logger) (*mcp.CallToolResult, error) {
	planID, err := request.RequireString("plan_id")
	if err != nil {
		return ToolError(logger, "missing required input: plan_id", err)
	}

	tfeClient, err := client.GetTfeClientFromContext(ctx, logger)
	if err != nil {
		return ToolError(logger, "failed to get Terraform client", err)
	}

	planJSON, err := tfeClient.Plans.ReadJSONOutput(ctx, planID)
	if err != nil {
		return ToolErrorf(logger, "failed to retrieve plan JSON output: %s", planID)
	}

	compactJSON, err := buildPlanDiff(planJSON, configuredPlanDiffMaxBytes())
	if err != nil {
		return ToolError(logger, "failed to build compact Terraform plan changes", err)
	}
	return mcp.NewToolResultText(string(compactJSON)), nil
}
