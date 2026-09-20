// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package boothold

import (
	"strings"
	"testing"
)

func ptr(b bool) *bool { return &b }

// The measured shape of RAH-001 (kernel 6.18.44+rex+5-amd64): /etc/kernel/cmdline
// plus two Boot Loader Specification type#1 entries written by
// 90-loaderentry.install.
func measuredFiles(held bool) []FileReading {
	return []FileReading{
		{Path: "/etc/kernel/cmdline", HasToken: held},
		{Path: "/efi/loader/entries/amd-ryzen-ai-developer-platform-6.18.44.conf", HasToken: held},
		{Path: "/efi/loader/entries/amd-ryzen-ai-developer-platform-6.18.44-fallback.conf", HasToken: held},
	}
}

// measuredObs is the RAH-001 shape with the ESP successfully listed: two BLS
// entries, which is what `sudo sh -c 'for f in /efi/loader/entries/*.conf'`
// returns on that unit. Every fixture must state the ESP listing, because a
// zero there is the autofs trap rather than a clean unit.
func measuredObs(held bool, kernelHeld bool) Observation {
	return Observation{
		Node:                  "amd-halo",
		RunningKernelHasToken: ptr(kernelHeld),
		Files:                 measuredFiles(held),
		LoaderEntriesListed:   true,
		LoaderEntriesFound:    2,
	}
}

func TestDecide_ClearedNeedsCleanFilesAndACleanRunningKernel(t *testing.T) {
	got := Decide(measuredObs(false, false))
	if got.Verdict != Cleared {
		t.Fatalf("Verdict = %v, want Cleared (%+v)", got.Verdict, got)
	}
}

func TestDecide_AFileStillCarryingTheTokenIsHeldAndNamesIt(t *testing.T) {
	obs := measuredObs(false, false)
	obs.Files[1].HasToken = true
	files := obs.Files
	got := Decide(obs)
	if got.Verdict != Held {
		t.Fatalf("Verdict = %v, want Held", got.Verdict)
	}
	if len(got.HoldingFiles) != 1 || got.HoldingFiles[0] != files[1].Path {
		t.Errorf("HoldingFiles = %v, want exactly %q — a reader who is not told WHICH file has to go looking",
			got.HoldingFiles, files[1].Path)
	}
}

// Clean files under a kernel that still carries the token. Glimmer's scan calls
// this "staged": the edit landed and the unit has not restarted into it.
func TestDecide_CleanFilesUnderACarryingKernelIsRebootPendingNotCleared(t *testing.T) {
	got := Decide(measuredObs(false, true))
	if got.Verdict != RebootPending {
		t.Fatalf("Verdict = %v, want RebootPending", got.Verdict)
	}
	if got.Reason != "RebootPending" {
		t.Errorf("Reason = %q, want RebootPending", got.Reason)
	}
}

// THE ONE THAT MATTERS. Reading /efi/loader/entries needs root, and an
// unprivileged agent gets "permission denied". Every file it COULD read is
// clean, so a decision that ignored the unread one would report Cleared — and
// the unread file is exactly where the token would be.
func TestDecide_AnUnreadableFileIsNeverAFileWithoutTheToken(t *testing.T) {
	got := Decide(Observation{
		Node:                  "amd-halo",
		RunningKernelHasToken: ptr(false),
		Files:                 []FileReading{{Path: "/etc/kernel/cmdline", HasToken: false}},
		Unreadable:            []string{"/efi/loader/entries/amd-ryzen-ai-developer-platform-6.18.44.conf: permission denied"},
		LoaderEntriesListed:   true,
		LoaderEntriesFound:    2,
	})
	if got.Verdict != NotObserved {
		t.Fatalf("Verdict = %v, want NotObserved — every READABLE file was clean, which is exactly "+
			"the state that tempts a Cleared verdict off a partial view", got.Verdict)
	}
	if got.HoldingFiles != nil {
		t.Errorf("HoldingFiles = %v, want none: NotObserved names no file as holding", got.HoldingFiles)
	}
}

func TestDecide_AnUnreadableRunningKernelIsNotObserved(t *testing.T) {
	obs := measuredObs(false, false)
	obs.RunningKernelHasToken = nil
	got := Decide(obs)
	if got.Verdict != NotObserved {
		t.Fatalf("Verdict = %v, want NotObserved", got.Verdict)
	}
}

// An agent that reported no sources at all is not a clean node.
func TestDecide_NoFilesReportedIsNotObservedRatherThanCleared(t *testing.T) {
	obs := measuredObs(false, false)
	obs.Files = nil
	got := Decide(obs)
	if got.Verdict != NotObserved {
		t.Fatalf("Verdict = %v, want NotObserved — no file was read, so nothing supports Cleared", got.Verdict)
	}
}

// Every verdict must be reachable. A decision function that can only ever
// return two of its four values is a narrower instrument than it looks, and
// nothing else in this file would notice.
func TestDecide_EveryVerdictIsReachable(t *testing.T) {
	seen := map[Verdict]bool{}
	autofs := measuredObs(false, false)
	autofs.LoaderEntriesFound = 0
	for _, obs := range []Observation{
		measuredObs(false, false),
		measuredObs(true, false),
		measuredObs(false, true),
		autofs,
	} {
		seen[Decide(obs).Verdict] = true
	}
	for _, want := range []Verdict{Cleared, Held, RebootPending, NotObserved} {
		if !seen[want] {
			t.Errorf("verdict %v was not reachable from any fixture", want)
		}
	}
}

// Every non-Cleared verdict must say something a human can act on. A blank
// message turns a False condition into a puzzle.
func TestDecide_EveryVerdictCarriesAMessage(t *testing.T) {
	autofs := measuredObs(false, false)
	autofs.LoaderEntriesFound = 0
	for _, obs := range []Observation{
		measuredObs(false, false),
		measuredObs(true, false),
		measuredObs(false, true),
		autofs,
		{},
	} {
		got := Decide(obs)
		if got.Message == "" || got.Reason == "" {
			t.Errorf("Decide(%+v) = %+v, want a non-empty Reason and Message", obs, got)
		}
	}
}

// The autofs trap, measured on RAH-001 by the AMD parity lane. /efi is an
// autofs mount; a container that has not triggered it sees an EMPTY directory.
// No error and no permission denial — the listing simply returns nothing, on a
// unit whose two entries both carry the token.
//
// A zero here must NOT reach Cleared. That would be True on the condition for a
// unit that holds every boot: the wrong value, not a missing one.
func TestDecide_AnUntriggeredAutofsMountIsNotObservedRatherThanCleared(t *testing.T) {
	obs := measuredObs(false, false)
	obs.Files = []FileReading{{Path: "/etc/kernel/cmdline", HasToken: false}}
	obs.LoaderEntriesFound = 0

	got := Decide(obs)
	if got.Verdict != NotObserved {
		t.Fatalf("Verdict = %v, want NotObserved — an empty loader-entries listing on a booted BLS "+
			"unit is an untriggered mount, and Cleared here reports True for a unit that holds every boot", got.Verdict)
	}
}

// The same zero by the other route: the glob is expanded by the UNPRIVILEGED
// shell before sudo runs, /efi/loader/entries is mode 0700, so it matches
// nothing. `sudo grep -l splash /efi/loader/entries/*.conf | wc -l` returns 0
// on a unit where the correct answer is 2.
func TestDecide_AListingThatFailedIsNotAnEmptyOne(t *testing.T) {
	obs := measuredObs(false, false)
	obs.Files = []FileReading{{Path: "/etc/kernel/cmdline", HasToken: false}}
	obs.LoaderEntriesListed = false
	obs.LoaderEntriesFound = 0

	got := Decide(obs)
	if got.Verdict != NotObserved {
		t.Fatalf("Verdict = %v, want NotObserved", got.Verdict)
	}

	// THE VERDICT ALONE CANNOT SEE THIS GUARD. A failed listing reports zero
	// entries, so the zero-entry guard behind it returns NotObserved too and
	// the verdict is right for the wrong reason. Measured: removing the
	// listing guard alone reddens NOTHING until this assertion exists.
	//
	// The reason is what differs, and it is what a human acts on — "I could not
	// list the directory" sends someone to permissions, "the listing came back
	// empty" sends them to the autofs mount. Blaming the mount for a failed
	// listing is a true verdict with a false explanation.
	if !strings.Contains(got.Message, "could not be listed") {
		t.Errorf("Message = %q, want it to name a FAILED listing; reporting this as an empty "+
			"listing sends the reader to the autofs mount for a permissions problem", got.Message)
	}
}

func res(node string, v Verdict) NodeResult {
	return NodeResult{Node: node, Result: Result{Verdict: v, Reason: "r", Message: "m"}}
}

func TestSummarize_EveryNodeClearedIsCleared(t *testing.T) {
	got := Summarize([]NodeResult{res("a", Cleared), res("b", Cleared)})
	if got.Verdict != Cleared {
		t.Fatalf("Verdict = %v, want Cleared", got.Verdict)
	}
}

// THE PROPERTY. Cleared is about the fleet the config targets, and an unread
// node is not a cleared node. One of them denies True.
func TestSummarize_OneUnreadNodeDeniesCleared(t *testing.T) {
	got := Summarize([]NodeResult{res("a", Cleared), res("b", NotObserved)})
	if got.Verdict != NotObserved {
		t.Fatalf("Verdict = %v, want NotObserved — every node that COULD be read was clear, "+
			"which is exactly the state that tempts a fleet-wide True", got.Verdict)
	}
	if !strings.Contains(got.Message, "b") {
		t.Errorf("Message = %q, want it to name the unread node", got.Message)
	}
}

// A definite problem outranks an unknown, because it is actionable now — but
// the unknown must not vanish, or the reader thinks the rest were fine.
func TestSummarize_AHeldNodeOutranksAnUnreadOneAndNeitherIsLost(t *testing.T) {
	got := Summarize([]NodeResult{res("a", Held), res("b", NotObserved), res("c", Cleared)})
	if got.Verdict != Held {
		t.Fatalf("Verdict = %v, want Held", got.Verdict)
	}
	if !strings.Contains(got.Message, "a") {
		t.Errorf("Message = %q, want it to name the holding node", got.Message)
	}
	if !strings.Contains(got.Message, "b") {
		t.Errorf("Message = %q, want it to STILL mention the unread node — a reader who sees only "+
			"the holding node assumes the others were checked and clear", got.Message)
	}
}

func TestSummarize_RebootPendingWhenNothingIsHeld(t *testing.T) {
	got := Summarize([]NodeResult{res("a", RebootPending), res("b", Cleared)})
	if got.Verdict != RebootPending {
		t.Fatalf("Verdict = %v, want RebootPending", got.Verdict)
	}
}

func TestSummarize_NoNodesIsNotObservedRatherThanCleared(t *testing.T) {
	if got := Summarize(nil); got.Verdict != Cleared {
		if got.Verdict != NotObserved {
			t.Fatalf("Verdict = %v, want NotObserved", got.Verdict)
		}
	} else {
		t.Fatal("Verdict = Cleared for an empty fleet — nothing was observed, so nothing is cleared")
	}
}

func TestSummarize_EveryVerdictIsReachable(t *testing.T) {
	seen := map[Verdict]bool{}
	for _, in := range [][]NodeResult{
		{res("a", Cleared)},
		{res("a", Held)},
		{res("a", RebootPending)},
		{res("a", NotObserved)},
	} {
		seen[Summarize(in).Verdict] = true
	}
	for _, want := range []Verdict{Cleared, Held, RebootPending, NotObserved} {
		if !seen[want] {
			t.Errorf("verdict %v unreachable from any fleet fixture", want)
		}
	}
}
