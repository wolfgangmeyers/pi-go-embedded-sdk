package ai

import (
	"fmt"
)

// ValidateToolArguments validates (and coerces) tool-call arguments against the
// tool's schema, returning validated arguments or a value-free error.
func ValidateToolArguments(tool Tool, toolCall ToolCall) (map[string]any, error) {
	if tool.Parameters == nil {
		return toolCall.Arguments, nil
	}
	// Deep-copy the arguments so coercion does not mutate the original.
	args, _ := deepCopy(toolCall.Arguments).(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	normalizeOptionalNulls(args, tool.Parameters)

	coerced := tool.Parameters.Coerce(args)
	if obj, ok := coerced.(map[string]any); ok {
		args = obj
	}

	if !tool.Parameters.Check(args) {
		// Names, paths, schema text and argument values can all originate from
		// untrusted registrations or calls. Do not reflect any of them here.
		return nil, fmt.Errorf("Tool arguments failed schema validation")
	}
	return args, nil
}

// normalizeOptionalNulls treats null as omission: strict constrained sampling
// forces the model to emit every property, so optional properties come back as
// explicit nulls. A null value for a present property is deleted when the
// property is not required and its schema does not accept null (port of
// normalizeOptionalNulls, utils/validation.ts).
func normalizeOptionalNulls(value any, schema *Schema) {
	if arr, ok := value.([]any); ok {
		// pi also walks per-index tuple items here; ai.Schema cannot represent
		// tuple items, so only the single-schema branch exists.
		if schema.Items != nil {
			for _, item := range arr {
				normalizeOptionalNulls(item, schema.Items)
			}
		}
		return
	}
	obj, ok := value.(map[string]any)
	if !ok || schema.Properties == nil {
		return
	}

	required := make(map[string]bool, len(schema.Required))
	for _, key := range schema.Required {
		required[key] = true
	}
	for key, propertySchema := range schema.Properties {
		v, present := obj[key]
		if !present || propertySchema == nil {
			continue
		}
		// pi skips $ref properties (their compiled validator may not resolve)
		// and otherwise asks the compiled sub-validator whether null passes;
		// the Go schema validates structurally, so Check(nil) plays that role.
		_, refIsString := propertySchema.Extra["$ref"].(string)
		if v == nil && !required[key] && !refIsString && !propertySchema.Check(nil) {
			delete(obj, key)
		} else {
			normalizeOptionalNulls(v, propertySchema)
		}
	}
}

// ValidateToolCall finds a tool by name and validates the call's arguments.
func ValidateToolCall(tools []Tool, toolCall ToolCall) (map[string]any, error) {
	for _, t := range tools {
		if t.Name == toolCall.Name {
			return ValidateToolArguments(t, toolCall)
		}
	}
	return nil, fmt.Errorf("Tool unavailable")
}
