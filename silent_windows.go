package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Installer technologies recognised for silent uninstall.
const (
	installerStore    = "store"
	installerMSI      = "msi"
	installerInno     = "inno"
	installerNSIS     = "nsis"
	installerBurn     = "burn"
	installerSquirrel = "squirrel"
	installerVendor   = "vendor_quiet" // the vendor registered a QuietUninstallString
	installerUnknown  = "unknown"
)

var (
	innoUninstaller = regexp.MustCompile(`(?i)^unins\d{3}\.exe$`)
	msiUILevel      = regexp.MustCompile(`(?i)^[/-](q[nbrf]?[+!-]*|quiet|passive)$`)
)

type uninstallPlan struct {
	Exe       string
	Args      []string
	Installer string
	Silent    bool // the command runs without showing a wizard
}

type uninstallInspection struct {
	ID              string `json:"id"`
	SilentSupported bool   `json:"silent_supported"`
	Installer       string `json:"installer"`
}

func registryKeyName(a App) string {
	if i := strings.LastIndex(a.RegistryPath, `\`); i >= 0 {
		return a.RegistryPath[i+1:]
	}
	return a.RegistryPath
}

func hasSwitch(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if strings.EqualFold(a, n) {
				return true
			}
		}
	}
	return false
}

func isMSIExec(exe string) bool { return strings.EqualFold(filepath.Base(exe), "msiexec.exe") }

// looksLikeNSIS checks for the Nullsoft stub markers near the start of the file.
func looksLikeNSIS(path string) bool {
	f, e := os.Open(path)
	if e != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 2<<20)
	n, _ := io.ReadFull(f, buf)
	buf = buf[:n]
	return bytes.Contains(buf, []byte("Nullsoft.NSIS.exehead")) || bytes.Contains(buf, []byte("NullsoftInst"))
}

func detectInstaller(a App, exe string, args []string) string {
	base := filepath.Base(exe)
	switch {
	case isMSIExec(exe):
		return installerMSI
	case strings.HasSuffix(strings.ToLower(registryKeyName(a)), "_is1"), innoUninstaller.MatchString(base):
		return installerInno
	case strings.Contains(strings.ToLower(exe), `\package cache\`) && hasSwitch(args, "/uninstall", "-uninstall"):
		return installerBurn
	case strings.EqualFold(base, "update.exe") && hasSwitch(args, "--uninstall"):
		return installerSquirrel
	case looksLikeNSIS(exe):
		return installerNSIS
	}
	return installerUnknown
}

// msiSilentArgs replaces any UI level with a fully silent, no-restart removal.
func msiSilentArgs(args []string) []string {
	out := make([]string, 0, len(args)+2)
	for _, a := range args {
		if !msiUILevel.MatchString(a) {
			out = append(out, a)
		}
	}
	out = append(out, "/qn")
	if !hasSwitch(out, "/norestart", "-norestart") {
		out = append(out, "/norestart")
	}
	return out
}

func appendMissing(args []string, switches ...string) []string {
	for _, s := range switches {
		if !hasSwitch(args, s) {
			args = append(args, s)
		}
	}
	return args
}

// planUninstall builds the command to run. Standard mode runs the registered
// command unchanged. Silent mode uses, in order: Windows Installer switches,
// the vendor's QuietUninstallString, then the switches of a recognised
// installer technology. If none applies the plan stays interactive and
// Silent is false, so the caller can report the fallback.
func planUninstall(a App, silent bool) (uninstallPlan, *uninstallError) {
	exe, args, e := uninstallCommand(a.Command)
	if e != nil {
		return uninstallPlan{}, failure("unsupported_command", "%s", e.Error())
	}
	plan := uninstallPlan{Exe: exe, Args: args, Installer: detectInstaller(a, exe, args)}
	if !silent {
		return plan, nil
	}
	if plan.Installer == installerMSI {
		plan.Args, plan.Silent = msiSilentArgs(args), true
		return plan, nil
	}
	if quiet := strings.TrimSpace(a.QuietCommand); quiet != "" {
		if qexe, qargs, qe := uninstallCommand(quiet); qe == nil {
			plan.Exe, plan.Args, plan.Silent = qexe, qargs, true
			if isMSIExec(qexe) {
				plan.Installer, plan.Args = installerMSI, msiSilentArgs(qargs)
			} else if plan.Installer == installerUnknown {
				plan.Installer = installerVendor
			}
			return plan, nil
		}
	}
	switch plan.Installer {
	case installerInno:
		if !hasSwitch(args, "/VERYSILENT") {
			args = append(args, "/VERYSILENT")
		}
		plan.Args, plan.Silent = appendMissing(args, "/SUPPRESSMSGBOXES", "/NORESTART"), true
	case installerNSIS:
		// The switch is case sensitive and must precede any `_?=` argument.
		present := false
		for _, v := range args {
			if v == "/S" {
				present = true
			}
		}
		if !present {
			args = append([]string{"/S"}, args...)
		}
		plan.Args, plan.Silent = args, true
	case installerBurn:
		plan.Args, plan.Silent = appendMissing(args, "/quiet", "/norestart"), true
	case installerSquirrel:
		plan.Args, plan.Silent = appendMissing(args, "-s"), true
	}
	return plan, nil
}

// inspectUninstall reports whether an app can be removed without a wizard.
func inspectUninstall(a App) uninstallInspection {
	out := uninstallInspection{ID: a.ID, Installer: installerUnknown}
	if a.Package != "" {
		out.Installer = installerStore
		out.SilentSupported = storeUninstallAllowed(a) == nil
		return out
	}
	if plan, e := planUninstall(a, true); e == nil {
		out.Installer, out.SilentSupported = plan.Installer, plan.Silent
	}
	return out
}
