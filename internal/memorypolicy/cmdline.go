// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

// Package memorypolicy decides whether a node's unified-memory configuration
// matches what the fleet declared. Every decision in this package is a pure
// function of an observation: nothing here reads a file, calls a kernel
// interface, or needs a GPU, so all of it is exercised in CI on hardware that
// has never seen an APU.
//
// The package never mutates a host. Applying kernel arguments is the Glimmer
// signed-task path; this operator only ever reports what it found.
package memorypolicy

import "strings"

// ParseKernelCmdline turns the contents of /proc/cmdline into the parameters
// it sets.
//
// The kernel command line is a space-separated list of parameters, each either
// a bare flag or a key=value pair. A bare flag is recorded with an empty
// value, so a caller can tell "amd_iommu was not on the command line at all"
// from "amd_iommu was present with no value" — those mean different things when
// checking a policy.
//
// Values may be quoted, as in `foo="a b"`. A quoted value keeps its spaces and
// loses its quotes. A later occurrence of a parameter wins, which is what the
// kernel itself does.
func ParseKernelCmdline(cmdline string) map[string]string {
	params := map[string]string{}
	for _, field := range splitCmdline(cmdline) {
		if field == "" {
			continue
		}
		key, value, found := strings.Cut(field, "=")
		if !found {
			params[key] = ""
			continue
		}
		params[key] = strings.Trim(value, `"`)
	}
	return params
}

// splitCmdline splits on whitespace, except inside double quotes.
func splitCmdline(cmdline string) []string {
	var (
		fields  []string
		current strings.Builder
		quoted  bool
	)
	flush := func() {
		if current.Len() > 0 {
			fields = append(fields, current.String())
			current.Reset()
		}
	}
	for _, r := range cmdline {
		switch {
		case r == '"':
			quoted = !quoted
			current.WriteRune(r)
		case !quoted && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return fields
}
