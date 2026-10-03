// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/memorypolicy"
	"github.com/baldwinSPC/halo-operator/internal/observe"
)

// BootImagesComponent names the boot-image consistency check in a Report.
const BootImagesComponent = "boot-images"

// BootImageReader is what the boot-image check reads. DebianSystem implements
// it; tests use a fake, because unpacking a real initramfs needs the node's
// tooling and the interesting cases are the ones a real node rarely shows.
type BootImageReader interface {
	// InitramfsImages lists every image a boot could load: the /boot copies
	// and the EFI system partition copies the boot loader actually reads.
	InitramfsImages() ([]string, error)
	// ModprobeFiles returns every /etc/modprobe.d/*.conf inside an image, by
	// file name.
	ModprobeFiles(image string) (map[string]string, error)
}

// ImageLine is one boot image's result.
type ImageLine struct {
	Image  string `json:"image"`
	Result string `json:"result"`
	Detail string `json:"detail"`
}

// CheckBootImages answers whether every image a boot could load carries the
// /etc/modprobe.d files that set the policy's module parameters, byte for
// byte as they are on disk (halo-operator#20).
//
// It exists because the running system cannot answer that. On this platform
// ttm and amdgpu load from the initramfs, and the boot loader reads the
// initramfs from the EFI system partition, where a post-update hook copies it
// — and exits 0 without copying when it cannot. So a node can be correct on
// disk, correct in /boot, correct in its running kernel, and still come up at
// the kernel-default GTT the next time a different kernel boots. The physical
// unit was in exactly that state: its older kernel's ESP image carried no
// ttm.conf while every other check reported the node as fine.
//
// Only files that set a governed parameter are compared, and in both
// directions: a governed file missing from an image, a copy that differs, and
// an image carrying a governed file the disk no longer has are all drift. Only
// /etc/modprobe.d is compared — the administrator's directory, where the
// platform's ttm-config and `halo apply` write.
func CheckBootImages(root string, cfg halov1alpha1.HaloDeviceConfig, images BootImageReader) ComponentReport {
	c := ComponentReport{Name: BootImagesComponent}
	resolved := cfg.Spec.WithDefaults()
	if !*resolved.MemoryPolicy.Enabled {
		c.State = halov1alpha1.ComponentDisabled
		c.Message = "the memory policy is disabled, so no module parameter governs what a boot image carries"
		return c
	}
	governed := governedModuleParams(resolved.MemoryPolicy)
	if len(governed) == 0 {
		c.State = halov1alpha1.ComponentDisabled
		c.Message = "the policy declares no module parameters, so there is nothing a boot image must carry"
		return c
	}
	c.counted = true

	disk, err := governedFilesOnDisk(filepath.Join(root, "etc", "modprobe.d"), governed)
	if err != nil {
		c.State = halov1alpha1.ComponentNotObserved
		c.Message = fmt.Sprintf("could not read /etc/modprobe.d: %v", err)
		return c
	}

	paths, err := images.InitramfsImages()
	if err != nil {
		c.State = halov1alpha1.ComponentNotObserved
		c.Message = fmt.Sprintf("could not list the boot images: %v", err)
		return c
	}
	if len(paths) == 0 {
		// Nothing checked is not the same as nothing wrong.
		c.State = halov1alpha1.ComponentNotObserved
		c.Message = "no initramfs image was found for any installed kernel, so no boot path was checked"
		return c
	}

	var drifted, unobserved int
	for _, image := range paths {
		line := ImageLine{Image: displayPath(root, image)}
		inImage, err := images.ModprobeFiles(image)
		if err != nil {
			line.Result = LineUnobserved
			line.Detail = fmt.Sprintf("could not be read: %v", err)
			unobserved++
			c.Images = append(c.Images, line)
			continue
		}
		if problems := compareModprobeD(disk, inImage, governed); len(problems) > 0 {
			line.Result = LineDrift
			line.Detail = strings.Join(problems, "; ")
			drifted++
		} else {
			line.Result = LineOK
			line.Detail = describeCarried(disk)
		}
		c.Images = append(c.Images, line)
	}

	switch {
	case drifted > 0:
		c.State = halov1alpha1.ComponentDegraded
		c.Message = fmt.Sprintf("%d of %d boot images do not carry /etc/modprobe.d as it is on disk — "+
			"booting one of them silently applies a different memory configuration", drifted, len(paths))
	case unobserved > 0:
		c.State = halov1alpha1.ComponentNotObserved
		c.Message = fmt.Sprintf("%d of %d boot images could not be read, so consistency is not established",
			unobserved, len(paths))
	default:
		c.State = halov1alpha1.ComponentReady
		c.Message = fmt.Sprintf("all %d boot images carry /etc/modprobe.d as it is on disk", len(paths))
	}
	return c
}

// SkippedBootImages reports the check as not run. It is not counted, so a
// skip never changes the outcome: an unprivileged `halo check` answers exactly
// as it did before this check existed, and says what it did not look at.
func SkippedBootImages(state halov1alpha1.ComponentState, reason string) ComponentReport {
	return ComponentReport{Name: BootImagesComponent, State: state, Message: reason}
}

// governedModuleParams is module -> parameters for every module parameter the
// memory policy expects. Core kernel parameters exist only on the command line
// and never in modprobe.d, so they govern no boot image.
func governedModuleParams(spec halov1alpha1.MemoryPolicySpec) map[string][]string {
	out := map[string][]string{}
	for name := range observe.MemoryPolicy(spec).ExpectedKernelArgs {
		if !memorypolicy.IsModuleParam(name) {
			continue
		}
		module, param, _ := strings.Cut(name, ".")
		out[module] = append(out[module], param)
	}
	return out
}

func setsGoverned(content string, governed map[string][]string) bool {
	for module, params := range governed {
		for _, param := range params {
			if setsParam(content, module, param) {
				return true
			}
		}
	}
	return false
}

// governedFilesOnDisk returns the content of every *.conf in dir that sets a
// governed parameter, by file name.
func governedFilesOnDisk(dir string, governed map[string][]string) (map[string]string, error) {
	files, err := confFiles(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, f := range files {
		content, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if setsGoverned(string(content), governed) {
			out[filepath.Base(f)] = string(content)
		}
	}
	return out, nil
}

// compareModprobeD lists every way one image's /etc/modprobe.d disagrees with
// the disk about a governed file, in a stable order.
func compareModprobeD(disk, inImage map[string]string, governed map[string][]string) []string {
	var problems []string
	for _, name := range sortedKeys(disk) {
		got, ok := inImage[name]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("/etc/modprobe.d/%s is not present", name))
		case got != disk[name]:
			problems = append(problems, fmt.Sprintf("/etc/modprobe.d/%s differs from the one on disk", name))
		}
	}
	for _, name := range sortedKeys(inImage) {
		if _, onDisk := disk[name]; !onDisk && setsGoverned(inImage[name], governed) {
			problems = append(problems, fmt.Sprintf("/etc/modprobe.d/%s is in the image but no longer on disk", name))
		}
	}
	return problems
}

func describeCarried(disk map[string]string) string {
	if len(disk) == 0 {
		return "carries no file setting a governed parameter, and neither does the disk"
	}
	names := sortedKeys(disk)
	for i, n := range names {
		names[i] = "/etc/modprobe.d/" + n
	}
	return "carries " + strings.Join(names, ", ") + " as on disk"
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// imageSegment matches the directories unmkinitramfs unpacks a multi-segment
// image into: early microcode archives first, then the main archive.
var imageSegment = regexp.MustCompile(`^(early[0-9]*|main)$`)

// modprobeFilesIn reads every etc/modprobe.d/*.conf from an unpacked image.
//
// unmkinitramfs writes a single-archive image at the top level and a
// multi-segment one as early/, early2/, …, main/. So the directory is looked
// for at the top and one segment down, and nowhere else: a modprobe.d buried
// deeper is not one modprobe reads at boot. A file present in two places is
// refused rather than one copy being picked and called verified — the same
// rule ReadFromInitramfs applies.
func modprobeFilesIn(dir string) (map[string]string, error) {
	candidates := []string{filepath.Join(dir, "etc", "modprobe.d")}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() && imageSegment.MatchString(e.Name()) {
			candidates = append(candidates, filepath.Join(dir, e.Name(), "etc", "modprobe.d"))
		}
	}
	out := map[string]string{}
	from := map[string]string{}
	for _, d := range candidates {
		files, err := confFiles(d)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			name := filepath.Base(f)
			where := displayPath(dir, f)
			if prev, dup := from[name]; dup {
				return nil, fmt.Errorf("%s is in the image twice (%s and %s), so which one applies is ambiguous",
					name, prev, where)
			}
			content, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			out[name], from[name] = string(content), where
		}
	}
	return out, nil
}
