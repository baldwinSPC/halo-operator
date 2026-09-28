// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// System is everything Execute does to a node beyond writing modprobe.d. It is
// an interface so that the rollback paths — the parts that matter most and are
// hardest to provoke on real hardware — are exercised by tests.
type System interface {
	// RebuildInitramfs regenerates the initramfs for every installed kernel.
	RebuildInitramfs() error
	// InitramfsImages lists every initramfs image a boot could load: the
	// /boot copies update-initramfs writes AND the EFI system partition
	// copies the boot loader actually reads.
	InitramfsImages() ([]string, error)
	// ReadFromInitramfs returns one file's content from inside an image.
	ReadFromInitramfs(image, path string) (string, error)
}

// ExitNoPerm: execution needs root (sysexits EX_NOPERM). Distinct from a usage
// error so automation can tell "retry with privilege" from "fix the command".
const ExitNoPerm = 77

// ExecResult records what execution did, including what it undid.
type ExecResult struct {
	Written    []string `json:"written,omitempty"`
	Verified   []string `json:"verifiedImages,omitempty"`
	RolledBack bool     `json:"rolledBack"`
	// Resynced reports whether, after a rollback, the initramfs was rebuilt
	// again from the restored files so the boot image and the config agree.
	Resynced bool   `json:"resynced,omitempty"`
	Error    string `json:"error,omitempty"`
	ExitCode int    `json:"exitCode"`
}

// original is a file's state before execution, so it can be put back exactly.
type original struct {
	path    string
	existed bool
	content []byte
	mode    os.FileMode
}

// Execute carries out a plan: it writes the modprobe.d edits AND rebuilds the
// initramfs, together or not at all. That pairing is the whole point, and the
// owner's decision: on this platform ttm and amdgpu load from the initramfs,
// so a modprobe.d write alone changes nothing at the next boot — and worse,
// lies in wait for the next initramfs rebuild (a kernel upgrade triggers one)
// to take effect at an unpredictable time. So there is no mode that writes
// without rebuilding.
//
// "Together" is verified, not assumed. The boot loader here reads the
// initramfs from the EFI system partition, not /boot, and a post-update hook
// copies it there. After rebuilding, every image a boot could load is opened
// and checked to carry the new options. If any does not, the write is rolled
// back and the initramfs rebuilt again from the restored files, so the node's
// persistent configuration and its boot image never diverge because of this
// tool.
//
// It never reboots. The new values take effect at the next boot, and
// `halo check` then verifies them against the running system.
func Execute(plan Plan, root string, sys System) ExecResult {
	var r ExecResult

	if len(plan.Edits) == 0 {
		r.ExitCode = ExitCompliant
		return r
	}
	for _, c := range plan.Params {
		if c.Action == ActionUnknown {
			r.Error = fmt.Sprintf("%s could not be planned (%s); refusing to execute a plan with an unknown in it", c.Param, c.Reason)
			r.ExitCode = ExitUsage
			return r
		}
	}

	// Snapshot everything before touching anything.
	var originals []original
	for _, e := range plan.Edits {
		path := filepath.Join(root, e.Path)
		o := original{path: path, mode: 0o644}
		if info, err := os.Stat(path); err == nil {
			content, err := os.ReadFile(path)
			if err != nil {
				r.Error = fmt.Sprintf("could not read %s before writing it: %v", e.Path, err)
				r.ExitCode = ExitSoftware
				return r
			}
			o.existed, o.content, o.mode = true, content, info.Mode().Perm()
		} else if !errors.Is(err, os.ErrNotExist) {
			r.Error = fmt.Sprintf("could not stat %s: %v", e.Path, err)
			r.ExitCode = ExitSoftware
			return r
		}
		// The plan was computed from a read of this file. If it has changed
		// since, the diff the operator approved is no longer the diff that
		// would be written.
		if string(o.content) != e.Current {
			r.Error = fmt.Sprintf("%s changed since the plan was computed; re-run apply to plan against its current content", e.Path)
			r.ExitCode = ExitUsage
			return r
		}
		originals = append(originals, o)
	}

	for i, e := range plan.Edits {
		if err := writeAtomic(originals[i].path, []byte(e.Desired), originals[i].mode); err != nil {
			r.Error = fmt.Sprintf("writing %s: %v", e.Path, err)
			r.rollback(originals, sys, false)
			return r
		}
		r.Written = append(r.Written, e.Path)
	}

	if err := sys.RebuildInitramfs(); err != nil {
		r.Error = fmt.Sprintf("rebuilding the initramfs failed: %v", err)
		r.rollback(originals, sys, true)
		return r
	}

	images, err := sys.InitramfsImages()
	if err == nil && len(images) == 0 {
		err = errors.New("no initramfs images were found to verify")
	}
	if err != nil {
		r.Error = fmt.Sprintf("could not verify the rebuilt initramfs: %v", err)
		r.rollback(originals, sys, true)
		return r
	}
	for _, image := range images {
		for _, e := range plan.Edits {
			inside := strings.TrimPrefix(e.Path, "/")
			got, err := sys.ReadFromInitramfs(image, inside)
			if err != nil {
				r.Error = fmt.Sprintf("%s does not carry %s after the rebuild: %v", image, e.Path, err)
				r.rollback(originals, sys, true)
				return r
			}
			if got != e.Desired {
				r.Error = fmt.Sprintf("%s carries a different %s than was written after the rebuild — "+
					"the boot image and the config would diverge", image, e.Path)
				r.rollback(originals, sys, true)
				return r
			}
		}
		r.Verified = append(r.Verified, image)
	}

	r.ExitCode = ExitCompliant
	return r
}

// rollback restores every file to exactly its pre-execution state. If the
// initramfs may already have been rebuilt from the new files, it is rebuilt
// again from the restored ones, so that a failed apply leaves the boot image
// agreeing with the config rather than carrying a change nobody kept.
func (r *ExecResult) rollback(originals []original, sys System, resync bool) {
	r.RolledBack = true
	r.ExitCode = ExitSoftware
	var restoreErrs []string
	for _, o := range originals {
		var err error
		if o.existed {
			err = writeAtomic(o.path, o.content, o.mode)
		} else {
			err = os.Remove(o.path)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		}
		if err != nil {
			restoreErrs = append(restoreErrs, fmt.Sprintf("%s: %v", o.path, err))
		}
	}
	if len(restoreErrs) > 0 {
		r.RolledBack = false
		r.Error += "; ROLLBACK FAILED, restore by hand: " + strings.Join(restoreErrs, "; ")
		return
	}
	if resync {
		if err := sys.RebuildInitramfs(); err != nil {
			r.Error += fmt.Sprintf("; files restored, but rebuilding the initramfs from them also failed (%v) — "+
				"the boot image may not match the config until update-initramfs succeeds", err)
			return
		}
		r.Resynced = true
	}
}

// writeAtomic replaces path's content in one rename, so a crash mid-write
// leaves either the old file or the new one — never a truncated modprobe.d.
func writeAtomic(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".halo-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// imagesUnder lists every initramfs image a boot could load under root, for
// each INSTALLED kernel: the /boot copy update-initramfs writes, and the EFI
// system partition copies the boot loader actually reads, in the Boot Loader
// Specification layout (<esp>/<entry-token>/<version>/initrd.img-<version>).
//
// It is driven by the kernel versions in /lib/modules and matches
// initrd.img-<version> exactly — a positive list — rather than globbing
// initrd.img-*. The physical unit has /boot/initrd.img-<ver>.dpkg-bak, a stale
// package backup update-initramfs never touches; a glob would include it, fail
// to find the new option inside, and roll back every correct apply.
func imagesUnder(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "lib", "modules"))
	if err != nil {
		return nil, fmt.Errorf("listing installed kernels: %w", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ver := e.Name()
		name := "initrd.img-" + ver
		candidates := []string{filepath.Join(root, "boot", name)}
		for _, esp := range []string{"efi", filepath.Join("boot", "efi")} {
			matches, err := filepath.Glob(filepath.Join(root, esp, "*", ver, name))
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, matches...)
		}
		for _, c := range candidates {
			if info, err := os.Stat(c); err == nil && info.Mode().IsRegular() {
				out = append(out, c)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
