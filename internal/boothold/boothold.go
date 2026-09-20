// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

// Package boothold decides whether a node still carries the boot hold — the
// `splash` token that makes a headless unit wait for a display it does not
// have.
//
// Every decision here is a pure function of an observation: nothing in this
// package reads a file, calls a kernel interface, or needs a GPU, so all of it
// runs in CI on hardware that has never seen an APU.
//
// The package never mutates a host, and neither does anything that calls it.
// Removing `splash` from the kernel command line and from each loader entry is
// Glimmer's signed-task path (RESOLVE_HALO_SETUP, glimmer#3173 §4); this
// operator only ever reports what it found. That split is the repo boundary and
// not a temporary division of labour.
package boothold

import (
	"fmt"
	"sort"
	"strings"
)

// HoldToken is the kernel parameter that holds a boot for a display.
const HoldToken = "splash"

// FileReading is one file the agent managed to read, and whether the hold was
// in it.
//
// Path is reported back to a human verbatim, so it carries the path the agent
// actually opened rather than a category.
type FileReading struct {
	Path     string
	HasToken bool
}

// Observation is what one node's agent read.
//
// Every field distinguishes "read it, the token was absent" from "could not
// read it", because those are different answers and only one of them is good
// news. RunningKernelHasToken is a pointer for that reason, and Unreadable
// names the files that could not be opened at all.
type Observation struct {
	Node string

	// RunningKernelHasToken is the token's presence on /proc/cmdline. Nil when
	// the agent could not read it.
	RunningKernelHasToken *bool

	// Files are the persisted sources: /etc/kernel/cmdline and each
	// /efi/loader/entries/*.conf options line. A node with none of them read
	// leaves this empty and names them in Unreadable.
	Files []FileReading

	// Unreadable names every path the agent could not read, with the reason.
	// Reading /efi/loader/entries needs root; an unprivileged read gets
	// "permission denied", which is NOT evidence that the entries are clean.
	Unreadable []string
}

// Verdict is what an observation supports.
//
// There are four because there are two ways to be not-cleared and they call for
// different actions: Held needs the files edited, RebootPending needs only a
// reboot. Both are False on the condition — the unit is still holding boots
// either way — and the reason distinguishes them. Glimmer's scan calls
// RebootPending "staged".
type Verdict int

const (
	// NotObserved means the answer is unknown. A file that could not be read
	// is never treated as a file without the token.
	NotObserved Verdict = iota
	// Cleared means no persisted file carries the token and the running kernel
	// was booted without it.
	Cleared
	// Held means at least one persisted file still carries the token, so the
	// next boot holds too.
	Held
	// RebootPending means every persisted file is clean and the running kernel
	// still carries it. The fix landed; the unit has not restarted into it.
	RebootPending
)

// Result is the decision plus what a human needs to act on it.
type Result struct {
	Verdict Verdict
	// Reason is a CamelCase token suitable for a Kubernetes condition.
	Reason string
	// Message names the specific files, so the reader does not have to go
	// looking for which one carries the token.
	Message string
	// HoldingFiles are the paths that still carry the token, sorted. Empty for
	// every verdict but Held.
	HoldingFiles []string
}

// Decide reduces one node's observation to a verdict.
//
// THE ORDER MATTERS. Unreadable is tested FIRST, before any conclusion is drawn
// from the files that were read, because a partial view of the sources cannot
// support "no file carries the token" — the unread one is exactly where it
// would be. Deciding Cleared from a subset is the failure this ordering exists
// to prevent, and it is the reassuring direction.
func Decide(obs Observation) Result {
	if len(obs.Unreadable) > 0 || obs.RunningKernelHasToken == nil {
		return Result{
			Verdict: NotObserved,
			Reason:  "NotObserved",
			Message: notObservedMessage(obs),
		}
	}
	if len(obs.Files) == 0 {
		// Nothing was unreadable and nothing was read: the agent reported no
		// sources at all. That is not a clean node, it is an agent that did not
		// look, and saying Cleared here would be the same defect as trusting a
		// permission-denied read.
		return Result{
			Verdict: NotObserved,
			Reason:  "NotObserved",
			Message: "no boot configuration file was reported for this node, so whether the boot hold is set is unknown",
		}
	}

	holding := holdingFiles(obs.Files)
	if len(holding) > 0 {
		return Result{
			Verdict:      Held,
			Reason:       "BootHoldPresent",
			Message:      fmt.Sprintf("%s is still on the kernel command line in %s, so the next boot waits for a display", HoldToken, strings.Join(holding, ", ")),
			HoldingFiles: holding,
		}
	}
	if *obs.RunningKernelHasToken {
		return Result{
			Verdict: RebootPending,
			Reason:  "RebootPending",
			Message: fmt.Sprintf("every boot configuration file is clear of %s and the running kernel was booted with it, so the unit holds boots until it restarts", HoldToken),
		}
	}
	return Result{
		Verdict: Cleared,
		Reason:  "BootHoldCleared",
		Message: fmt.Sprintf("no boot configuration file carries %s and the running kernel was booted without it", HoldToken),
	}
}

func holdingFiles(files []FileReading) []string {
	var out []string
	for _, f := range files {
		if f.HasToken {
			out = append(out, f.Path)
		}
	}
	sort.Strings(out)
	return out
}

func notObservedMessage(obs Observation) string {
	var parts []string
	if obs.RunningKernelHasToken == nil {
		parts = append(parts, "the running kernel command line could not be read")
	}
	if len(obs.Unreadable) > 0 {
		u := append([]string(nil), obs.Unreadable...)
		sort.Strings(u)
		parts = append(parts, "could not read "+strings.Join(u, ", "))
	}
	return strings.Join(parts, "; ") +
		" — an unread file is not a file without " + HoldToken
}
