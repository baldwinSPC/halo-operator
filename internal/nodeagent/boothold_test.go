// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package nodeagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The measured RAH-001 shape, kernel 6.18.44+rex+5-amd64.
const (
	measuredCmdline = " root=UUID=643b044c-0000-0000-0000-000000000000 splash quiet loglevel=3 "
	measuredOptions = "options     root=UUID=643b044c-0000-0000-0000-000000000000 splash quiet loglevel=3\n"
)

func espWithEntries(t *testing.T, n int) (etcRoot, espRoot string) {
	t.Helper()
	etcRoot, espRoot = t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(etcRoot, "kernel"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etcRoot, "kernel", "cmdline"), []byte(measuredCmdline), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := filepath.Join(espRoot, "loader", "entries")
	if err := os.MkdirAll(entries, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		name := filepath.Join(entries, "amd-ryzen-ai-developer-platform-6.18.44.conf")
		if i > 0 {
			name = filepath.Join(entries, "amd-ryzen-ai-developer-platform-6.18.44-fallback.conf")
		}
		body := "title   AMD Ryzen AI\nversion 6.18.44\n" + measuredOptions
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return etcRoot, espRoot
}

func TestCollectBootHold_ReadsTheMeasuredShape(t *testing.T) {
	etcRoot, espRoot := espWithEntries(t, 2)
	got := collectBootHold(etcRoot, espRoot)

	if got.EtcKernelCmdline == nil {
		t.Fatalf("EtcKernelCmdline is nil; Unreadable = %v", got.Unreadable)
	}
	// VERBATIM. The measured file carries a leading and a trailing space, and
	// trimming at the collector loses a fact the reader cannot recover.
	if *got.EtcKernelCmdline != measuredCmdline {
		t.Errorf("EtcKernelCmdline = %q, want it byte-for-byte including the surrounding spaces", *got.EtcKernelCmdline)
	}
	if got.LoaderEntriesListed == nil || !*got.LoaderEntriesListed {
		t.Fatalf("LoaderEntriesListed = %v, want true", got.LoaderEntriesListed)
	}
	if got.LoaderEntriesFound == nil || *got.LoaderEntriesFound != 2 {
		t.Fatalf("LoaderEntriesFound = %v, want 2", got.LoaderEntriesFound)
	}
	if len(got.LoaderEntries) != 2 {
		t.Fatalf("LoaderEntries = %d, want 2", len(got.LoaderEntries))
	}
	for _, e := range got.LoaderEntries {
		if !strings.Contains(e.Options, "splash") {
			t.Errorf("%s options = %q, want the token; 90-loaderentry.install writes SEVERAL spaces "+
				"after the key, so a single-space split would miss it", e.Path, e.Options)
		}
	}
}

// A directory that cannot be listed. The fixture puts a regular FILE where the
// directory should be, so ReadDir fails with ENOTDIR WHATEVER UID THE SUITE
// RUNS AS.
//
// A chmod-based fixture would be inert under root, and this suite can run as
// root — so the permission case would pass by never being exercised, which is
// the one failure no widening of the assertion fixes.
func TestCollectBootHold_AnUnlistableDirectoryIsNotAnEmptyOne(t *testing.T) {
	etcRoot, espRoot := espWithEntries(t, 0)
	entries := filepath.Join(espRoot, "loader", "entries")
	if err := os.RemoveAll(entries); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entries, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := collectBootHold(etcRoot, espRoot)
	if got.LoaderEntriesListed == nil || *got.LoaderEntriesListed {
		t.Fatalf("LoaderEntriesListed = %v, want false — the listing FAILED and that is not an empty directory",
			got.LoaderEntriesListed)
	}
	if len(got.Unreadable) == 0 {
		t.Error("Unreadable is empty; a listing that failed must say so or the decision has nothing to refuse on")
	}
	if got.LoaderEntriesFound != nil {
		t.Errorf("LoaderEntriesFound = %v, want nil — reporting a count for a listing that never happened "+
			"is the zero that reads as a measurement", got.LoaderEntriesFound)
	}
}

// The autofs shape: the listing SUCCEEDS and returns nothing.
func TestCollectBootHold_AnEmptyDirectoryListsSuccessfullyWithZeroFound(t *testing.T) {
	etcRoot, espRoot := espWithEntries(t, 0)
	got := collectBootHold(etcRoot, espRoot)

	if got.LoaderEntriesListed == nil || !*got.LoaderEntriesListed {
		t.Fatalf("LoaderEntriesListed = %v, want true — the directory was readable", got.LoaderEntriesListed)
	}
	if got.LoaderEntriesFound == nil || *got.LoaderEntriesFound != 0 {
		t.Fatalf("LoaderEntriesFound = %v, want 0", got.LoaderEntriesFound)
	}
	// The distinction this whole type exists for: listed-and-empty is a
	// different answer from could-not-list, and only the decision package may
	// turn either into a verdict.
	if len(got.Unreadable) != 0 {
		t.Errorf("Unreadable = %v, want none — nothing failed here", got.Unreadable)
	}
}

// An entry that exists and cannot be opened. A dangling symlink fails ReadFile
// with ENOENT regardless of uid, for the same reason the ENOTDIR fixture above
// is used instead of a mode.
func TestCollectBootHold_AnUnopenableEntryIsCountedAndNamedNotSkipped(t *testing.T) {
	etcRoot, espRoot := espWithEntries(t, 1)
	entries := filepath.Join(espRoot, "loader", "entries")
	dangling := filepath.Join(entries, "ghost.conf")
	if err := os.Symlink(filepath.Join(entries, "does-not-exist"), dangling); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got := collectBootHold(etcRoot, espRoot)
	if got.LoaderEntriesFound == nil || *got.LoaderEntriesFound != 2 {
		t.Fatalf("LoaderEntriesFound = %v, want 2 — the unopenable entry EXISTS and must be counted, "+
			"or a file carrying the token vanishes from the tally", got.LoaderEntriesFound)
	}
	if len(got.LoaderEntries) != 1 {
		t.Errorf("LoaderEntries = %d, want 1 — only the readable entry has contents", len(got.LoaderEntries))
	}
	var named bool
	for _, u := range got.Unreadable {
		if strings.Contains(u, "ghost.conf") {
			named = true
		}
	}
	if !named {
		t.Errorf("Unreadable = %v, want it to name ghost.conf", got.Unreadable)
	}
}

func TestCollectBootHold_AMissingCmdlineIsNamedNotSilentlyEmpty(t *testing.T) {
	etcRoot, espRoot := espWithEntries(t, 1)
	if err := os.Remove(filepath.Join(etcRoot, "kernel", "cmdline")); err != nil {
		t.Fatal(err)
	}
	got := collectBootHold(etcRoot, espRoot)
	if got.EtcKernelCmdline != nil {
		t.Fatalf("EtcKernelCmdline = %q, want nil", *got.EtcKernelCmdline)
	}
	if len(got.Unreadable) == 0 {
		t.Error("Unreadable is empty; an absent file must be reported, not read as a file with no token")
	}
}

func TestOptionsLine(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"the measured multi-space form", "title x\n" + measuredOptions, "root=UUID=643b044c-0000-0000-0000-000000000000 splash quiet loglevel=3"},
		{"no options line at all", "title x\nversion 1\n", ""},
		{"a key that merely starts with options", "optionsfoo bar\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := optionsLine(c.body); got != c.want {
				t.Errorf("optionsLine = %q, want %q", got, c.want)
			}
		})
	}
}
