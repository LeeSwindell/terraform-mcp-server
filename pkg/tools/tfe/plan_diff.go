// Copyright IBM Corp. 2025
// SPDX-License-Identifier: MPL-2.0

package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

const (
	planDiffSchemaVersion  = 1
	planDiffMaxBytesEnv    = "TF_PLAN_DIFF_MAX_BYTES"
	defaultPlanDiffMaxByte = 64 * 1024
	redactedPlanValue      = "[REDACTED]"
)

// planDiff is the compact representation of Terraform's JSON
// plan format. Keeping only this representation at the MCP seam makes it
// difficult to accidentally add a raw-plan fallback later.
type planDiff struct {
	SchemaVersion       int                   `json:"schema_version"`
	SourceFormatVersion string                `json:"source_format_version"`
	TerraformVersion    string                `json:"terraform_version"`
	Summary             planDiffSummary       `json:"summary"`
	ResourceChanges     []resourcePlanDiff    `json:"resource_changes"`
	DeferredChanges     []deferredPlanDiff    `json:"deferred_changes"`
	OutputChanges       map[string]outputDiff `json:"output_changes"`
	Checks              []planCheck           `json:"checks,omitempty"`
	Truncated           bool                  `json:"truncated"`
	Truncation          *planTruncation       `json:"truncation,omitempty"`
}

type planDiffSummary struct {
	Create  int `json:"create"`
	Update  int `json:"update"`
	Delete  int `json:"delete"`
	Replace int `json:"replace"`
}

type resourcePlanDiff struct {
	Address       string               `json:"address"`
	ModuleAddress string               `json:"module_address,omitempty"`
	Actions       []string             `json:"actions"`
	ChangedKeys   []string             `json:"changed_keys"`
	Diffs         map[string]fieldDiff `json:"diffs"`
}

type outputDiff struct {
	Actions     []string             `json:"actions"`
	ChangedKeys []string             `json:"changed_keys"`
	Diffs       map[string]fieldDiff `json:"diffs"`
}

type deferredPlanDiff struct {
	Address     string               `json:"address,omitempty"`
	Actions     []string             `json:"actions"`
	Reason      string               `json:"reason,omitempty"`
	ChangedKeys []string             `json:"changed_keys"`
	Diffs       map[string]fieldDiff `json:"diffs"`
}

type planCheck struct {
	Address          string   `json:"address,omitempty"`
	Status           string   `json:"status,omitempty"`
	InstanceStatuses []string `json:"instance_statuses,omitempty"`
}

type planDiffCounts struct {
	ResourceChanges int `json:"resource_changes"`
	DeferredChanges int `json:"deferred_changes"`
	OutputChanges   int `json:"output_changes"`
	DiffFields      int `json:"diff_fields"`
}

type planTruncation struct {
	MaxBytes int            `json:"max_bytes"`
	Included planDiffCounts `json:"included"`
	Omitted  planDiffCounts `json:"omitted"`
}

// fieldDiff contains only values that have already been classified as safe to
// return. Its custom marshaler handles the two non-value states without ever
// serializing the corresponding Terraform value.
type fieldDiff struct {
	before          any
	beforeSet       bool
	beforeSensitive bool
	beforeUnknown   bool
	after           any
	afterSet        bool
	afterSensitive  bool
	afterUnknown    bool
	sensitive       bool
}

func (d fieldDiff) MarshalJSON() ([]byte, error) {
	result := make(map[string]any, 3)
	if d.beforeSet {
		result["before"] = safeDiffValue(d.before, d.beforeSensitive, d.beforeUnknown)
	}
	if d.afterSet {
		result["after"] = safeDiffValue(d.after, d.afterSensitive, d.afterUnknown)
	}
	if d.sensitive {
		result["sensitive"] = true
	}
	return json.Marshal(result)
}

func safeDiffValue(value any, sensitive, unknown bool) any {
	if sensitive {
		return redactedPlanValue
	}
	if unknown {
		return map[string]bool{"unknown": true}
	}
	return value
}

type planValueState struct {
	value     any
	exists    bool
	sensitive bool
	unknown   bool
}

// buildPlanDiff decodes and projects one Terraform JSON plan. The raw bytes
// are never logged or returned; only the compact representation is marshaled.
func buildPlanDiff(raw []byte, maxBytes int) ([]byte, error) {
	planJSON, err := decodePlanJSON(raw)
	if err != nil {
		return nil, err
	}

	result, err := projectPlan(planJSON)
	if err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = defaultPlanDiffMaxByte
	}

	return marshalPlanDiffWithinLimit(result, maxBytes)
}

func decodePlanJSON(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode Terraform plan JSON: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode Terraform plan JSON: trailing data")
		}
		return nil, fmt.Errorf("decode Terraform plan JSON: %w", err)
	}

	plan, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Terraform plan JSON must be an object")
	}
	return plan, nil
}

func projectPlan(plan map[string]any) (*planDiff, error) {
	formatVersion, err := optionalStringField(plan, "format_version")
	if err != nil {
		return nil, err
	}
	if formatVersion == "" {
		formatVersion, err = optionalStringField(plan, "plan_format_version")
		if err != nil {
			return nil, err
		}
	}
	terraformVersion, err := optionalStringField(plan, "terraform_version")
	if err != nil {
		return nil, err
	}

	result := &planDiff{
		SchemaVersion:       planDiffSchemaVersion,
		SourceFormatVersion: formatVersion,
		TerraformVersion:    terraformVersion,
		ResourceChanges:     []resourcePlanDiff{},
		DeferredChanges:     []deferredPlanDiff{},
		OutputChanges:       map[string]outputDiff{},
	}

	resourceChanges, err := arrayField(plan, "resource_changes")
	if err != nil {
		return nil, err
	}
	for _, rawChange := range resourceChanges {
		resourceChange, include, err := projectResourceChange(rawChange)
		if err != nil {
			return nil, err
		}
		if !include {
			continue
		}
		result.ResourceChanges = append(result.ResourceChanges, resourceChange)
		addSummary(&result.Summary, resourceChange.Actions)
	}

	deferredChanges, err := arrayField(plan, "deferred_changes")
	if err != nil {
		return nil, err
	}
	for _, rawChange := range deferredChanges {
		deferredChange, include, err := projectDeferredChange(rawChange)
		if err != nil {
			return nil, err
		}
		if include {
			result.DeferredChanges = append(result.DeferredChanges, deferredChange)
		}
	}

	outputChanges, err := objectField(plan, "output_changes")
	if err != nil {
		return nil, err
	}
	for name, rawChange := range outputChanges {
		change, ok := rawChange.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("output_changes.%s must be an object", name)
		}
		actions, err := actionField(change)
		if err != nil {
			return nil, fmt.Errorf("output_changes.%s: %w", name, err)
		}
		if !isActionable(actions) {
			continue
		}
		diffs, err := diffsForChange(change, actionRequiresCompleteDiff(actions))
		if err != nil {
			return nil, fmt.Errorf("output_changes.%s: %w", name, err)
		}
		result.OutputChanges[name] = outputDiff{
			Actions:     actions,
			ChangedKeys: sortedDiffKeys(diffs),
			Diffs:       diffs,
		}
	}

	checks, err := arrayField(plan, "checks")
	if err != nil {
		return nil, err
	}
	result.Checks, err = projectChecks(checks)
	if err != nil {
		return nil, err
	}

	sortResourceDiffs(result.ResourceChanges)
	sort.SliceStable(result.DeferredChanges, func(i, j int) bool {
		if result.DeferredChanges[i].Address != result.DeferredChanges[j].Address {
			return result.DeferredChanges[i].Address < result.DeferredChanges[j].Address
		}
		return result.DeferredChanges[i].Reason < result.DeferredChanges[j].Reason
	})
	sort.SliceStable(result.Checks, func(i, j int) bool {
		return result.Checks[i].Address < result.Checks[j].Address
	})

	return result, nil
}

func projectResourceChange(raw any) (resourcePlanDiff, bool, error) {
	entry, ok := raw.(map[string]any)
	if !ok {
		return resourcePlanDiff{}, false, fmt.Errorf("resource change must be an object")
	}
	change, ok := entry["change"].(map[string]any)
	if !ok {
		return resourcePlanDiff{}, false, fmt.Errorf("resource change.change must be an object")
	}

	actions, err := actionField(change)
	if err != nil {
		return resourcePlanDiff{}, false, err
	}
	if !isActionable(actions) {
		return resourcePlanDiff{}, false, nil
	}

	address, err := optionalStringField(entry, "address")
	if err != nil {
		return resourcePlanDiff{}, false, err
	}
	moduleAddress, err := optionalStringField(entry, "module_address")
	if err != nil {
		return resourcePlanDiff{}, false, err
	}
	diffs, err := diffsForChange(change, actionRequiresCompleteDiff(actions))
	if err != nil {
		return resourcePlanDiff{}, false, err
	}

	return resourcePlanDiff{
		Address:       address,
		ModuleAddress: moduleAddress,
		Actions:       actions,
		ChangedKeys:   sortedDiffKeys(diffs),
		Diffs:         diffs,
	}, true, nil
}

func projectDeferredChange(raw any) (deferredPlanDiff, bool, error) {
	entry, ok := raw.(map[string]any)
	if !ok {
		return deferredPlanDiff{}, false, fmt.Errorf("deferred change must be an object")
	}

	reason, err := optionalStringField(entry, "reason")
	if err != nil {
		return deferredPlanDiff{}, false, err
	}

	change, ok := entry["change"].(map[string]any)
	if !ok {
		return deferredPlanDiff{}, false, fmt.Errorf("deferred change.change must be an object")
	}

	// Terraform has represented deferred changes with both a normal
	// resource-change shape and a compact resource/action shape. Accept both,
	// while projecting only address, actions, and safe value differences.
	resourceChange := change
	if nested, ok := change["resource_change"].(map[string]any); ok {
		resourceChange = nested
	}
	if nested, ok := change["change"].(map[string]any); ok {
		resourceChange = nested
	}

	actions, err := actionField(resourceChange)
	if err != nil {
		if action, ok := change["action"].(string); ok {
			actions = []string{action}
		} else {
			return deferredPlanDiff{}, false, err
		}
	}
	if !isActionable(actions) {
		return deferredPlanDiff{}, false, nil
	}

	address, err := deferredAddress(entry, change, resourceChange)
	if err != nil {
		return deferredPlanDiff{}, false, err
	}
	diffs, err := diffsForChange(resourceChange, actionRequiresCompleteDiff(actions))
	if err != nil {
		return deferredPlanDiff{}, false, err
	}

	return deferredPlanDiff{
		Address:     address,
		Actions:     actions,
		Reason:      reason,
		ChangedKeys: sortedDiffKeys(diffs),
		Diffs:       diffs,
	}, true, nil
}

func deferredAddress(entry, change, resourceChange map[string]any) (string, error) {
	for _, object := range []map[string]any{resourceChange, change, entry} {
		for _, field := range []string{"address", "addr"} {
			if value, exists := object[field]; exists {
				address, ok := value.(string)
				if !ok {
					return "", fmt.Errorf("deferred change.%s must be a string", field)
				}
				return address, nil
			}
		}
	}
	if resource, ok := change["resource"].(map[string]any); ok {
		for _, field := range []string{"address", "addr"} {
			if value, exists := resource[field]; exists {
				address, ok := value.(string)
				if !ok {
					return "", fmt.Errorf("deferred change.resource.%s must be a string", field)
				}
				return address, nil
			}
		}
	}
	return "", nil
}

func projectChecks(raw []any) ([]planCheck, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	checks := make([]planCheck, 0, len(raw))
	for _, rawCheck := range raw {
		check, ok := rawCheck.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("check must be an object")
		}
		address, err := compactAddressField(check, "address")
		if err != nil {
			return nil, err
		}
		status, err := optionalStringField(check, "status")
		if err != nil {
			return nil, err
		}

		statuses := []string{}
		instances, err := arrayValue(check["instances"], "check.instances")
		if err != nil {
			return nil, err
		}
		for _, rawInstance := range instances {
			instance, ok := rawInstance.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("check.instances item must be an object")
			}
			instanceStatus, err := optionalStringField(instance, "status")
			if err != nil {
				return nil, err
			}
			if instanceStatus != "" {
				statuses = append(statuses, instanceStatus)
			}
		}
		checks = append(checks, planCheck{Address: address, Status: status, InstanceStatuses: statuses})
	}
	return checks, nil
}

func diffsForChange(change map[string]any, force bool) (map[string]fieldDiff, error) {
	for _, field := range []string{"before_sensitive", "after_sensitive", "before_unknown", "after_unknown"} {
		if value, exists := change[field]; exists {
			if err := validateMetadataTree(value, field); err != nil {
				return nil, err
			}
		}
	}

	before, beforeExists := change["before"]
	after, afterExists := change["after"]
	diffs := make(map[string]fieldDiff)
	collectDiffs(
		"",
		planValueState{value: before, exists: beforeExists},
		planValueState{value: after, exists: afterExists},
		change["before_sensitive"],
		change["after_sensitive"],
		change["before_unknown"],
		change["after_unknown"],
		force,
		diffs,
	)
	return diffs, nil
}

func collectDiffs(path string, before, after planValueState, beforeSensitive, afterSensitive, beforeUnknown, afterUnknown any, force bool, diffs map[string]fieldDiff) {
	before.sensitive = metadataMarked(beforeSensitive)
	after.sensitive = metadataMarked(afterSensitive)
	before.unknown = metadataMarked(beforeUnknown)
	after.unknown = metadataMarked(afterUnknown)
	before.exists = before.exists || before.sensitive || before.unknown
	after.exists = after.exists || after.sensitive || after.unknown
	if before.sensitive || after.sensitive || before.unknown || after.unknown {
		if force || before.unknown || after.unknown || !equalPlanValues(before, after) {
			addFieldDiff(path, before, after, diffs)
		}
		return
	}

	beforeMap, beforeIsMap := before.value.(map[string]any)
	afterMap, afterIsMap := after.value.(map[string]any)
	if beforeIsMap && afterIsMap {
		keys := unionMapKeys(beforeMap, afterMap, beforeSensitive, afterSensitive, beforeUnknown, afterUnknown)
		if len(keys) == 0 {
			if force && !equalPlanValues(before, after) {
				addFieldDiff(path, before, after, diffs)
			}
			return
		}
		for _, key := range keys {
			beforeValue, beforeExists := beforeMap[key]
			afterValue, afterExists := afterMap[key]
			collectDiffs(
				joinObjectPath(path, key),
				planValueState{value: beforeValue, exists: beforeExists},
				planValueState{value: afterValue, exists: afterExists},
				metadataChild(beforeSensitive, key),
				metadataChild(afterSensitive, key),
				metadataChild(beforeUnknown, key),
				metadataChild(afterUnknown, key),
				force,
				diffs,
			)
		}
		return
	}

	beforeList, beforeIsList := before.value.([]any)
	afterList, afterIsList := after.value.([]any)
	if beforeIsList && afterIsList {
		length := maxInt(len(beforeList), len(afterList), metadataLength(beforeSensitive), metadataLength(afterSensitive), metadataLength(beforeUnknown), metadataLength(afterUnknown))
		if length == 0 {
			if force && !equalPlanValues(before, after) {
				addFieldDiff(path, before, after, diffs)
			}
			return
		}
		for index := 0; index < length; index++ {
			var beforeValue, afterValue any
			beforeExists := index < len(beforeList)
			afterExists := index < len(afterList)
			if beforeExists {
				beforeValue = beforeList[index]
			}
			if afterExists {
				afterValue = afterList[index]
			}
			collectDiffs(
				joinArrayPath(path, index),
				planValueState{value: beforeValue, exists: beforeExists},
				planValueState{value: afterValue, exists: afterExists},
				metadataChild(beforeSensitive, index),
				metadataChild(afterSensitive, index),
				metadataChild(beforeUnknown, index),
				metadataChild(afterUnknown, index),
				force,
				diffs,
			)
		}
		return
	}

	// When an object/list is created or deleted, expand it into leaf paths
	// instead of returning the entire container. This keeps the response
	// compact and gives nested sensitivity metadata a chance to redact values.
	if beforeIsMap || beforeIsList || afterIsMap || afterIsList {
		if beforeIsMap {
			for _, key := range unionMapKeys(beforeMap, nil, beforeSensitive, nil, beforeUnknown, nil) {
				beforeValue, beforeExists := beforeMap[key]
				collectDiffs(
					joinObjectPath(path, key),
					planValueState{value: beforeValue, exists: beforeExists},
					planValueState{},
					metadataChild(beforeSensitive, key), nil,
					metadataChild(beforeUnknown, key), nil,
					force, diffs,
				)
			}
		}
		if beforeIsList {
			length := maxInt(len(beforeList), metadataLength(beforeSensitive), metadataLength(beforeUnknown))
			for index := 0; index < length; index++ {
				var value any
				exists := index < len(beforeList)
				if exists {
					value = beforeList[index]
				}
				collectDiffs(
					joinArrayPath(path, index),
					planValueState{value: value, exists: exists}, planValueState{},
					metadataChild(beforeSensitive, index), nil,
					metadataChild(beforeUnknown, index), nil,
					force, diffs,
				)
			}
		}
		if afterIsMap {
			for _, key := range unionMapKeys(nil, afterMap, nil, afterSensitive, nil, afterUnknown) {
				afterValue, afterExists := afterMap[key]
				collectDiffs(
					joinObjectPath(path, key), planValueState{},
					planValueState{value: afterValue, exists: afterExists},
					nil, metadataChild(afterSensitive, key),
					nil, metadataChild(afterUnknown, key),
					force, diffs,
				)
			}
		}
		if afterIsList {
			length := maxInt(len(afterList), metadataLength(afterSensitive), metadataLength(afterUnknown))
			for index := 0; index < length; index++ {
				var value any
				exists := index < len(afterList)
				if exists {
					value = afterList[index]
				}
				collectDiffs(
					joinArrayPath(path, index), planValueState{},
					planValueState{value: value, exists: exists},
					nil, metadataChild(afterSensitive, index),
					nil, metadataChild(afterUnknown, index),
					force, diffs,
				)
			}
		}

		// Empty containers have no leaf path to report. The root marker is safe
		// here because both values are empty containers, not arbitrary objects.
		if len(diffs) == 0 && (isEmptyContainer(before.value) || isEmptyContainer(after.value)) && !equalPlanValues(before, after) {
			addFieldDiff(path, before, after, diffs)
		}
		return
	}

	if force || !equalPlanValues(before, after) {
		addFieldDiff(path, before, after, diffs)
	}
}

func addFieldDiff(path string, before, after planValueState, diffs map[string]fieldDiff) {
	if !before.exists && !after.exists {
		return
	}
	if path == "" {
		path = "$"
	}
	diffs[path] = fieldDiff{
		before:          before.value,
		beforeSet:       before.exists,
		beforeSensitive: before.sensitive,
		beforeUnknown:   before.unknown,
		after:           after.value,
		afterSet:        after.exists,
		afterSensitive:  after.sensitive,
		afterUnknown:    after.unknown,
		sensitive:       before.sensitive || after.sensitive,
	}
}

func equalPlanValues(before, after planValueState) bool {
	return before.exists == after.exists && (!before.exists || reflect.DeepEqual(before.value, after.value)) && before.unknown == after.unknown
}

func actionRequiresCompleteDiff(actions []string) bool {
	return hasAction(actions, "create") || hasAction(actions, "delete")
}

func addSummary(summary *planDiffSummary, actions []string) {
	if hasAction(actions, "create") && hasAction(actions, "delete") {
		summary.Replace++
		return
	}
	if hasAction(actions, "create") {
		summary.Create++
	}
	if hasAction(actions, "update") {
		summary.Update++
	}
	if hasAction(actions, "delete") {
		summary.Delete++
	}
}

func actionField(change map[string]any) ([]string, error) {
	value, exists := change["actions"]
	if !exists {
		return nil, fmt.Errorf("change.actions is required")
	}
	actions, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("change.actions must be an array")
	}
	result := make([]string, 0, len(actions))
	for _, rawAction := range actions {
		action, ok := rawAction.(string)
		if !ok {
			return nil, fmt.Errorf("change.actions items must be strings")
		}
		result = append(result, action)
	}
	return result, nil
}

func hasAction(actions []string, wanted string) bool {
	for _, action := range actions {
		if action == wanted {
			return true
		}
	}
	return false
}

func isActionable(actions []string) bool {
	for _, action := range actions {
		if action != "no-op" && action != "read" {
			return true
		}
	}
	return false
}

func optionalStringField(object map[string]any, field string) (string, error) {
	value, exists := object[field]
	if !exists || value == nil {
		return "", nil
	}
	stringValue, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", field)
	}
	return stringValue, nil
}

func compactAddressField(object map[string]any, field string) (string, error) {
	value, exists := object[field]
	if !exists || value == nil {
		return "", nil
	}
	if address, ok := value.(string); ok {
		return address, nil
	}
	if structured, ok := value.(map[string]any); ok {
		if display, ok := structured["to_display"].(string); ok {
			return display, nil
		}
		if name, ok := structured["name"].(string); ok {
			return name, nil
		}
		return "", nil
	}
	return "", fmt.Errorf("%s must be a string or address object", field)
}

func objectField(object map[string]any, field string) (map[string]any, error) {
	value, exists := object[field]
	if !exists || value == nil {
		return map[string]any{}, nil
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", field)
	}
	return result, nil
}

func arrayField(object map[string]any, field string) ([]any, error) {
	value, exists := object[field]
	if !exists || value == nil {
		return []any{}, nil
	}
	return arrayValue(value, field)
}

func arrayValue(value any, field string) ([]any, error) {
	if value == nil {
		return []any{}, nil
	}
	result, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", field)
	}
	return result, nil
}

func validateMetadataTree(value any, field string) error {
	switch typed := value.(type) {
	case nil, bool:
		return nil
	case map[string]any:
		for key, child := range typed {
			if err := validateMetadataTree(child, field+"."+key); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for index, child := range typed {
			if err := validateMetadataTree(child, fmt.Sprintf("%s[%d]", field, index)); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%s must contain only booleans, objects, arrays, or nulls", field)
	}
}

func metadataMarked(value any) bool {
	marked, ok := value.(bool)
	return ok && marked
}

func metadataChild(value any, key any) any {
	switch typed := value.(type) {
	case bool:
		if typed {
			return true
		}
	case map[string]any:
		if stringKey, ok := key.(string); ok {
			return typed[stringKey]
		}
	case []any:
		if index, ok := key.(int); ok && index >= 0 && index < len(typed) {
			return typed[index]
		}
	}
	return nil
}

func metadataLength(value any) int {
	if typed, ok := value.([]any); ok {
		return len(typed)
	}
	return 0
}

func unionMapKeys(objects ...any) []string {
	keys := map[string]struct{}{}
	for _, object := range objects {
		if typed, ok := object.(map[string]any); ok {
			for key := range typed {
				keys[key] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func maxInt(values ...int) int {
	maximum := 0
	for _, value := range values {
		if value > maximum {
			maximum = value
		}
	}
	return maximum
}

func isEmptyContainer(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		return len(typed) == 0
	case []any:
		return len(typed) == 0
	default:
		return false
	}
}

func joinObjectPath(parent, key string) string {
	if parent == "" {
		return key
	}
	if isSimplePathKey(key) {
		return parent + "." + key
	}
	encoded, _ := json.Marshal(key)
	return parent + "[" + string(encoded) + "]"
}

func joinArrayPath(parent string, index int) string {
	return parent + "[" + strconv.Itoa(index) + "]"
}

func isSimplePathKey(key string) bool {
	if key == "" {
		return false
	}
	for index, character := range key {
		if index == 0 {
			if character != '_' && (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') {
				return false
			}
			continue
		}
		if character != '_' && (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func sortedDiffKeys(diffs map[string]fieldDiff) []string {
	keys := make([]string, 0, len(diffs))
	for key := range diffs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortResourceDiffs(changes []resourcePlanDiff) {
	sort.SliceStable(changes, func(i, j int) bool {
		return changes[i].Address < changes[j].Address
	})
}

func configuredPlanDiffMaxBytes() int {
	value := strings.TrimSpace(os.Getenv(planDiffMaxBytesEnv))
	if value == "" {
		return defaultPlanDiffMaxByte
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return defaultPlanDiffMaxByte
	}
	return parsed
}

func marshalPlanDiffWithinLimit(result *planDiff, maxBytes int) ([]byte, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal compact Terraform plan: %w", err)
	}
	if len(encoded) <= maxBytes {
		return encoded, nil
	}

	total := planDiffCountsFor(result)
	result.Truncated = true
	for {
		current := planDiffCountsFor(result)
		result.Truncation = &planTruncation{
			MaxBytes: maxBytes,
			Included: current,
			Omitted:  subtractPlanDiffCounts(total, current),
		}
		encoded, err = json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("marshal truncated Terraform plan: %w", err)
		}
		if len(encoded) <= maxBytes {
			return encoded, nil
		}
		if !removeOneDiff(result) && !removeOnePlanEntry(result) {
			return minimalPlanDiff(maxBytes)
		}
	}
}

func planDiffCountsFor(result *planDiff) planDiffCounts {
	counts := planDiffCounts{
		ResourceChanges: len(result.ResourceChanges),
		DeferredChanges: len(result.DeferredChanges),
		OutputChanges:   len(result.OutputChanges),
	}
	for _, change := range result.ResourceChanges {
		counts.DiffFields += len(change.Diffs)
	}
	for _, change := range result.DeferredChanges {
		counts.DiffFields += len(change.Diffs)
	}
	for _, change := range result.OutputChanges {
		counts.DiffFields += len(change.Diffs)
	}
	return counts
}

func subtractPlanDiffCounts(total, current planDiffCounts) planDiffCounts {
	return planDiffCounts{
		ResourceChanges: maxInt(total.ResourceChanges-current.ResourceChanges, 0),
		DeferredChanges: maxInt(total.DeferredChanges-current.DeferredChanges, 0),
		OutputChanges:   maxInt(total.OutputChanges-current.OutputChanges, 0),
		DiffFields:      maxInt(total.DiffFields-current.DiffFields, 0),
	}
}

func removeOneDiff(result *planDiff) bool {
	for index := len(result.ResourceChanges) - 1; index >= 0; index-- {
		if removeLastDiff(result.ResourceChanges[index].Diffs) {
			result.ResourceChanges[index].ChangedKeys = sortedDiffKeys(result.ResourceChanges[index].Diffs)
			return true
		}
	}
	for index := len(result.DeferredChanges) - 1; index >= 0; index-- {
		if removeLastDiff(result.DeferredChanges[index].Diffs) {
			result.DeferredChanges[index].ChangedKeys = sortedDiffKeys(result.DeferredChanges[index].Diffs)
			return true
		}
	}
	keys := sortedOutputKeys(result.OutputChanges)
	for index := len(keys) - 1; index >= 0; index-- {
		change := result.OutputChanges[keys[index]]
		if removeLastDiff(change.Diffs) {
			change.ChangedKeys = sortedDiffKeys(change.Diffs)
			result.OutputChanges[keys[index]] = change
			return true
		}
	}
	return false
}

func removeLastDiff(diffs map[string]fieldDiff) bool {
	keys := sortedDiffKeys(diffs)
	if len(keys) == 0 {
		return false
	}
	delete(diffs, keys[len(keys)-1])
	return true
}

func removeOnePlanEntry(result *planDiff) bool {
	// Keep the main resource changes ahead of secondary details when the
	// budget is tight, but remove complete entries rather than returning JSON
	// fragments.
	if keys := sortedOutputKeys(result.OutputChanges); len(keys) > 0 {
		delete(result.OutputChanges, keys[len(keys)-1])
		return true
	}
	if len(result.DeferredChanges) > 0 {
		result.DeferredChanges = result.DeferredChanges[:len(result.DeferredChanges)-1]
		return true
	}
	if len(result.ResourceChanges) > 0 {
		result.ResourceChanges = result.ResourceChanges[:len(result.ResourceChanges)-1]
		return true
	}
	if len(result.Checks) > 0 {
		result.Checks = result.Checks[:len(result.Checks)-1]
		return true
	}
	return false
}

func sortedOutputKeys(changes map[string]outputDiff) []string {
	keys := make([]string, 0, len(changes))
	for key := range changes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func minimalPlanDiff(maxBytes int) ([]byte, error) {
	for _, candidate := range [][]byte{
		[]byte(`{"schema_version":1,"truncated":true}`),
		[]byte(`{"truncated":true}`),
		[]byte(`{}`),
	} {
		if len(candidate) <= maxBytes {
			return candidate, nil
		}
	}
	return nil, fmt.Errorf("compact Terraform plan cannot fit within %d bytes", maxBytes)
}
