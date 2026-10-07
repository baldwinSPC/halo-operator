// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package nodeagent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BootHoldReading is the persisted boot configuration as the agent found it.
//
// It mirrors the API type rather than importing it, the same way Reading does:
// the collector's output is a plain Go value and the publisher lifts it.
type BootHoldReading struct {
	EtcKernelCmdline    *string
	LoaderEntriesListed *bool
	LoaderEntriesFound  *int32
	LoaderEntries       []LoaderEntry
	Unreadable          []string
}

// LoaderEntry is one Boot Loader Specification type#1 entry.
type LoaderEntry struct {
	Path    string
	Options string
}

// collectBootHold reads the two persisted sources that decide the NEXT boot.
//
// The running kernel's own command line is collected separately; this is only
// what is on disk.
//
// os.ReadDir, NEVER filepath.Glob. Glob returns the same empty slice and a nil
// error for a directory that is missing, one that cannot be entered, and one
// that is genuinely empty — so the caller cannot tell a failure from a clean
// result, and the reassuring reading wins. Measured on RAH-001 this matters
// twice over: /efi is an autofs mount a container can see as empty until it is
// triggered, and /efi/loader/entries is mode 0700.
func collectBootHold(etcRoot, espRoot string) *BootHoldReading {
	out := &BootHoldReading{}

	cmdlinePath := filepath.Join(etcRoot, "kernel", "cmdline")
	if raw, err := os.ReadFile(cmdlinePath); err != nil {
		out.Unreadable = append(out.Unreadable, cmdlinePath+": "+err.Error())
	} else {
		// Stored VERBATIM. Measured on RAH-001 this file carries a leading and
		// a trailing space; trimming here would lose a fact the reader may
		// need and cannot recover.
		s := string(raw)
		out.EtcKernelCmdline = &s
	}

	entriesDir := filepath.Join(espRoot, "loader", "entries")
	ents, err := os.ReadDir(entriesDir)
	if err != nil {
		listed := false
		out.LoaderEntriesListed = &listed
		out.Unreadable = append(out.Unreadable, entriesDir+": "+err.Error())
		return out
	}
	listed := true
	out.LoaderEntriesListed = &listed

	var found int32
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
			continue
		}
		found++
		p := filepath.Join(entriesDir, e.Name())
		raw, err := os.ReadFile(p)
		if err != nil {
			// Counted in found and absent from LoaderEntries: the entry exists
			// and was not read, which is neither "no entry" nor "an entry
			// without the token".
			out.Unreadable = append(out.Unreadable, p+": "+err.Error())
			continue
		}
		out.LoaderEntries = append(out.LoaderEntries, LoaderEntry{Path: p, Options: optionsLine(string(raw))})
	}
	out.LoaderEntriesFound = &found
	return out
}

// optionsLine returns the entry's `options` value, or "" when it has none.
//
// A type#1 entry is a line-oriented key/value file; the key is separated from
// the value by whitespace, and 90-loaderentry.install writes several spaces.
// An entry with no options line is a real state — it simply sets no kernel
// arguments — and is reported as an empty value rather than as unreadable.
func optionsLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 1 && fields[0] == "options" {
			return strings.Join(fields[1:], " ")
		}
	}
	return ""
}

// bootHoldMessages says, for a human, what could not be read.
func bootHoldMessages(r *BootHoldReading) []string {
	if r == nil {
		return nil
	}
	var msgs []string
	for _, u := range r.Unreadable {
		msgs = append(msgs, fmt.Sprintf("boot hold: %s", u))
	}
	return msgs
}
