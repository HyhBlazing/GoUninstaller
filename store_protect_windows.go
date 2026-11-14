package main

import (
	"errors"
	"strings"
)

var errStoreBlocked = errors.New("该商店组件为系统关键或不可卸载，已阻止卸载")

// Store package protection levels exposed to the UI and uninstall path.
const (
	protectUser    = "user"    // 普通用户应用
	protectSystem  = "system"  // 系统相关依赖（可尝试卸载，需额外确认）
	protectBlocked = "blocked" // 不可卸载 / 系统组件
)

// PackageSignatureKind values from Windows.ApplicationModel.PackageSignatureKind.
const (
	sigNone       int32 = 0
	sigDeveloper  int32 = 1
	sigEnterprise int32 = 2
	sigStore      int32 = 3
	sigSystem     int32 = 4
)

// Windows inbox publisher id (CN=Microsoft Windows, ...).
const windowsPublisherID = "cw5n1h2txyewy"

// Microsoft Store / Microsoft Corporation publisher id.
const microsoftPublisherID = "8wekyb3d8bbwe"

// Families that must never be uninstalled through this tool.
var blockedStoreFamilies = map[string]struct{}{
	"Microsoft.AAD.BrokerPlugin":                  {},
	"Microsoft.AccountsControl":                   {},
	"Microsoft.AsyncTextService":                  {},
	"Microsoft.BioEnrollment":                     {},
	"Microsoft.ECApp":                             {},
	"Microsoft.LockApp":                           {},
	"Microsoft.Win32WebViewHost":                  {},
	"Microsoft.Windows.Apprep.ChxApp":             {},
	"Microsoft.Windows.AugLoop.CBS":               {},
	"Microsoft.Windows.CallingShellApp":           {},
	"Microsoft.Windows.CloudExperienceHost":       {},
	"Microsoft.Windows.ContentDeliveryManager":    {},
	"Microsoft.Windows.NarratorQuickStart":        {},
	"Microsoft.Windows.PeopleExperienceHost":      {},
	"Microsoft.Windows.PinningConfirmationDialog": {},
	"Microsoft.Windows.PrintQueueActionCenter":    {},
	"Microsoft.Windows.Search":                    {},
	"Microsoft.Windows.ShellExperienceHost":       {},
	"Microsoft.Windows.StartMenuExperienceHost":   {},
	"Microsoft.XboxGameCallableUI":                {},
	"Windows.CBSPreview":                          {},
	"Windows.PrintDialog":                         {},
	"windows.immersivecontrolpanel":               {},
}

// Important Microsoft dependencies: uninstallable in theory, but high risk.
var systemStoreFamilies = map[string]struct{}{
	"Microsoft.DesktopAppInstaller":  {},
	"Microsoft.MicrosoftEdge.Stable": {},
	"Microsoft.SecHealthUI":          {},
	"Microsoft.StorePurchaseApp":     {},
	"Microsoft.WindowsStore":         {},
}

var blockedFamilyPrefixes = []string{
	"MicrosoftWindows.Client.",
	"Microsoft.Windows.Shell",
	"Microsoft.Windows.StartMenu",
	"Microsoft.UI.Xaml.",
}

var systemFamilyPrefixes = []string{
	"Microsoft.MicrosoftEdge",
	"MicrosoftWindows.",
}

func packageFamilyName(family string) string {
	family = strings.TrimSpace(family)
	if i := strings.LastIndex(family, "_"); i > 0 {
		return family[:i]
	}
	return family
}

func packagePublisherID(family string) string {
	family = strings.TrimSpace(family)
	if i := strings.LastIndex(family, "_"); i >= 0 && i+1 < len(family) {
		return strings.ToLower(family[i+1:])
	}
	return ""
}

func hasAnyPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// classifyStorePackage maps a Store package into user / system / blocked.
// signatureKind may be -1 when the OS does not expose IPackage4.
func classifyStorePackage(family, publisher string, signatureKind int32) string {
	name := packageFamilyName(family)
	pubID := packagePublisherID(family)
	if name == "" {
		return protectUser
	}
	if signatureKind == sigSystem {
		return protectBlocked
	}
	if pubID == windowsPublisherID {
		return protectBlocked
	}
	if _, ok := blockedStoreFamilies[name]; ok {
		return protectBlocked
	}
	if hasAnyPrefix(name, blockedFamilyPrefixes) {
		return protectBlocked
	}
	// Case-insensitive match for immersive control panel style names.
	lower := strings.ToLower(name)
	if lower == "windows.immersivecontrolpanel" || lower == "windows.printdialog" || lower == "windows.cbspreview" {
		return protectBlocked
	}
	if _, ok := systemStoreFamilies[name]; ok {
		return protectSystem
	}
	if hasAnyPrefix(name, systemFamilyPrefixes) {
		return protectSystem
	}
	// Remaining Microsoft-published packages under SystemRoot-style naming.
	if pubID == microsoftPublisherID && strings.HasPrefix(name, "Microsoft.Windows.") {
		switch name {
		case "Microsoft.WindowsCalculator",
			"Microsoft.WindowsCamera",
			"Microsoft.WindowsNotepad",
			"Microsoft.WindowsTerminal",
			"Microsoft.WindowsTerminalPreview",
			"Microsoft.Windows.Photos",
			"Microsoft.Paint",
			"Microsoft.ScreenSketch",
			"Microsoft.WindowsAlarms",
			"Microsoft.WindowsMaps",
			"Microsoft.WindowsSoundRecorder",
			"Microsoft.WindowsFeedbackHub",
			"Microsoft.GetHelp",
			"Microsoft.Getstarted":
			return protectUser
		default:
			if publisherLooksMicrosoft(publisher) {
				return protectSystem
			}
		}
	}
	return protectUser
}

func publisherLooksMicrosoft(publisher string) bool {
	p := strings.ToLower(strings.TrimSpace(publisher))
	return strings.Contains(p, "microsoft")
}

func protectionLabel(level string) string {
	switch level {
	case protectBlocked:
		return "不可卸载"
	case protectSystem:
		return "系统相关"
	default:
		return ""
	}
}

func storeUninstallAllowed(a App) error {
	if a.Package == "" {
		return nil
	}
	level := a.Protection
	if level == "" {
		level = classifyStorePackage(a.Family, a.Publisher, -1)
	}
	if level == protectBlocked {
		return errStoreBlocked
	}
	return nil
}
