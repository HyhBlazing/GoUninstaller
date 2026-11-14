package main

import (
	"fmt"
	"golang.org/x/sys/windows/registry"
	"sort"
	"strings"
)

type App struct {
	ID, Name, Version, Publisher, Kind, Location, Command, Package, Family string
	RegistryPath                                                           string
	Protection                                                             string // store: user | system | blocked
	QuietCommand                                                           string // QuietUninstallString, if the vendor registered one
	MSI                                                                    bool   // WindowsInstaller=1
	Machine                                                                bool
	View                                                                   uint32
	IconPath, InstalledOn, SizeSource                                      string
	IconIndex                                                              int
	SizeBytes                                                              uint64
	SizeKnown                                                              bool
}

const uninstallRoot = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`

func regText(k registry.Key, name string) string {
	s, _, _ := k.GetStringValue(name)
	return strings.TrimSpace(s)
}
func inventory() ([]App, error) {
	result, _ := inventoryKind(0)
	store, e := inventoryKind(1)
	result = append(result, store...)
	sort.Slice(result, func(i, j int) bool { return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name) })
	return result, e
}
func inventoryKind(index int) ([]App, error) {
	var items []App
	var err error
	if index == 1 {
		items, err = storeApps()
	} else {
		items = desktopApps()
	}
	for i := range items {
		enrichApp(&items[i])
	}
	sort.Slice(items, func(i, j int) bool { return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name) })
	return items, err
}
func desktopApps() []App {
	result := []App{}
	seen := map[string]bool{}
	for _, scope := range []struct {
		root    registry.Key
		machine bool
	}{{registry.CURRENT_USER, false}, {registry.LOCAL_MACHINE, true}} {
		for _, view := range []uint32{registry.WOW64_64KEY, registry.WOW64_32KEY} {
			root, e := registry.OpenKey(scope.root, uninstallRoot, registry.READ|view)
			if e != nil {
				continue
			}
			names, _ := root.ReadSubKeyNames(-1)
			for _, name := range names {
				k, e := registry.OpenKey(root, name, registry.READ|view)
				if e != nil {
					continue
				}
				display, cmd := regText(k, "DisplayName"), regText(k, "UninstallString")
				system, _, _ := k.GetIntegerValue("SystemComponent")
				if display == "" || cmd == "" || system == 1 || regText(k, "ParentKeyName") != "" {
					k.Close()
					continue
				}
				id := fmt.Sprintf("desktop:%t:%d:%s", scope.machine, view, name)
				dedup := fmt.Sprintf("%t|%s|%s", scope.machine, strings.ToLower(display), strings.ToLower(cmd))
				if !seen[dedup] {
					a := App{ID: id, Name: display, Command: cmd, Version: regText(k, "DisplayVersion"), Publisher: regText(k, "Publisher"), Location: regText(k, "InstallLocation"), Kind: "桌面软件", Machine: scope.machine, View: view, RegistryPath: uninstallRoot + `\` + name}
					a.QuietCommand = regText(k, "QuietUninstallString")
					if wi, _, err := k.GetIntegerValue("WindowsInstaller"); err == nil && wi == 1 {
						a.MSI = true
					}
					a.IconPath, a.IconIndex = parseIconLocation(regText(k, "DisplayIcon"))
					a.InstalledOn = readInstallDate(k)
					if kb, _, err := k.GetIntegerValue("EstimatedSize"); err == nil && kb > 0 {
						a.SizeBytes, a.SizeKnown, a.SizeSource = kb*1024, true, "安装程序登记的估计大小"
					}
					result = append(result, a)
					seen[dedup] = true
				}
				k.Close()
			}
			root.Close()
		}
	}
	return result
}
func stillInstalled(a App) (bool, error) {
	if a.Package != "" {
		items, e := storeApps()
		if e != nil {
			return true, e
		}
		for _, b := range items {
			if b.Family == a.Family {
				return true, nil
			}
		}
		return false, nil
	}
	root := registry.CURRENT_USER
	if a.Machine {
		root = registry.LOCAL_MACHINE
	}
	k, e := registry.OpenKey(root, a.RegistryPath, registry.READ|a.View)
	if e == registry.ErrNotExist {
		return false, nil
	}
	if e != nil {
		return true, e
	}
	k.Close()
	return true, nil
}
