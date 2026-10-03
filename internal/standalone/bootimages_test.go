// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
)

// The four images the physical unit had on 2026-09-27 (halo-operator#20): two
// kernels, each with a /boot copy and an ESP copy.
var (
	bootOld = "boot/initrd.img-6.18+unreleased-amd64"
	bootNew = "boot/initrd.img-6.18.44+rex+5-amd64"
	espOld  = "efi/amd-ryzen-ai-developer-platform/6.18+unreleased-amd64/initrd.img-6.18+unreleased-amd64"
	espNew  = "efi/amd-ryzen-ai-developer-platform/6.18.44+rex+5-amd64/initrd.img-6.18.44+rex+5-amd64"
)

const ttmConf = "options ttm pages_limit=24692260\n"

// fakeImages is a BootImageReader over canned image contents.
type fakeImages struct {
	root    string
	images  []string                     // relative to root
	files   map[string]map[string]string // image -> file name -> content
	broken  map[string]error             // image -> read error
	listErr error
}

func (f fakeImages) InitramfsImages() ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []string
	for _, im := range f.images {
		out = append(out, filepath.Join(f.root, im))
	}
	return out, nil
}

func (f fakeImages) ModprobeFiles(image string) (map[string]string, error) {
	rel, _ := filepath.Rel(f.root, image)
	if err := f.broken[rel]; err != nil {
		return nil, err
	}
	return f.files[rel], nil
}

// diskWith writes /etc/modprobe.d under a fresh root.
func diskWith(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "etc", "modprobe.d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// allCarry is four images that each carry the given files.
func allCarry(files map[string]string) map[string]map[string]string {
	return map[string]map[string]string{bootOld: files, bootNew: files, espOld: files, espNew: files}
}

func lineFor(t *testing.T, c ComponentReport, image string) ImageLine {
	t.Helper()
	for _, l := range c.Images {
		if l.Image == "/"+image {
			return l
		}
	}
	t.Fatalf("no line for /%s in %+v", image, c.Images)
	return ImageLine{}
}

func TestCheckBootImages(t *testing.T) {
	images := []string{bootOld, bootNew, espOld, espNew}
	unrelated := "blacklist pcspkr\n"

	cases := []struct {
		name       string
		disk       map[string]string
		files      map[string]map[string]string
		broken     map[string]error
		want       halov1alpha1.ComponentState
		wantDrift  map[string]string // image -> substring its detail must carry
		wantUnread []string
	}{
		{
			name:  "every image carries ttm.conf as on disk",
			disk:  map[string]string{"ttm.conf": ttmConf},
			files: allCarry(map[string]string{"ttm.conf": ttmConf}),
			want:  halov1alpha1.ComponentReady,
		},
		{
			// The measured #20 layout: the older kernel's ESP image, the one the
			// boot loader reads, has no ttm.conf; its /boot copy does.
			name: "the older kernel's ESP image lacks ttm.conf (#20)",
			disk: map[string]string{"ttm.conf": ttmConf},
			files: map[string]map[string]string{
				bootOld: {"ttm.conf": ttmConf}, bootNew: {"ttm.conf": ttmConf},
				espOld: {}, espNew: {"ttm.conf": ttmConf},
			},
			want:      halov1alpha1.ComponentDegraded,
			wantDrift: map[string]string{espOld: "/etc/modprobe.d/ttm.conf is not present"},
		},
		{
			name: "an image carries a different copy",
			disk: map[string]string{"ttm.conf": ttmConf},
			files: map[string]map[string]string{
				bootOld: {"ttm.conf": ttmConf}, bootNew: {"ttm.conf": ttmConf},
				espOld: {"ttm.conf": "options ttm pages_limit=4096\n"}, espNew: {"ttm.conf": ttmConf},
			},
			want:      halov1alpha1.ComponentDegraded,
			wantDrift: map[string]string{espOld: "differs from the one on disk"},
		},
		{
			// The reverse: the disk dropped the setting, a stale image did not.
			name: "an image carries a governed file the disk no longer has",
			disk: map[string]string{},
			files: map[string]map[string]string{
				bootOld: {}, bootNew: {}, espOld: {"ttm.conf": ttmConf}, espNew: {},
			},
			want:      halov1alpha1.ComponentDegraded,
			wantDrift: map[string]string{espOld: "in the image but no longer on disk"},
		},
		{
			// Files that set no governed parameter are not this check's business,
			// in either direction.
			name: "files that set no governed parameter may differ",
			disk: map[string]string{"ttm.conf": ttmConf, "blacklist.conf": unrelated},
			files: map[string]map[string]string{
				bootOld: {"ttm.conf": ttmConf}, bootNew: {"ttm.conf": ttmConf, "blacklist.conf": "blacklist other\n"},
				espOld: {"ttm.conf": ttmConf, "extra.conf": unrelated}, espNew: {"ttm.conf": ttmConf},
			},
			want: halov1alpha1.ComponentReady,
		},
		{
			name:       "an unreadable image is not observed, never fine",
			disk:       map[string]string{"ttm.conf": ttmConf},
			files:      allCarry(map[string]string{"ttm.conf": ttmConf}),
			broken:     map[string]error{espOld: errors.New("unmkinitramfs: exit status 1")},
			want:       halov1alpha1.ComponentNotObserved,
			wantUnread: []string{espOld},
		},
		{
			name: "drift outranks an unreadable image",
			disk: map[string]string{"ttm.conf": ttmConf},
			files: map[string]map[string]string{
				bootOld: {"ttm.conf": ttmConf}, bootNew: {"ttm.conf": ttmConf}, espOld: {}, espNew: {"ttm.conf": ttmConf},
			},
			broken:     map[string]error{espNew: errors.New("permission denied")},
			want:       halov1alpha1.ComponentDegraded,
			wantDrift:  map[string]string{espOld: "is not present"},
			wantUnread: []string{espNew},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := diskWith(t, tc.disk)
			c := CheckBootImages(root, config(platformPolicy()), fakeImages{root: root, images: images, files: tc.files, broken: tc.broken})
			if c.State != tc.want {
				t.Fatalf("state %s, want %s: %s\n%+v", c.State, tc.want, c.Message, c.Images)
			}
			if !c.counted {
				t.Error("an attempted boot-image check must count toward the outcome")
			}
			if len(c.Images) != len(images) {
				t.Errorf("%d image lines, want one per image (%d)", len(c.Images), len(images))
			}
			for _, im := range images {
				l := lineFor(t, c, im)
				switch {
				case tc.wantDrift[im] != "":
					if l.Result != LineDrift || !strings.Contains(l.Detail, tc.wantDrift[im]) {
						t.Errorf("/%s: %s %q, want drift naming %q", im, l.Result, l.Detail, tc.wantDrift[im])
					}
				case contains(tc.wantUnread, im):
					if l.Result != LineUnobserved {
						t.Errorf("/%s: %s, want unobserved", im, l.Result)
					}
				default:
					if l.Result != LineOK {
						t.Errorf("/%s: %s %q, want ok", im, l.Result, l.Detail)
					}
				}
			}
		})
	}
}

// Every way the check can have nothing to look at is not observed, never
// compliant — or, when the policy governs nothing, not counted at all.
func TestCheckBootImagesWithNothingToCheck(t *testing.T) {
	root := diskWith(t, map[string]string{"ttm.conf": ttmConf})

	t.Run("no images for any installed kernel", func(t *testing.T) {
		c := CheckBootImages(root, config(platformPolicy()), fakeImages{root: root})
		if c.State != halov1alpha1.ComponentNotObserved || !c.counted {
			t.Errorf("state %s counted=%v, want a counted NotObserved: %s", c.State, c.counted, c.Message)
		}
	})
	t.Run("the images cannot be listed", func(t *testing.T) {
		c := CheckBootImages(root, config(platformPolicy()), fakeImages{root: root, listErr: errors.New("no /lib/modules")})
		if c.State != halov1alpha1.ComponentNotObserved || !c.counted {
			t.Errorf("state %s counted=%v, want a counted NotObserved: %s", c.State, c.counted, c.Message)
		}
	})
	t.Run("the memory policy is disabled", func(t *testing.T) {
		cfg := config(platformPolicy())
		off := false
		cfg.Spec.MemoryPolicy.Enabled = &off
		c := CheckBootImages(root, cfg, fakeImages{root: root, images: []string{bootNew}})
		if c.State != halov1alpha1.ComponentDisabled || c.counted {
			t.Errorf("state %s counted=%v, want an uncounted Disabled", c.State, c.counted)
		}
	})
	t.Run("the policy declares only core kernel parameters", func(t *testing.T) {
		cfg := halov1alpha1.HaloDeviceConfig{Spec: halov1alpha1.HaloDeviceConfigSpec{
			MemoryPolicy: halov1alpha1.MemoryPolicySpec{ExpectedKernelArgs: map[string]string{"iommu": "pt"}},
		}}
		c := CheckBootImages(root, cfg, fakeImages{root: root, images: []string{bootNew}})
		if c.State != halov1alpha1.ComponentDisabled || c.counted {
			t.Errorf("state %s counted=%v, want an uncounted Disabled: %s", c.State, c.counted, c.Message)
		}
	})
}

// The boot-image component changes the node's answer when it is counted, and
// leaves it alone when it is skipped.
func TestAddComponentRecomputesTheOutcome(t *testing.T) {
	base := func() Report {
		return Report{Components: []ComponentReport{
			{Name: "memory-policy", State: halov1alpha1.ComponentReady, counted: true},
		}, Outcome: OutcomeCompliant, ExitCode: ExitCompliant}
	}

	r := base()
	r.AddComponent(ComponentReport{Name: BootImagesComponent, State: halov1alpha1.ComponentDegraded, counted: true})
	if r.Outcome != OutcomeDrifted || r.ExitCode != ExitDrifted {
		t.Errorf("a drifted boot image left the node %s (exit %d)", r.Outcome, r.ExitCode)
	}

	r = base()
	r.AddComponent(SkippedBootImages(halov1alpha1.ComponentNotObserved, "skipped, not counted"))
	if r.Outcome != OutcomeCompliant || r.ExitCode != ExitCompliant {
		t.Errorf("a skipped check changed the node to %s (exit %d)", r.Outcome, r.ExitCode)
	}
}

// unmkinitramfs's two layouts, and what must not be mistaken for the
// directory modprobe reads at boot.
func TestModprobeFilesIn(t *testing.T) {
	write := func(t *testing.T, dir string, files map[string]string) {
		t.Helper()
		for rel, content := range files {
			p := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("a single-archive image", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, map[string]string{
			"etc/modprobe.d/ttm.conf": ttmConf,
			"etc/modprobe.d/README":   "not a conf file",
		})
		got, err := modprobeFilesIn(dir)
		if err != nil || len(got) != 1 || got["ttm.conf"] != ttmConf {
			t.Errorf("got %v, %v; want only ttm.conf", got, err)
		}
	})
	t.Run("an early-microcode image unpacked into segments", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, map[string]string{
			"early/kernel/x86/microcode/AuthenticAMD.bin": "ucode",
			"main/etc/modprobe.d/ttm.conf":                ttmConf,
		})
		got, err := modprobeFilesIn(dir)
		if err != nil || got["ttm.conf"] != ttmConf {
			t.Errorf("got %v, %v; want ttm.conf from main/", got, err)
		}
	})
	t.Run("a modprobe.d buried deeper is not the one read at boot", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, map[string]string{
			"usr/etc/modprobe.d/ttm.conf":    ttmConf,
			"main/usr/etc/modprobe.d/x.conf": ttmConf,
		})
		got, err := modprobeFilesIn(dir)
		if err != nil || len(got) != 0 {
			t.Errorf("got %v, %v; want nothing", got, err)
		}
	})
	t.Run("a file in two segments is refused, not picked", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, map[string]string{
			"etc/modprobe.d/ttm.conf":      ttmConf,
			"main/etc/modprobe.d/ttm.conf": "options ttm pages_limit=1\n",
		})
		if got, err := modprobeFilesIn(dir); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("got %v, %v; want an ambiguity error", got, err)
		}
	})
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
