// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/memorypolicy"
	"github.com/baldwinSPC/halo-operator/internal/observe"
)

// Actions a plan can take for one parameter.
const (
	ActionUnchanged  = "unchanged"
	ActionModprobe   = "modprobe.d"
	ActionOutOfScope = "out-of-scope"
	ActionUnknown    = "unknown"
)

// ParamChange is what the plan would do about one expected parameter.
type ParamChange struct {
	Param   string              `json:"param"`
	Current string              `json:"current,omitempty"`
	Source  memorypolicy.Source `json:"currentSource"`
	Desired string              `json:"desired"`
	Action  string              `json:"action"`
	Reason  string              `json:"reason,omitempty"`
}

// FileEdit is one modprobe.d file the plan would write.
type FileEdit struct {
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	Current string `json:"current"`
	Desired string `json:"desired"`
}

// Plan is what `halo apply` would do. Computing it writes nothing.
type Plan struct {
	Node     string        `json:"node"`
	Params   []ParamChange `json:"params"`
	Edits    []FileEdit    `json:"edits,omitempty"`
	FollowUp []string      `json:"followUp,omitempty"`
	Warnings []string      `json:"warnings,omitempty"`
	// ExitCode: 0 nothing to do, 1 action needed, 2 a plan could not be
	// established.
	ExitCode int `json:"exitCode"`
}

// modprobeDirs are searched for existing settings, highest precedence first.
// Only the /etc directory is ever proposed as a write target: it is the
// administrator's directory, and the others belong to packages or are
// runtime-only.
var modprobeDirs = []string{"etc/modprobe.d", "run/modprobe.d", "usr/local/lib/modprobe.d", "usr/lib/modprobe.d", "lib/modprobe.d"}

// PlanApply works out what it would take to bring this node's module
// parameters to the policy, rooted at root ("/" on a real node, a fixture tree
// in tests). It never writes.
//
// The scope is deliberately narrow. Only module parameters are planned, and
// only through modprobe.d, because that is the mechanism this platform itself
// uses — its ttm-config.service writes /etc/modprobe.d/ttm.conf. The kernel
// command line is never planned: changing it is Glimmer's signed-task path,
// decided by the owner for exactly this hardware (halo-operator#13), and a
// second tool editing loader entries would be two writers on one boot
// configuration.
func PlanApply(node string, spec halov1alpha1.HaloNodeObservationSpec, cfg halov1alpha1.HaloDeviceConfig, root string) Plan {
	plan := Plan{Node: node}
	resolved := cfg.Spec.WithDefaults()
	if !*resolved.MemoryPolicy.Enabled {
		plan.Warnings = append(plan.Warnings, "the memory policy is disabled, so there is nothing to apply")
		return plan
	}

	obs := observe.MemoryObservation(node, &spec)
	policy := observe.MemoryPolicy(resolved.MemoryPolicy)

	names := make([]string, 0, len(policy.ExpectedKernelArgs))
	for name := range policy.ExpectedKernelArgs {
		names = append(names, name)
	}
	sort.Strings(names)

	// module -> param -> desired value, for the modprobe.d edits.
	wanted := map[string]map[string]string{}
	for _, name := range names {
		desired := policy.ExpectedKernelArgs[name]
		current, source := memorypolicy.ResolveParam(obs, name)
		change := ParamChange{Param: name, Current: current, Source: source, Desired: desired}

		switch {
		case (source == memorypolicy.SourceCmdline || source == memorypolicy.SourceModule) && current == desired:
			change.Action = ActionUnchanged

		case !memorypolicy.IsModuleParam(name):
			change.Action = ActionOutOfScope
			change.Reason = "a core kernel parameter, which exists only on the kernel command line; " +
				"changing the command line is Glimmer's signed-task path (halo-operator#13), not this tool's"

		case source == memorypolicy.SourceCmdline:
			// The command line is applied at boot, after modprobe.d options
			// are baked into the initramfs, so a modprobe.d edit would be
			// silently overridden. Planning one would be planning a no-op.
			change.Action = ActionOutOfScope
			change.Reason = fmt.Sprintf("set on the kernel command line to %q, which overrides modprobe.d "+
				"at boot; a modprobe.d edit would be silently ignored, and the command line is Glimmer's "+
				"signed-task path (halo-operator#13)", current)

		default:
			change.Action = ActionModprobe
			if source == memorypolicy.SourceUnreadable {
				change.Reason = "the running value could not be read by this user, so whether this is a change " +
					"is unknown; the write is planned from the policy alone (run as root to see the current value)"
			}
			module, param, _ := strings.Cut(name, ".")
			if wanted[module] == nil {
				wanted[module] = map[string]string{}
			}
			wanted[module][param] = desired
		}
		plan.Params = append(plan.Params, change)
	}

	modules := make([]string, 0, len(wanted))
	for m := range wanted {
		modules = append(modules, m)
	}
	sort.Strings(modules)
	for _, module := range modules {
		edit, warnings, err := planModuleEdit(root, module, wanted[module])
		plan.Warnings = append(plan.Warnings, warnings...)
		if err != nil {
			plan.Warnings = append(plan.Warnings, err.Error())
			for i := range plan.Params {
				if strings.HasPrefix(plan.Params[i].Param, module+".") && plan.Params[i].Action == ActionModprobe {
					plan.Params[i].Action = ActionUnknown
					plan.Params[i].Reason = err.Error()
				}
			}
			continue
		}
		if edit != nil {
			plan.Edits = append(plan.Edits, *edit)
		}
	}

	if len(plan.Edits) > 0 {
		plan.FollowUp = []string{
			"update-initramfs -u — on this platform ttm and amdgpu are loaded from the initramfs, and " +
				"modprobe.d options are copied into it when it is built, so editing modprobe.d alone changes " +
				"nothing at the next boot. The platform's own ttm-config does exactly this rebuild.",
			"reboot — GTT is sized when amdgpu initialises, so a new value cannot take effect on a running system.",
			"halo check — verify against the running value afterwards, rather than trusting that the write took.",
		}
		plan.Warnings = append(plan.Warnings,
			"writing modprobe.d WITHOUT rebuilding the initramfs does not leave the node unchanged: the "+
				"stale initramfs keeps the old value until something else rebuilds it, and a kernel upgrade "+
				"does that automatically — so the change lands silently, at an unpredictable later boot.")
	}

	plan.ExitCode = ExitCompliant
	for _, c := range plan.Params {
		switch c.Action {
		case ActionModprobe, ActionOutOfScope:
			plan.ExitCode = ExitDrifted
		case ActionUnknown:
			if plan.ExitCode == ExitCompliant {
				plan.ExitCode = ExitNotObserved
			}
		}
	}
	return plan
}

// planModuleEdit decides which file to edit for one module and what it would
// contain. It prefers a file that already sets one of the parameters, then a
// file with any options line for the module, and only then a new
// /etc/modprobe.d/<module>.conf — matching the platform's own naming, and
// never creating a second file that competes with an existing one.
func planModuleEdit(root, module string, params map[string]string) (*FileEdit, []string, error) {
	var warnings []string
	etc := filepath.Join(root, "etc", "modprobe.d")

	// Where else, anywhere, is each parameter set? A value set in more than
	// one place is decided by modprobe's merge order, and the plan should say
	// so rather than imply its edit is the only one that counts.
	setIn := map[string][]string{}
	for _, dir := range modprobeDirs {
		files, err := confFiles(filepath.Join(root, dir))
		if err != nil {
			return nil, warnings, err
		}
		for _, f := range files {
			content, err := os.ReadFile(f)
			if err != nil {
				return nil, warnings, fmt.Errorf("could not read %s: %v", displayPath(root, f), err)
			}
			for param := range params {
				if setsParam(string(content), module, param) {
					setIn[param] = append(setIn[param], displayPath(root, f))
				}
			}
		}
	}
	for param, where := range setIn {
		if len(where) > 1 {
			warnings = append(warnings, fmt.Sprintf(
				"%s.%s is set in %d files (%s); the effective value depends on modprobe's merge order, "+
					"so verify with `halo check` after rebooting", module, param, len(where), strings.Join(where, ", ")))
		}
	}

	etcFiles, err := confFiles(etc)
	if err != nil {
		return nil, warnings, err
	}
	target := ""
	for _, f := range etcFiles {
		content, _ := os.ReadFile(f)
		for param := range params {
			if setsParam(string(content), module, param) {
				target = f
				break
			}
		}
		if target != "" {
			break
		}
	}
	if target == "" {
		for _, f := range etcFiles {
			content, _ := os.ReadFile(f)
			if hasOptionsFor(string(content), module) {
				target = f
				break
			}
		}
	}
	exists := target != ""
	if !exists {
		target = filepath.Join(etc, module+".conf")
	}

	current := ""
	if exists {
		raw, err := os.ReadFile(target)
		if err != nil {
			return nil, warnings, fmt.Errorf("could not read %s: %v", displayPath(root, target), err)
		}
		current = string(raw)
	}

	desired := current
	paramNames := make([]string, 0, len(params))
	for p := range params {
		paramNames = append(paramNames, p)
	}
	sort.Strings(paramNames)
	for _, p := range paramNames {
		desired, err = editOptions(desired, module, p, params[p])
		if err != nil {
			return nil, warnings, fmt.Errorf("%s: %v", displayPath(root, target), err)
		}
	}

	if filepath.Base(target) == "ttm.conf" && module == "ttm" {
		warnings = append(warnings,
			"/etc/modprobe.d/ttm.conf is also managed by the platform's ttm-config.service, which runs only "+
				"when that file is absent; editing it in place keeps that service skipped, and deleting it "+
				"would let the service rewrite it at the next boot")
	}
	if desired == current {
		return nil, warnings, nil
	}
	return &FileEdit{Path: displayPath(root, target), Exists: exists, Current: current, Desired: desired}, warnings, nil
}

// editOptions sets <module> <param>=<value> in modprobe.d content, touching
// nothing else. An existing occurrence is rewritten in place; failing that the
// parameter is appended to the module's last options line; failing that a new
// options line is added.
//
// Line continuations are refused rather than edited. A backslash-continued
// options line is legal modprobe.d and rare, and the parameter could be on the
// continued half — guessing there risks writing a file that says something
// other than what the plan showed.
func editOptions(content, module, param, value string) (string, error) {
	hadTrailingNewline := content == "" || strings.HasSuffix(content, "\n")
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if content == "" {
		lines = nil
	}

	found := false
	lastOptions := -1
	for i, line := range lines {
		if strings.HasSuffix(strings.TrimRight(line, " \t"), `\`) {
			return "", fmt.Errorf("line %d uses a line continuation, which this tool will not edit", i+1)
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "options" || fields[1] != module {
			continue
		}
		lastOptions = i
		changed := false
		for j := 2; j < len(fields); j++ {
			if key, _, ok := strings.Cut(fields[j], "="); ok && key == param {
				fields[j] = param + "=" + value
				found, changed = true, true
			}
		}
		if changed {
			lines[i] = strings.Join(fields, " ")
		}
	}

	switch {
	case found:
	case lastOptions >= 0:
		lines[lastOptions] = strings.TrimRight(lines[lastOptions], " \t") + " " + param + "=" + value
	default:
		lines = append(lines, "options "+module+" "+param+"="+value)
	}

	out := strings.Join(lines, "\n")
	if hadTrailingNewline {
		out += "\n"
	}
	return out, nil
}

func setsParam(content, module, param string) bool {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "options" || fields[1] != module {
			continue
		}
		for _, f := range fields[2:] {
			if key, _, ok := strings.Cut(f, "="); ok && key == param {
				return true
			}
		}
	}
	return false
}

func hasOptionsFor(content, module string) bool {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "options" && fields[1] == module {
			return true
		}
	}
	return false
}

// confFiles lists *.conf in dir, sorted. A missing directory is empty, not an
// error — most of the search path does not exist on any given system.
func confFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not list %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".conf") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// displayPath shows a path as it is on the node, not as it is under a test
// fixture root.
func displayPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return "/" + filepath.ToSlash(rel)
}
