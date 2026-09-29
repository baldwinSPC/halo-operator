// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSystem stands in for update-initramfs and unmkinitramfs. Its initramfs
// "contents" are whatever modprobe.d held at the last successful rebuild, which
// is exactly the property Execute has to verify.
type fakeSystem struct {
	root        string
	images      []string
	baked       map[string]string // image -> content baked at last rebuild
	rebuilds    int
	failRebuild map[int]bool // fail the Nth rebuild (1-based)
	skipImage   string       // an image the rebuild "forgets" to update
	bakedPath   string       // the modprobe.d file, relative to root
}

func (f *fakeSystem) RebuildInitramfs() error {
	f.rebuilds++
	if f.failRebuild[f.rebuilds] {
		return errors.New("update-initramfs: simulated failure")
	}
	content, _ := os.ReadFile(filepath.Join(f.root, f.bakedPath))
	for _, img := range f.images {
		if img == f.skipImage {
			continue
		}
		f.baked[img] = string(content)
	}
	return nil
}

func (f *fakeSystem) InitramfsImages() ([]string, error) { return f.images, nil }

func (f *fakeSystem) ReadFromInitramfs(image, path string) (string, error) {
	if "/"+path != f.bakedPath && path != strings.TrimPrefix(f.bakedPath, "/") {
		return "", errors.New("not present in the image")
	}
	c, ok := f.baked[image]
	if !ok {
		return "", errors.New("not present in the image")
	}
	return c, nil
}

const oldTTM = "options ttm pages_limit=24692260\n"
const newTTM = "options ttm pages_limit=32505856\n"

// setup returns a root holding the real ttm.conf, a plan to change it, and a
// fake system whose two boot images currently carry the old value — the shape
// of the physical unit.
func setup(t *testing.T) (string, Plan, *fakeSystem) {
	t.Helper()
	root := fixtureRoot(t, map[string]string{"etc/modprobe.d/ttm.conf": oldTTM})
	plan := Plan{
		Params: []ParamChange{{Param: "ttm.pages_limit", Action: ActionModprobe}},
		Edits:  []FileEdit{{Path: "/etc/modprobe.d/ttm.conf", Exists: true, Current: oldTTM, Desired: newTTM}},
	}
	sys := &fakeSystem{
		root:      root,
		images:    []string{"/boot/initrd.img-6.18.44", "/efi/amd/6.18.44/initrd.img-6.18.44"},
		baked:     map[string]string{},
		bakedPath: "/etc/modprobe.d/ttm.conf",
	}
	for _, img := range sys.images {
		sys.baked[img] = oldTTM
	}
	return root, plan, sys
}

func readTTM(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "etc/modprobe.d/ttm.conf"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExecuteWritesRebuildsAndVerifiesEveryImage(t *testing.T) {
	root, plan, sys := setup(t)
	r := Execute(plan, root, sys)
	if r.ExitCode != ExitCompliant || r.RolledBack {
		t.Fatalf("exit %d rolledBack=%v err=%q", r.ExitCode, r.RolledBack, r.Error)
	}
	if got := readTTM(t, root); got != newTTM {
		t.Errorf("ttm.conf = %q, want %q", got, newTTM)
	}
	if len(r.Verified) != 2 {
		t.Errorf("verified %d images, want both the /boot and the ESP copy: %v", len(r.Verified), r.Verified)
	}
}

// Together or not at all: a failed rebuild must not leave the write in place,
// because a modprobe.d change without a rebuild lies in wait for the next
// kernel upgrade.
func TestExecuteRollsBackTheWriteWhenTheRebuildFails(t *testing.T) {
	root, plan, sys := setup(t)
	sys.failRebuild = map[int]bool{1: true}
	r := Execute(plan, root, sys)
	if !r.RolledBack || r.ExitCode != ExitSoftware {
		t.Fatalf("rolledBack=%v exit=%d, want a rollback and exit %d", r.RolledBack, r.ExitCode, ExitSoftware)
	}
	if got := readTTM(t, root); got != oldTTM {
		t.Errorf("after a failed rebuild ttm.conf = %q, want the original %q", got, oldTTM)
	}
}

// The subtle case the physical unit makes real: update-initramfs succeeds, but
// the post-update hook that copies to the EFI partition exits 0 without doing
// it (it does exactly that when bootctl is missing). The boot loader reads the
// ESP copy, so without verification this would report success while the next
// boot used the old value.
func TestExecuteCatchesAnImageTheRebuildDidNotUpdate(t *testing.T) {
	root, plan, sys := setup(t)
	sys.skipImage = "/efi/amd/6.18.44/initrd.img-6.18.44"
	r := Execute(plan, root, sys)
	if !r.RolledBack {
		t.Fatalf("a stale ESP image was reported verified; err=%q", r.Error)
	}
	if !strings.Contains(r.Error, "/efi/amd/6.18.44/initrd.img-6.18.44") {
		t.Errorf("the error does not name the stale image: %q", r.Error)
	}
	if got := readTTM(t, root); got != oldTTM {
		t.Errorf("ttm.conf = %q after rollback, want %q", got, oldTTM)
	}
	// And the rollback resynced: the images that DID take the new value were
	// rebuilt again from the restored file, so no boot path carries a change
	// nobody kept.
	if !r.Resynced {
		t.Error("the rollback did not rebuild the initramfs from the restored file")
	}
	for img, content := range sys.baked {
		if content != oldTTM {
			t.Errorf("after rollback %s still carries %q", img, content)
		}
	}
}

func TestExecuteReportsAResyncThatAlsoFails(t *testing.T) {
	root, plan, sys := setup(t)
	sys.skipImage = "/efi/amd/6.18.44/initrd.img-6.18.44"
	sys.failRebuild = map[int]bool{2: true} // the resync, not the first rebuild
	r := Execute(plan, root, sys)
	if r.Resynced {
		t.Error("a failed resync was reported as resynced")
	}
	if !strings.Contains(r.Error, "boot image may not match") {
		t.Errorf("the error does not warn that the boot image may be out of step: %q", r.Error)
	}
	if got := readTTM(t, root); got != oldTTM {
		t.Errorf("files were not restored: %q", got)
	}
}

func TestExecuteRemovesAFileItCreatedOnRollback(t *testing.T) {
	root := fixtureRoot(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "etc/modprobe.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan := Plan{
		Params: []ParamChange{{Param: "amdgpu.gttsize", Action: ActionModprobe}},
		Edits:  []FileEdit{{Path: "/etc/modprobe.d/amdgpu.conf", Exists: false, Current: "", Desired: "options amdgpu gttsize=126976\n"}},
	}
	sys := &fakeSystem{root: root, images: []string{"/boot/initrd.img-x"}, baked: map[string]string{},
		bakedPath: "/etc/modprobe.d/amdgpu.conf", failRebuild: map[int]bool{1: true}}
	r := Execute(plan, root, sys)
	if !r.RolledBack {
		t.Fatal("no rollback")
	}
	if _, err := os.Stat(filepath.Join(root, "etc/modprobe.d/amdgpu.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a file created by the failed apply was left behind")
	}
}

// The operator approved a specific diff. If the file changed between planning
// and executing, that diff is no longer what would be written.
func TestExecuteRefusesAFileThatChangedSincePlanning(t *testing.T) {
	root, plan, sys := setup(t)
	if err := os.WriteFile(filepath.Join(root, "etc/modprobe.d/ttm.conf"), []byte("options ttm pages_limit=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Execute(plan, root, sys)
	if r.ExitCode != ExitUsage || sys.rebuilds != 0 {
		t.Errorf("exit %d after %d rebuilds; want refusal before touching anything", r.ExitCode, sys.rebuilds)
	}
	if got := readTTM(t, root); got != "options ttm pages_limit=1\n" {
		t.Errorf("the changed file was overwritten: %q", got)
	}
}

func TestExecuteRefusesAPlanWithAnUnknown(t *testing.T) {
	root, plan, sys := setup(t)
	plan.Params = append(plan.Params, ParamChange{Param: "amdgpu.gttsize", Action: ActionUnknown, Reason: "could not read"})
	r := Execute(plan, root, sys)
	if r.ExitCode != ExitUsage || sys.rebuilds != 0 || readTTM(t, root) != oldTTM {
		t.Errorf("exit %d, %d rebuilds, file %q: a plan containing an unknown was partly executed",
			r.ExitCode, sys.rebuilds, readTTM(t, root))
	}
}

func TestExecuteWithNothingToDoTouchesNothing(t *testing.T) {
	root, _, sys := setup(t)
	r := Execute(Plan{}, root, sys)
	if r.ExitCode != ExitCompliant || sys.rebuilds != 0 {
		t.Errorf("exit %d after %d rebuilds for an empty plan", r.ExitCode, sys.rebuilds)
	}
}

// The physical unit has /boot/initrd.img-<ver>.dpkg-bak. A glob would verify
// it, find the old value, and roll back every correct apply.
func TestImagesUnderIgnoresBackupsAndOnlyListsInstalledKernels(t *testing.T) {
	root := fixtureRoot(t, map[string]string{
		"lib/modules/6.18.44+rex+5-amd64/modules.dep":                                                "",
		"lib/modules/6.18+unreleased-amd64/modules.dep":                                              "",
		"boot/initrd.img-6.18.44+rex+5-amd64":                                                        "x",
		"boot/initrd.img-6.18+unreleased-amd64":                                                      "x",
		"boot/initrd.img-6.18+unreleased-amd64.dpkg-bak":                                             "stale",
		"boot/initrd.img-5.10-removed":                                                               "orphan",
		"efi/amd-ryzen-ai-developer-platform/6.18.44+rex+5-amd64/initrd.img-6.18.44+rex+5-amd64":     "x",
		"efi/amd-ryzen-ai-developer-platform/6.18+unreleased-amd64/initrd.img-6.18+unreleased-amd64": "x",
	})
	got, err := imagesUnder(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range got {
		if strings.HasSuffix(g, ".dpkg-bak") || strings.Contains(g, "5.10-removed") {
			t.Errorf("listed %s, which no boot loads", g)
		}
	}
	if len(got) != 4 {
		t.Errorf("listed %d images, want 4 (two kernels x /boot and ESP): %v", len(got), got)
	}
}
