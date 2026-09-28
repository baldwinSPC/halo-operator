// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/healthchecks"
	"github.com/baldwinSPC/halo-operator/internal/memorypolicy"
)

// memoryParams are the module parameters that govern unified memory on this
// hardware, shown by `inspect` in text form. JSON output carries every
// readable parameter; the text view shows the ones a person is looking for.
var memoryParams = []string{
	"ttm.pages_limit",
	"ttm.page_pool_size",
	"amdgpu.gttsize",
	"amdgpu.vramlimit",
	"amdgpu.gartsize",
}

// WriteJSON writes v as indented JSON.
func WriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// RenderInspect writes a human view of one reading. Every absent reading is
// shown as absent, with the collector's reason — never as zero, never omitted.
func RenderInspect(w io.Writer, node string, spec halov1alpha1.HaloNodeObservationSpec) {
	fmt.Fprintf(w, "halo inspect   node=%s   observed=%s\n\n", node, spec.ObservedAt.UTC().Format(time.RFC3339))

	fmt.Fprintln(w, "kernel command line")
	if spec.KernelCmdline == nil {
		fmt.Fprintln(w, "  not observed")
	} else if *spec.KernelCmdline == "" {
		fmt.Fprintln(w, "  (read, and empty)")
	} else {
		fmt.Fprintf(w, "  %s\n", *spec.KernelCmdline)
	}

	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if spec.ModuleParams == nil {
		fmt.Fprintln(tw, "module parameters\tnot observed")
	} else {
		fmt.Fprintf(tw, "module parameters\t%d readable (all of them in --json)\n", len(spec.ModuleParams))
		for _, name := range memoryParams {
			if v, ok := spec.ModuleParams[name]; ok {
				fmt.Fprintf(tw, "  %s\t%s\n", name, v)
				continue
			}
			fmt.Fprintf(tw, "  %s\t%s\n", name, absentParamReason(spec.ModuleParams, name))
		}
	}
	tw.Flush()

	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "amdgpu device\t")
	if spec.GTTTotalBytes == nil {
		fmt.Fprintln(tw, "  GTT total\tnot observed")
	} else {
		fmt.Fprintf(tw, "  GTT total\t%d GiB (%d bytes)\n", *spec.GTTTotalBytes>>30, *spec.GTTTotalBytes)
	}
	fmt.Fprintf(tw, "  GPU busy\t%s\n", percentOrAbsent(spec.GPUBusyPercent))
	switch {
	case spec.SCLKMHz != nil && spec.RatedSCLKMHz != nil:
		fmt.Fprintf(tw, "  shader clock\t%d MHz of %d MHz rated\n", *spec.SCLKMHz, *spec.RatedSCLKMHz)
	case spec.RatedSCLKMHz != nil:
		fmt.Fprintf(tw, "  shader clock\tcurrent not observed, %d MHz rated\n", *spec.RatedSCLKMHz)
	default:
		fmt.Fprintln(tw, "  shader clock\tnot observed")
	}
	tw.Flush()

	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "kernel-log counters\t")
	fmt.Fprintf(tw, "  permission faults\t%s\n", countOrAbsent(spec.PermissionFaults, "no validated kernel-log reader yet"))
	fmt.Fprintf(tw, "  queue-create failures\t%s\n", countOrAbsent(spec.QueueCreateFailures, "no validated kernel-log reader yet"))
	tw.Flush()

	if len(spec.Messages) > 0 {
		fmt.Fprintln(w, "\nwhy some readings are absent")
		for _, m := range spec.Messages {
			fmt.Fprintf(w, "  - %s\n", m)
		}
	}
}

// absentParamReason distinguishes a module that is not loaded from a loaded
// module whose parameter file is unreadable. The second is routine here —
// /sys/module/amdgpu/parameters/* is mode 0400 — and the fix is different, so
// the message should be too.
func absentParamReason(params map[string]string, name string) string {
	module, _, _ := strings.Cut(name, ".")
	for k := range params {
		if strings.HasPrefix(k, module+".") {
			return "not readable by this user (run as root to see it)"
		}
	}
	return "not observed (module not loaded, or its parameters are unreadable)"
}

// RenderCheck writes a human view of a Report.
func RenderCheck(w io.Writer, r Report, policyPath string) {
	fmt.Fprintf(w, "halo check   node=%s   observed=%s   policy=%s\n",
		r.Node, r.ObservedAt.UTC().Format(time.RFC3339), policyPath)

	for _, c := range r.Components {
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Name, c.State, c.Message)
		for _, p := range c.Params {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", marker(p.Result), p.Name, describeParam(p))
		}
		for _, p := range c.Probes {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", marker(probeResult(p.Result)), p.Probe, p.Message)
		}
		tw.Flush()
		if hint := healthHint(c); hint != "" {
			fmt.Fprintf(w, "  hint: %s\n", hint)
		}
	}

	if len(r.Warnings) > 0 {
		fmt.Fprintln(w)
		for _, warning := range r.Warnings {
			fmt.Fprintf(w, "warning: %s\n", warning)
		}
	}
	fmt.Fprintf(w, "\nresult: %s (exit %d)\n", r.Outcome, r.ExitCode)
}

func describeParam(p ParamLine) string {
	switch p.Result {
	case LineOK:
		return fmt.Sprintf("%s\tfrom %s", p.Actual, p.Source)
	case LineUnobserved:
		return fmt.Sprintf("expected %s\t%s", p.Expected, p.Source)
	}
	if p.Source == memorypolicy.SourceUnset {
		return fmt.Sprintf("expected %s\tnot set", p.Expected)
	}
	return fmt.Sprintf("expected %s\tbut is %s, from %s", p.Expected, p.Actual, p.Source)
}

// healthHint says the one useful thing when health is undecidable only
// because of the kernel-log probes, which have no validated reader: scoping
// the probes makes the condition decidable today.
func healthHint(c ComponentReport) string {
	if c.Name != "health-checks" || c.State != halov1alpha1.ComponentNotObserved {
		return ""
	}
	var blocked []string
	for _, p := range c.Probes {
		if p.Result != string(healthchecks.ResultNotObserved) {
			continue
		}
		if p.Probe != healthchecks.ProbePermissionFault && p.Probe != healthchecks.ProbeQueueCreate {
			return ""
		}
		blocked = append(blocked, p.Probe)
	}
	if len(blocked) == 0 {
		return ""
	}
	sort.Strings(blocked)
	return fmt.Sprintf("%s have no validated kernel-log reader, so they can never be observed yet; "+
		"set healthChecks.probes to [clock-under-load] to make health decidable today",
		strings.Join(blocked, " and "))
}

func marker(result string) string {
	switch result {
	case LineOK:
		return "[ok]"
	case LineDrift:
		return "[drift]"
	default:
		return "[?]"
	}
}

func probeResult(r string) string {
	switch r {
	case string(healthchecks.ResultHealthy):
		return LineOK
	case string(healthchecks.ResultUnhealthy):
		return LineDrift
	default:
		return LineUnobserved
	}
}

func percentOrAbsent(p *int32) string {
	if p == nil {
		return "not observed"
	}
	return fmt.Sprintf("%d%%", *p)
}

func countOrAbsent(p *int32, why string) string {
	if p == nil {
		return "not observed — " + why
	}
	return fmt.Sprintf("%d", *p)
}

// RenderPlan writes a human view of a Plan. It leads with the fact that
// nothing was written, because the most dangerous misreading of a plan is
// taking it for a record of what happened.
func RenderPlan(w io.Writer, p Plan, policyPath string) {
	fmt.Fprintf(w, "halo apply   PLAN ONLY — nothing has been written   node=%s   policy=%s\n\n", p.Node, policyPath)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "parameters\t\t\t")
	for _, c := range p.Params {
		current := c.Current
		if current == "" {
			current = string(c.Source)
		}
		fmt.Fprintf(tw, "  [%s]\t%s\t%s -> %s\n", c.Action, c.Param, current, c.Desired)
	}
	tw.Flush()
	for _, c := range p.Params {
		if c.Reason != "" {
			fmt.Fprintf(w, "    %s: %s\n", c.Param, c.Reason)
		}
	}

	for _, e := range p.Edits {
		state := "edit"
		if !e.Exists {
			state = "new file"
		}
		fmt.Fprintf(w, "\n%s  (%s)\n", e.Path, state)
		for _, line := range diffLines(e.Current, e.Desired) {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}

	if len(p.FollowUp) > 0 {
		fmt.Fprintln(w, "\nthen, for the change to take effect:")
		for i, step := range p.FollowUp {
			fmt.Fprintf(w, "  %d. %s\n", i+1, step)
		}
	}
	if len(p.Warnings) > 0 {
		fmt.Fprintln(w, "\nwarnings:")
		for _, warning := range p.Warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
	}

	switch p.ExitCode {
	case ExitCompliant:
		fmt.Fprintln(w, "\nnothing to do: every planned parameter already has its declared value.")
	default:
		fmt.Fprintln(w, "\nthis is a plan. `halo apply` does not execute it yet — writing modprobe.d is only")
		fmt.Fprintln(w, "meaningful together with an initramfs rebuild, and that rebuild is an operation this")
		fmt.Fprintln(w, "project gates as destructive. Execution is pending an explicit owner decision.")
	}
	fmt.Fprintf(w, "\nexit %d\n", p.ExitCode)
}

// diffLines renders the change between two versions of a file. The planner
// only ever rewrites lines in place or appends, so a positional comparison is
// exact — no general diff algorithm is needed, and none that could misalign.
func diffLines(before, after string) []string {
	b := splitLines(before)
	a := splitLines(after)
	var out []string
	for i := 0; i < len(a) || i < len(b); i++ {
		switch {
		case i >= len(b):
			out = append(out, "+ "+a[i])
		case i >= len(a):
			out = append(out, "- "+b[i])
		case a[i] != b[i]:
			out = append(out, "- "+b[i], "+ "+a[i])
		default:
			out = append(out, "  "+a[i])
		}
	}
	return out
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
