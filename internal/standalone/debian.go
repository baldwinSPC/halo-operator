// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DebianSystem is the real System on a Debian-family node, which is what the
// AMD Ryzen AI Developer Platform is. It is deliberately the only code here
// that runs external commands, and it is kept thin: every decision about what
// to do and whether it worked lives in Execute, where tests reach it.
type DebianSystem struct {
	// Root is the filesystem root, "/" on a real node.
	Root string
}

// updateInitramfs is looked up at an absolute path. It lives in /usr/sbin,
// which is not on an ordinary user's PATH — and a tool that silently found a
// different binary, or none, would fail in the one place it must not.
var updateInitramfsPaths = []string{"/usr/sbin/update-initramfs", "/sbin/update-initramfs"}

// RebuildInitramfs runs `update-initramfs -u -k all`, as root.
//
// -k all rather than the platform ttm-config's bare -u, which updates only the
// newest kernel. modprobe.d is kernel-independent configuration; an installed
// kernel whose initramfs does not carry it is a boot path where the change
// silently does not apply, and the default boot entry is not guaranteed to be
// the newest kernel. Every image is then verified, so a kernel the rebuild
// missed is caught rather than assumed.
//
// Note the side effect, which is the platform's and not this tool's: the
// systemd-boot post-update hook runs `kernel-install add` for each kernel,
// which also regenerates the loader entries from /etc/kernel/cmdline.
func (d DebianSystem) RebuildInitramfs() error {
	bin := ""
	for _, p := range updateInitramfsPaths {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			bin = p
			break
		}
	}
	if bin == "" {
		return fmt.Errorf("update-initramfs not found at %s; this tool only knows Debian-family initramfs tooling",
			strings.Join(updateInitramfsPaths, " or "))
	}
	out, err := exec.Command(bin, "-u", "-k", "all").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s -u -k all: %v\n%s", bin, err, lastLines(string(out), 12))
	}
	return nil
}

// InitramfsImages lists every image a boot could load.
func (d DebianSystem) InitramfsImages() ([]string, error) { return imagesUnder(d.Root) }

// ReadFromInitramfs unpacks an image and returns one file from inside it.
// unmkinitramfs handles early-microcode segments by splitting into early/ and
// main/, so the file is looked for at the top level and under each segment.
func (d DebianSystem) ReadFromInitramfs(image, path string) (string, error) {
	dir, err := os.MkdirTemp("", "halo-initramfs-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	if out, err := exec.Command("unmkinitramfs", image, dir).CombinedOutput(); err != nil {
		return "", fmt.Errorf("unmkinitramfs %s: %v\n%s", image, err, lastLines(string(out), 6))
	}
	var found []string
	err = filepath.WalkDir(dir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if !entry.IsDir() && (rel == path || strings.HasSuffix(rel, "/"+path)) {
			found = append(found, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	switch len(found) {
	case 0:
		return "", errors.New("not present in the image")
	case 1:
		content, err := os.ReadFile(found[0])
		return string(content), err
	default:
		// Two copies in one image is not a layout this tool understands, so
		// it will not pick one and call the result verified.
		return "", fmt.Errorf("present %d times in the image, so which one applies is ambiguous", len(found))
	}
}

// ModprobeFiles unpacks an image once and returns every
// /etc/modprobe.d/*.conf inside it. Once per image rather than once per file:
// the boot-image check reads the whole directory, and each unpack of a
// real image takes seconds.
func (d DebianSystem) ModprobeFiles(image string) (map[string]string, error) {
	dir, err := os.MkdirTemp("", "halo-initramfs-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if out, err := exec.Command("unmkinitramfs", image, dir).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("unmkinitramfs %s: %v\n%s", image, err, lastLines(string(out), 6))
	}
	return modprobeFilesIn(dir)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
