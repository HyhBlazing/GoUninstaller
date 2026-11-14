package main

import "testing"

func TestClassifyStorePackage(t *testing.T) {
	cases := []struct {
		family, publisher string
		sig               int32
		want              string
	}{
		{"SpotifyAB.SpotifyMusic_zpdnekdrzrea0", "Spotify AB", sigStore, protectUser},
		{"Microsoft.WindowsCalculator_8wekyb3d8bbwe", "Microsoft Corporation", sigStore, protectUser},
		{"Microsoft.Windows.ShellExperienceHost_cw5n1h2txyewy", "Microsoft Windows", sigSystem, protectBlocked},
		{"Something.Custom_cw5n1h2txyewy", "Microsoft Windows", -1, protectBlocked},
		{"Microsoft.WindowsStore_8wekyb3d8bbwe", "Microsoft Corporation", sigStore, protectSystem},
		{"MicrosoftWindows.Client.CBS_cw5n1h2txyewy", "Microsoft Windows", -1, protectBlocked},
		{"MicrosoftWindows.Client.WebExperience_cw5n1h2txyewy", "Microsoft Windows", sigSystem, protectBlocked},
		{"Microsoft.DesktopAppInstaller_8wekyb3d8bbwe", "Microsoft Corporation", sigStore, protectSystem},
		{"Microsoft.SecHealthUI_8wekyb3d8bbwe", "Microsoft Corporation", sigStore, protectSystem},
		{"windows.immersivecontrolpanel_cw5n1h2txyewy", "Microsoft Windows", -1, protectBlocked},
		{"Microsoft.Windows.CloudExperienceHost_cw5n1h2txyewy", "Microsoft Windows", -1, protectBlocked},
		{"Microsoft.BingWeather_8wekyb3d8bbwe", "Microsoft Corporation", sigStore, protectUser},
		{"Developer.Sideload_abc123", "Dev", sigDeveloper, protectUser},
		{"Any.Package_xyz", "Someone", sigSystem, protectBlocked},
	}
	for _, c := range cases {
		got := classifyStorePackage(c.family, c.publisher, c.sig)
		if got != c.want {
			t.Errorf("%s sig=%d: got %s want %s", c.family, c.sig, got, c.want)
		}
	}
}

func TestStoreUninstallAllowed(t *testing.T) {
	if e := storeUninstallAllowed(App{Name: "Desktop"}); e != nil {
		t.Fatal(e)
	}
	if e := storeUninstallAllowed(App{Package: "x", Family: "App_zpdnekdrzrea0", Protection: protectUser}); e != nil {
		t.Fatal(e)
	}
	if e := storeUninstallAllowed(App{Package: "x", Family: "App_zpdnekdrzrea0", Protection: protectSystem}); e != nil {
		t.Fatal(e)
	}
	if e := storeUninstallAllowed(App{Package: "x", Family: "Microsoft.Windows.ShellExperienceHost_cw5n1h2txyewy", Protection: protectBlocked}); e != errStoreBlocked {
		t.Fatalf("blocked: %v", e)
	}
	// Derive from family when Protection empty.
	if e := storeUninstallAllowed(App{Package: "x", Family: "Microsoft.AAD.BrokerPlugin_cw5n1h2txyewy"}); e != errStoreBlocked {
		t.Fatalf("derived blocked: %v", e)
	}
}

func TestProtectionLabel(t *testing.T) {
	if protectionLabel(protectBlocked) != "不可卸载" || protectionLabel(protectSystem) != "系统相关" || protectionLabel(protectUser) != "" {
		t.Fatal("labels")
	}
}
