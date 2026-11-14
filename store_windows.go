package main

// Native Windows Runtime ABI: no PowerShell, .NET, CGo or external helper.
import (
	"fmt"
	"golang.org/x/sys/windows"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

type comObject struct{ table *[32]uintptr }

var combase = windows.NewLazySystemDLL("combase.dll")

func hrErr(hr uintptr) error {
	if int32(hr) < 0 {
		return fmt.Errorf("Windows HRESULT 0x%08X", uint32(hr))
	}
	return nil
}

//go:uintptrescapes
func (c *comObject) call(slot int, args ...uintptr) error {
	a := append([]uintptr{uintptr(unsafe.Pointer(c))}, args...)
	h, _, _ := syscall.SyscallN(c.table[slot], a...)
	runtime.KeepAlive(c)
	return hrErr(h)
}
func (c *comObject) release() {
	if c != nil {
		c.call(2)
	}
}
func (c *comObject) query(id string) (*comObject, error) {
	guid, e := windows.GUIDFromString(id)
	if e != nil {
		return nil, e
	}
	var p *comObject
	e = c.call(0, uintptr(unsafe.Pointer(&guid)), uintptr(unsafe.Pointer(&p)))
	return p, e
}
func hstring(s string) (uintptr, error) {
	u := syscall.StringToUTF16(s)
	var h uintptr
	r, _, _ := combase.NewProc("WindowsCreateString").Call(uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&h)))
	runtime.KeepAlive(u)
	return h, hrErr(r)
}
func dropString(h uintptr) { combase.NewProc("WindowsDeleteString").Call(h) }
func (c *comObject) text(slot int) string {
	var h uintptr
	if c.call(slot, uintptr(unsafe.Pointer(&h))) != nil {
		return ""
	}
	defer dropString(h)
	var n uint32
	p, _, _ := combase.NewProc("WindowsGetStringRawBuffer").Call(h, uintptr(unsafe.Pointer(&n)))
	if p == 0 || n == 0 || n > 1048576 {
		return ""
	}
	// Copy the native HSTRING buffer before releasing it, without converting a
	// returned integer address into a Go pointer.
	buffer := make([]uint16, n)
	var copied uintptr
	if windows.ReadProcessMemory(windows.CurrentProcess(), p, (*byte)(unsafe.Pointer(&buffer[0])), uintptr(n)*2, &copied) != nil || copied != uintptr(n)*2 {
		return ""
	}
	return windows.UTF16ToString(buffer)
}
func manager() (*comObject, func(), error) {
	runtime.LockOSThread()
	hr, _, _ := combase.NewProc("RoInitialize").Call(1)
	if e := hrErr(hr); e != nil {
		runtime.UnlockOSThread()
		return nil, func() {}, e
	}
	done := func() { combase.NewProc("RoUninitialize").Call(); runtime.UnlockOSThread() }
	h, e := hstring("Windows.Management.Deployment.PackageManager")
	if e != nil {
		done()
		return nil, func() {}, e
	}
	defer dropString(h)
	var obj *comObject
	hr, _, _ = combase.NewProc("RoActivateInstance").Call(h, uintptr(unsafe.Pointer(&obj)))
	if e = hrErr(hr); e != nil {
		done()
		return nil, func() {}, e
	}
	p, e := obj.query("{9A7D4B65-5E8F-4FC7-A2E5-7F6925CB8B53}")
	obj.release()
	if e != nil {
		done()
		return nil, func() {}, e
	}
	return p, func() { p.release(); done() }, nil
}
func storeApps() ([]App, error) {
	pm, done, e := manager()
	if e != nil {
		return nil, e
	}
	defer done()
	var collection *comObject
	// Empty SID means the current user, including when the desktop inventory reads HKLM.
	if e = pm.call(12, 0, uintptr(unsafe.Pointer(&collection))); e != nil {
		return nil, e
	}
	defer collection.release()
	var it *comObject
	if e = collection.call(6, uintptr(unsafe.Pointer(&it))); e != nil {
		return nil, e
	}
	defer it.release()
	result := []App{}
	for {
		var has byte
		if e = it.call(7, uintptr(unsafe.Pointer(&has))); e != nil {
			return result, e
		}
		if has == 0 {
			break
		}
		var p *comObject
		if e = it.call(6, uintptr(unsafe.Pointer(&p))); e != nil {
			return result, e
		}
		a, ok := readPackage(p)
		p.release()
		if ok {
			result = append(result, a)
		}
		if e = it.call(8, uintptr(unsafe.Pointer(&has))); e != nil {
			return result, e
		}
	}
	return result, nil
}
func readPackage(p *comObject) (App, bool) {
	var framework byte
	if p.call(8, uintptr(unsafe.Pointer(&framework))) != nil || framework != 0 {
		return App{}, false
	}
	var id *comObject
	if p.call(6, uintptr(unsafe.Pointer(&id))) != nil {
		return App{}, false
	}
	defer id.release()
	full, family, name := id.text(12), id.text(13), id.text(6)
	if full == "" || family == "" {
		return App{}, false
	}
	a := App{ID: "store:" + full, Name: name, Kind: "商店应用", Package: full, Family: family, Publisher: id.text(10)}
	if info, err := p.query("{5F738B61-F86A-4917-93D1-F1EE9D3B35D9}"); err == nil {
		var ticks int64
		if info.call(7, uintptr(unsafe.Pointer(&ticks))) == nil && ticks > 116444736000000000 {
			a.InstalledOn = time.Unix((ticks-116444736000000000)/10000000, 0).Local().Format("2006-01-02")
		}
		info.release()
	}
	var ver [4]uint16
	if id.call(7, uintptr(unsafe.Pointer(&ver[0]))) == nil {
		a.Version = fmt.Sprintf("%d.%d.%d.%d", ver[0], ver[1], ver[2], ver[3])
	}
	sigKind := int32(-1)
	if info, e := p.query("{A6612FB6-7688-4ACE-95FB-359538E7AA01}"); e == nil {
		defer info.release()
		var resource, bundle byte
		info.call(10, uintptr(unsafe.Pointer(&resource)))
		info.call(11, uintptr(unsafe.Pointer(&bundle)))
		if resource != 0 || bundle != 0 {
			return App{}, false
		}
		if s := info.text(6); s != "" {
			a.Name = s
		}
		if s := info.text(7); s != "" {
			a.Publisher = s
		}
	}
	// IPackage4.get_SignatureKind — System packages are non-removable.
	if pkg4, e := p.query("{65AED1AE-B289-4F07-B29F-2793947E9599}"); e == nil {
		var kind int32
		if pkg4.call(6, uintptr(unsafe.Pointer(&kind))) == nil {
			sigKind = kind
		}
		pkg4.release()
	}
	a.Protection = classifyStorePackage(family, a.Publisher, sigKind)
	return a, true
}

// storeRemovalError maps deployment HRESULTs to messages a user can act on.
func storeRemovalError(code uint32) *uninstallError {
	switch code {
	case 0x80073CFA:
		return failure("store_protected", "Windows 拒绝移除该应用，它可能是系统内置组件")
	case 0x80073D02:
		return failure("store_in_use", "应用或其组件正在运行，请关闭后重试")
	case 0x80073CF1:
		return failure("store_not_found", "系统中找不到该应用包，可能已被卸载，请刷新列表")
	case 0x80073D01:
		return failure("store_policy", "系统策略禁止移除该应用")
	case 0x80070005:
		return failure("access_denied", "没有权限移除该应用")
	}
	return failure("store_failed", "商店应用卸载失败（0x%08X）", code)
}

func removeStore(full string) error {
	unavailable := failure("store_unavailable", "无法连接 Windows 应用部署服务，请稍后重试")
	pm, done, e := manager()
	if e != nil {
		return unavailable
	}
	defer done()
	h, e := hstring(full)
	if e != nil {
		return unavailable
	}
	defer dropString(h)
	var op *comObject
	if e = pm.call(8, h, uintptr(unsafe.Pointer(&op))); e != nil {
		return failure("store_failed", "Windows 未接受该应用的卸载请求")
	}
	defer op.release()
	info, e := op.query("{00000036-0000-0000-C000-000000000046}")
	if e != nil {
		return unavailable
	}
	defer info.release()
	// IAsyncInfo.Status: Started=0, Completed=1, Canceled=2, Error=3.
	for deadline := time.Now().Add(15 * time.Minute); ; {
		var status uint32
		if e = info.call(7, uintptr(unsafe.Pointer(&status))); e != nil {
			return unavailable
		}
		if status == 1 {
			return nil
		}
		if status == 2 {
			return failure("store_canceled", "卸载已被系统取消")
		}
		if status == 3 {
			var code uint32
			info.call(8, uintptr(unsafe.Pointer(&code)))
			return storeRemovalError(code)
		}
		if time.Now().After(deadline) {
			return failure("timeout", "等待 Windows 移除该应用超时，请稍后刷新列表确认")
		}
		time.Sleep(150 * time.Millisecond)
	}
}
