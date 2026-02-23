// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package decoder

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/nomad-pack/internal/pkg/errors/packdiags"
	"github.com/hashicorp/nomad-pack/internal/pkg/variable/internal/hclhelp"
	"github.com/hashicorp/nomad-pack/internal/pkg/variable/schema"
	"github.com/hashicorp/nomad-pack/sdk/pack/variables"
	"github.com/hashicorp/nomad/jobspec2/addrs"
	"github.com/zclconf/go-cty/cty"
)

// DecodeVariableBlock parses a variable definition into a variable. When the
// provided block or its Body is nil, the function returns (nil, nil)
func DecodeVariableBlock(block *hcl.Block) (*variables.Variable, hcl.Diagnostics) {
	if block == nil || block.Body == nil {
		return nil, hcl.Diagnostics{}
	}

	// If block and Body is non-nil, then the block is ready to be parsed
	content, diags := block.Body.Content(schema.VariableBlockSchema)
	if content == nil {
		return nil, diags
	}

	if diags == nil {
		diags = hcl.Diagnostics{}
	}

	v := &variables.Variable{
		Name:      variables.ID(block.Labels[0]),
		DeclRange: block.DefRange,
	}

	// Ensure the variable name is valid. If this isn't checked it will cause
	// problems in future use.
	if !hclsyntax.ValidIdentifier(v.Name.String()) {
		diags = diags.Append(packdiags.DiagInvalidVariableName(v.DeclRange.Ptr()))
	}

	// A variable doesn't need to declare a description. If it does, process
	// this and store it, along with any processing errors.
	if attr, exists := content.Attributes[schema.VariableAttributeDescription]; exists {
		val, descDiags := attr.Expr.Value(nil)
		diags = packdiags.SafeDiagnosticsExtend(diags, descDiags)

		if val.Type() == cty.String {
			v.SetDescription(val.AsString())
		} else {
			diags = packdiags.SafeDiagnosticsAppend(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid type for description",
				Detail: fmt.Sprintf("The description attribute is expected to be of type string, got %s",
					val.Type().FriendlyName()),
				Subject: attr.Range.Ptr(),
			})
		}
	}

	// A variable doesn't need to declare a type. If it does, process this and
	// store it, along with any processing errors.
	if attr, exists := content.Attributes[schema.VariableAttributeType]; exists {
		ty, tyDiags := typeexpr.Type(attr.Expr)
		diags = packdiags.SafeDiagnosticsExtend(diags, tyDiags)
		v.SetType(ty)
	}

	// A variable doesn't need to declare a default. If it does, process this
	// and store it, along with any processing errors.
	if attr, exists := content.Attributes[schema.VariableAttributeDefault]; exists {
		val, valDiags := attr.Expr.Value(nil)
		diags = packdiags.SafeDiagnosticsExtend(diags, valDiags)

		// If the found type isn't cty.NilType, then attempt to covert the
		// default variable, so we know they are compatible.
		if v.Type != cty.NilType {
			var err *hcl.Diagnostic
			val, err = hclhelp.ConvertValUsingType(val, v.Type, attr.Expr.Range().Ptr())
			diags = packdiags.SafeDiagnosticsAppend(diags, err)
		}
		v.SetDefault(val)
		v.Value = val
	}

	for _, block := range content.Blocks {
		switch block.Type {
		case "validation":
			vv, moreDiags := decodeVariableValidationBlock(string(v.Name), block)
			diags = append(diags, moreDiags...)
			v.Validations = append(v.Validations, vv)
		}
	}

	if diags.HasErrors() {
		return nil, diags
	}

	return v, diags
}

func decodeVariableValidationBlock(varName string, block *hcl.Block) (*variables.VariableValidation, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	vv := &variables.VariableValidation{
		DeclRange: block.DefRange,
	}

	content, moreDiags := block.Body.Content(schema.VariableValidationBlockSchema)
	diags = append(diags, moreDiags...)

	if attr, exists := content.Attributes["condition"]; exists {
		vv.Condition = attr.Expr

		// The validation condition must refer to the variable itself and
		// nothing else; to ensure that the variable declaration can't create
		// additional edges in the dependency graph.
		goodRefs := 0
		for _, traversal := range vv.Condition.Variables() {

			ref, moreDiags := addrs.ParseRef(traversal)
			if !moreDiags.HasErrors() {
				if addr, ok := ref.Subject.(addrs.InputVariable); ok {
					if addr.Name == varName {
						goodRefs++
						continue // Reference is valid
					}
				}
			}

			// If we fall out here then the reference is invalid.
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid reference in variable validation",
				Detail:   fmt.Sprintf("The condition for variable %q can only refer to the variable itself, using var.%s.", varName, varName),
				Subject:  traversal.SourceRange().Ptr(),
			})
		}
		if goodRefs < 1 {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid variable validation condition",
				Detail:   fmt.Sprintf("The condition for variable %q must refer to var.%s in order to test incoming values.", varName, varName),
				Subject:  attr.Expr.Range().Ptr(),
			})
		}
	}

	if attr, exists := content.Attributes["error_message"]; exists {
		moreDiags := gohcl.DecodeExpression(attr.Expr, nil, &vv.ErrorMessage)
		diags = append(diags, moreDiags...)
		if !moreDiags.HasErrors() {
			const errSummary = "Invalid validation error message"
			switch {
			case vv.ErrorMessage == "":
				diags = diags.Append(&hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  errSummary,
					Detail:   "An empty string is not a valid nor useful error message.",
					Subject:  attr.Expr.Range().Ptr(),
				})
			case !looksLikeSentences(vv.ErrorMessage):
				// Because we're going to include this string verbatim as part
				// of a bigger error message written in our usual style, we'll
				// require the given error message to conform to that. We might
				// relax this in future if e.g. we start presenting these error
				// messages in a different way, or if Packer starts supporting
				// producing error messages in other human languages, etc. For
				// pragmatism we also allow sentences ending with exclamation
				// points, but we don't mention it explicitly here because
				// that's not really consistent with the Packer UI writing
				// style.
				diags = diags.Append(&hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  errSummary,
					Detail:   "Validation error message must be at least one full sentence starting with an uppercase letter ( if the alphabet permits it ) and ending with a period or question mark.",
					Subject:  attr.Expr.Range().Ptr(),
				})
			}
		}
	}

	return vv, diags
}

// looksLikeSentence is a simple heuristic that encourages writing error
// messages that will be presentable when included as part of a larger error
// diagnostic whose other text is written in the UI writing style.
//
// This is intentionally not a very strong validation since we're assuming that
// authors want to write good messages and might just need a nudge about
// Packer's specific style, rather than that they are going to try to work
// around these rules to write a lower-quality message.
func looksLikeSentences(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 1 {
		return false
	}
	runes := []rune(s) // HCL guarantees that all strings are valid UTF-8
	first := runes[0]
	last := runes[len(runes)-1]

	// If the first rune is a letter then it must be an uppercase letter. To
	// sorts of nudge people into writting sentences. For alphabets that don't
	// have the notion of 'upper', this does nothing.
	if unicode.IsLetter(first) && !unicode.IsUpper(first) {
		return false
	}

	// The string must be at least one full sentence, which implies having
	// sentence-ending punctuation.
	return last == '.' || last == '?' || last == '!'
}
