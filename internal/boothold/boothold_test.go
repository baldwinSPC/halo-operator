// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package boothold

import "testing"

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

func TestDecide_ClearedNeedsCleanFilesAndACleanRunningKernel(t *testing.T) {
	got := Decide(Observation{
		Node:                  "amd-halo",
		RunningKernelHasToken: ptr(false),
		Files:                 measuredFiles(false),
	})
	if got.Verdict != Cleared {
		t.Fatalf("Verdict = %v, want Cleared (%+v)", got.Verdict, got)
	}
}

func TestDecide_AFileStillCarryingTheTokenIsHeldAndNamesIt(t *testing.T) {
	files := measuredFiles(false)
	files[1].HasToken = true
	got := Decide(Observation{
		Node:                  "amd-halo",
		RunningKernelHasToken: ptr(false),
		Files:                 files,
	})
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
	got := Decide(Observation{
		Node:                  "amd-halo",
		RunningKernelHasToken: ptr(true),
		Files:                 measuredFiles(false),
	})
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
	got := Decide(Observation{Node: "amd-halo", Files: measuredFiles(false)})
	if got.Verdict != NotObserved {
		t.Fatalf("Verdict = %v, want NotObserved", got.Verdict)
	}
}

// An agent that reported no sources at all is not a clean node.
func TestDecide_NoFilesReportedIsNotObservedRatherThanCleared(t *testing.T) {
	got := Decide(Observation{Node: "amd-halo", RunningKernelHasToken: ptr(false)})
	if got.Verdict != NotObserved {
		t.Fatalf("Verdict = %v, want NotObserved — no file was read, so nothing supports Cleared", got.Verdict)
	}
}

// Every verdict must be reachable. A decision function that can only ever
// return two of its four values is a narrower instrument than it looks, and
// nothing else in this file would notice.
func TestDecide_EveryVerdictIsReachable(t *testing.T) {
	seen := map[Verdict]bool{}
	for _, obs := range []Observation{
		{RunningKernelHasToken: ptr(false), Files: measuredFiles(false)},
		{RunningKernelHasToken: ptr(false), Files: measuredFiles(true)},
		{RunningKernelHasToken: ptr(true), Files: measuredFiles(false)},
		{RunningKernelHasToken: ptr(false), Files: measuredFiles(false), Unreadable: []string{"x: permission denied"}},
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
	for _, obs := range []Observation{
		{RunningKernelHasToken: ptr(false), Files: measuredFiles(false)},
		{RunningKernelHasToken: ptr(false), Files: measuredFiles(true)},
		{RunningKernelHasToken: ptr(true), Files: measuredFiles(false)},
		{RunningKernelHasToken: ptr(false), Files: measuredFiles(false), Unreadable: []string{"x: permission denied"}},
		{},
	} {
		got := Decide(obs)
		if got.Message == "" || got.Reason == "" {
			t.Errorf("Decide(%+v) = %+v, want a non-empty Reason and Message", obs, got)
		}
	}
}
