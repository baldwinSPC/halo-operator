// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

// halo is the AMD Halo Operator for a single node, with no Kubernetes.
//
// It reads the node exactly as the operator's DaemonSet agent does and judges
// it exactly as the controller does — through the same functions, not a copy
// of them — so a Halo that is not in a cluster gets the same verdict as one
// that is. The policy file is the HaloDeviceConfig manifest a cluster user
// would `kubectl apply`.
//
//	halo inspect             what this node reports, read-only
//	halo check  -policy F    judge it against a policy, read-only (as root, every
//	                         boot image too)
//	halo apply  -policy F    plan what would bring it into line (writes nothing)
//
// Exit codes carry the operator's three-valued answer, so a script can never
// read "could not tell" as "fine":
//
//	0   compliant: every enabled check was observed and matched
//	1   drifted: something was observed to disagree (for apply: action needed)
//	2   not observed: nothing disagreed, but compliance could not be established
//	64  usage error          70  internal error
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/nodeagent"
	"github.com/baldwinSPC/halo-operator/internal/standalone"
)

const usage = `halo — the AMD Halo Operator for a single node, no Kubernetes required

usage:
  halo inspect [-json]                 what this node reports (read-only)
  halo check   -policy FILE [-json]    judge this node against a policy (read-only)
  halo apply   -policy FILE [-json]    plan what would bring it into line (writes nothing)
  halo apply   -policy FILE -execute   carry the plan out, as root (see below)

FILE is a HaloDeviceConfig manifest — the same file you would kubectl apply.

check, run as root, also opens every initramfs image a boot could load — the
/boot copies and the EFI system partition copies the boot loader actually
reads — and verifies each carries the /etc/modprobe.d files that set the
policy's module parameters exactly as they are on disk. A node can be right on
disk and in its running kernel and still boot another kernel at the default
GTT. Not run as root, or with -boot-images=false, that part is skipped and
does not change the exit code.

apply -execute writes the planned /etc/modprobe.d edits AND rebuilds the
initramfs — together or not at all, since on this platform a modprobe.d
change without a rebuild does nothing now and lands silently later. Every boot
image is then checked to carry the change; if any does not, the write is
rolled back. It never reboots and never touches the kernel command line.

exit codes:
  0  compliant       every enabled check was observed and matched
                     (apply -execute: written, rebuilt and verified; reboot to apply)
  1  drifted         something was observed to disagree (apply: action needed)
  2  not observed    nothing disagreed, but compliance could not be established
  64 usage error     70 internal error (apply -execute: failed, rolled back)
  77 needs root
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// geteuid is os.Geteuid, held in a variable so each test chooses the
// privilege path it exercises. Inheriting it would make the tests mean
// different things on different runners: CI's job container runs as root.
var geteuid = os.Geteuid

// newBootImageReader is what check reads boot images with — a variable so
// tests can supply images without a node's initramfs tooling.
var newBootImageReader = func(root string) standalone.BootImageReader {
	return standalone.DebianSystem{Root: root}
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return standalone.ExitUsage
	}
	switch args[0] {
	case "inspect":
		return inspect(args[1:], stdout, stderr)
	case "check":
		return check(args[1:], stdout, stderr)
	case "apply":
		return apply(args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return standalone.ExitCompliant
	default:
		fmt.Fprintf(stderr, "halo: unknown command %q\n\n%s", args[0], usage)
		return standalone.ExitUsage
	}
}

// common holds the flags every subcommand shares.
type common struct {
	json     bool
	procRoot string
	sysRoot  string
	node     string
}

func newFlags(name string, stderr io.Writer, c *common) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	host, _ := os.Hostname()
	fs.BoolVar(&c.json, "json", false, "emit JSON instead of text")
	fs.StringVar(&c.procRoot, "proc-root", "/proc", "where the host's /proc is visible")
	fs.StringVar(&c.sysRoot, "sys-root", "/sys", "where the host's /sys is visible")
	fs.StringVar(&c.node, "node", host, "name to report this node as")
	return fs
}

// parse maps flag errors onto the usage exit code. flag.ExitOnError would exit
// 2, which here means "not observed" — a parse error must never be mistaken
// for a verdict about the node.
func parse(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return standalone.ExitCompliant, false
		}
		return standalone.ExitUsage, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "%s: unexpected arguments: %s\n", fs.Name(), strings.Join(fs.Args(), " "))
		return standalone.ExitUsage, false
	}
	return 0, true
}

func read(c common) halov1alpha1.HaloNodeObservationSpec {
	reading := nodeagent.Collector{ProcRoot: c.procRoot, SysRoot: c.sysRoot}.Collect()
	return nodeagent.SpecFromReading(reading, metav1.Now())
}

func inspect(args []string, stdout, stderr io.Writer) int {
	var c common
	fs := newFlags("inspect", stderr, &c)
	if code, ok := parse(fs, args); !ok {
		return code
	}
	spec := read(c)
	if c.json {
		if err := standalone.WriteJSON(stdout, spec); err != nil {
			fmt.Fprintf(stderr, "halo: %v\n", err)
			return standalone.ExitSoftware
		}
		return standalone.ExitCompliant
	}
	standalone.RenderInspect(stdout, c.node, spec)
	return standalone.ExitCompliant
}

func check(args []string, stdout, stderr io.Writer) int {
	var c common
	var policy, root string
	var bootImages bool
	fs := newFlags("check", stderr, &c)
	fs.StringVar(&policy, "policy", "", "HaloDeviceConfig manifest to check against (required)")
	fs.StringVar(&root, "root", "/", "filesystem root to read /etc/modprobe.d, installed kernels and boot images from")
	fs.BoolVar(&bootImages, "boot-images", true,
		"as root, check that every boot image carries /etc/modprobe.d as it is on disk")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if policy == "" {
		fmt.Fprintln(stderr, "check: -policy is required; a check with nothing declared would pass by checking nothing")
		return standalone.ExitUsage
	}
	cfg, err := standalone.LoadPolicy(policy)
	if err != nil {
		fmt.Fprintf(stderr, "check: %v\n", err)
		return standalone.ExitUsage
	}
	report := standalone.Evaluate(c.node, read(c), *cfg)
	switch {
	case !bootImages:
		report.AddComponent(standalone.SkippedBootImages(halov1alpha1.ComponentDisabled,
			"skipped: -boot-images=false"))
	case geteuid() != 0:
		report.AddComponent(standalone.SkippedBootImages(halov1alpha1.ComponentNotObserved,
			"skipped, not counted: the EFI system partition, where the boot loader reads the initramfs, "+
				"is readable only by root — run check as root to verify every boot image"))
	default:
		report.AddComponent(standalone.CheckBootImages(root, *cfg, newBootImageReader(root)))
	}
	if c.json {
		if err := standalone.WriteJSON(stdout, report); err != nil {
			fmt.Fprintf(stderr, "halo: %v\n", err)
			return standalone.ExitSoftware
		}
	} else {
		standalone.RenderCheck(stdout, report, policy)
	}
	return report.ExitCode
}

func apply(args []string, stdout, stderr io.Writer) int {
	var c common
	var policy, root string
	var execute bool
	fs := newFlags("apply", stderr, &c)
	fs.StringVar(&policy, "policy", "", "HaloDeviceConfig manifest to plan against (required)")
	fs.StringVar(&root, "root", "/", "filesystem root to read modprobe.d from")
	fs.BoolVar(&execute, "execute", false,
		"carry the plan out: write modprobe.d AND rebuild the initramfs, verified, rolled back on failure (root)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if policy == "" {
		fmt.Fprintln(stderr, "apply: -policy is required")
		return standalone.ExitUsage
	}
	// Checked before anything is read or planned, so a non-root run fails
	// fast and plainly rather than partway through with a permission error.
	if execute && geteuid() != 0 {
		fmt.Fprintln(stderr, "apply: -execute writes /etc/modprobe.d and rebuilds the initramfs, so it must run as root")
		return standalone.ExitNoPerm
	}
	cfg, err := standalone.LoadPolicy(policy)
	if err != nil {
		fmt.Fprintf(stderr, "apply: %v\n", err)
		return standalone.ExitUsage
	}
	plan := standalone.PlanApply(c.node, read(c), *cfg, root)

	if !execute {
		if c.json {
			if err := standalone.WriteJSON(stdout, plan); err != nil {
				fmt.Fprintf(stderr, "halo: %v\n", err)
				return standalone.ExitSoftware
			}
		} else {
			standalone.RenderPlan(stdout, plan, policy)
		}
		return plan.ExitCode
	}

	result := standalone.Execute(plan, root, standalone.DebianSystem{Root: root})
	if c.json {
		if err := standalone.WriteJSON(stdout, struct {
			Plan   standalone.Plan       `json:"plan"`
			Result standalone.ExecResult `json:"result"`
		}{plan, result}); err != nil {
			fmt.Fprintf(stderr, "halo: %v\n", err)
			return standalone.ExitSoftware
		}
	} else {
		standalone.RenderExec(stdout, plan, result, policy)
	}
	return result.ExitCode
}
