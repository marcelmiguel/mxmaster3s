// Package main — MX Master 3S HID++ 2.0 battery detection (Windows).
// This file contains only the HID++ protocol logic.
// The program entry point is in tray.go.
package main

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

// Windows API constants
const (
	GENERIC_READ  = 0x80000000
	GENERIC_WRITE = 0x40000000
	OPEN_EXISTING = 3
	SHARE_READ    = 1
	SHARE_WRITE   = 2
	DIGCF_FLAGS   = 0x12 // DIGCF_PRESENT | DIGCF_DEVICEINTERFACE
)

// Logitech VID and MX Master 3S PIDs
const (
	LogitechVID  = uint16(0x046D)
	PID_MX3S_BT  = uint16(0xB023) // Bluetooth LE
	PID_MX3S_USB = uint16(0xC548) // Bolt USB receiver
)

// HID++ 2.0 constants
const (
	ReportLong = byte(0x11) // 20-byte long report
	SoftwareID = byte(0x01) // arbitrary software ID (1-15)

	BatStatusDischarging = byte(0)
	BatStatusRecharging  = byte(1)
	BatStatusAlmostFull  = byte(2)
	BatStatusFull        = byte(3)
	BatStatusSlowCharge  = byte(4)
	BatStatusInvalid     = byte(5)
	BatStatusThermalErr  = byte(6)
)

var (
	hid      = syscall.NewLazyDLL("hid.dll")
	setupapi = syscall.NewLazyDLL("setupapi.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateFileW                      = kernel32.NewProc("CreateFileW")
	procWriteFile                        = kernel32.NewProc("WriteFile")
	procReadFile                         = kernel32.NewProc("ReadFile")
	procCancelIoEx                       = kernel32.NewProc("CancelIoEx")
	procHidGetHidGuid                    = hid.NewProc("HidD_GetHidGuid")
	procHidGetAttributes                 = hid.NewProc("HidD_GetAttributes")
	procHidGetPreparsedData              = hid.NewProc("HidD_GetPreparsedData")
	procHidFreePreparsedData             = hid.NewProc("HidD_FreePreparsedData")
	procHidGetCaps                       = hid.NewProc("HidP_GetCaps")
	procSetupDiGetClassDevsW             = setupapi.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceInterfaces      = setupapi.NewProc("SetupDiEnumDeviceInterfaces")
	procSetupDiGetDeviceInterfaceDetailW = setupapi.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procSetupDiDestroyDeviceInfoList     = setupapi.NewProc("SetupDiDestroyDeviceInfoList")
)

type hidAttrs struct {
	Size          uint32
	VendorID      uint16
	ProductID     uint16
	VersionNumber uint16
	_             uint16
}

type hidCaps struct {
	Usage, UsagePage       uint16
	InLen, OutLen, FeatLen uint16
	Reserved               [17]uint16
}

// BatteryInfo holds the result of a single battery scan.
type BatteryInfo struct {
	Found    bool   // false if device not present or unresponsive
	Level    int    // 0-100 %
	Status   string // human-readable status
	Charging bool   // true when actively recharging or slow-charging
	Stale    bool   // true when data comes from Bolt receiver cache (mouse sleeping)
}

// ScanMouse scans all connected HID devices and returns the MX Master 3S battery info.
func ScanMouse() BatteryInfo {
	var guid syscall.GUID
	procHidGetHidGuid.Call(uintptr(unsafe.Pointer(&guid)))

	hDevInfo, _, _ := procSetupDiGetClassDevsW.Call(
		uintptr(unsafe.Pointer(&guid)), 0, 0, DIGCF_FLAGS,
	)
	defer procSetupDiDestroyDeviceInfoList.Call(hDevInfo)

	for i := 0; ; i++ {
		var ifaceData [32]byte
		*(*uint32)(unsafe.Pointer(&ifaceData[0])) = 32
		ret, _, _ := procSetupDiEnumDeviceInterfaces.Call(
			hDevInfo, 0,
			uintptr(unsafe.Pointer(&guid)),
			uintptr(i),
			uintptr(unsafe.Pointer(&ifaceData[0])),
		)
		if ret == 0 {
			break
		}

		var size uint32
		procSetupDiGetDeviceInterfaceDetailW.Call(
			hDevInfo, uintptr(unsafe.Pointer(&ifaceData[0])),
			0, 0, uintptr(unsafe.Pointer(&size)), 0,
		)
		if size == 0 {
			continue
		}

		detailBuf := make([]byte, size)
		*(*uint32)(unsafe.Pointer(&detailBuf[0])) = 8
		procSetupDiGetDeviceInterfaceDetailW.Call(
			hDevInfo, uintptr(unsafe.Pointer(&ifaceData[0])),
			uintptr(unsafe.Pointer(&detailBuf[0])), uintptr(size),
			0, 0,
		)

		pathPtr := (*uint16)(unsafe.Pointer(&detailBuf[4]))
		handle, _, _ := procCreateFileW.Call(
			uintptr(unsafe.Pointer(pathPtr)),
			GENERIC_READ|GENERIC_WRITE,
			SHARE_READ|SHARE_WRITE,
			0, OPEN_EXISTING, 0, 0,
		)
		h := syscall.Handle(handle)
		if h == syscall.InvalidHandle {
			continue
		}

		attrs := hidAttrs{Size: 12}
		procHidGetAttributes.Call(uintptr(h), uintptr(unsafe.Pointer(&attrs)))
		if attrs.VendorID != LogitechVID ||
			(attrs.ProductID != PID_MX3S_BT && attrs.ProductID != PID_MX3S_USB) {
			syscall.CloseHandle(h)
			continue
		}

		var prep uintptr
		procHidGetPreparsedData.Call(uintptr(h), uintptr(unsafe.Pointer(&prep)))
		var caps hidCaps
		procHidGetCaps.Call(prep, uintptr(unsafe.Pointer(&caps)))
		procHidFreePreparsedData.Call(prep)

		if caps.UsagePage < 0xFF00 || caps.OutLen != 20 {
			syscall.CloseHandle(h)
			continue
		}

		info := fetchBattery(h, attrs.ProductID)
		syscall.CloseHandle(h)
		if info.Found {
			return info
		}
	}
	return BatteryInfo{}
}

// hidppSendTimeout sends a HID++ 2.0 long report and waits for a matching
// response for at most the given duration before cancelling with CancelIoEx.
func hidppSendTimeout(h syscall.Handle, devIdx, featIdx, funcIdx byte, params []byte, timeout time.Duration) ([]byte, bool) {
	cmd := make([]byte, 20)
	cmd[0] = ReportLong
	cmd[1] = devIdx
	cmd[2] = featIdx
	cmd[3] = (funcIdx << 4) | SoftwareID
	copy(cmd[4:], params)

	var done uint32
	ret, _, _ := procWriteFile.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&cmd[0])),
		20,
		uintptr(unsafe.Pointer(&done)),
		0,
	)
	if ret == 0 {
		return nil, false
	}

	type result struct {
		data []byte
		ok   bool
	}
	ch := make(chan result, 1)
	go func() {
		for {
			res := make([]byte, 20)
			var rdone uint32
			r, _, _ := procReadFile.Call(
				uintptr(h),
				uintptr(unsafe.Pointer(&res[0])),
				20,
				uintptr(unsafe.Pointer(&rdone)),
				0,
			)
			if r != 0 {
				// Discard packets that don't match our request
				if res[0] == ReportLong && res[1] == devIdx {
					if res[2] == featIdx || (res[2] == 0x8F && res[3] == featIdx) {
						ch <- result{res, true}
						return
					}
				}
			} else {
				ch <- result{nil, false}
				return
			}
		}
	}()

	select {
	case r := <-ch:
		return r.data, r.ok
	case <-time.After(timeout):
		procCancelIoEx.Call(uintptr(h), 0)
		return nil, false
	}
}

// hidppSend is a convenience wrapper using a 500 ms timeout (fast scan).
func hidppSend(h syscall.Handle, devIdx, featIdx, funcIdx byte, params []byte) ([]byte, bool) {
	return hidppSendTimeout(h, devIdx, featIdx, funcIdx, params, 500*time.Millisecond)
}

// fetchBattery queries battery status on all Bolt receiver device slots and returns
// the first successful result.
func fetchBattery(h syscall.Handle, pid uint16) BatteryInfo {
	devIndices := []byte{0xFF, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
	if pid == PID_MX3S_BT {
		devIndices = []byte{0xFF}
	}

	for _, devIdx := range devIndices {
		// Step 1: discover runtime feature index — try 0x1004 then 0x1000
		var batIdx byte
		var usedFeature uint16
		for _, feat := range []uint16{0x1004, 0x1000} {
			res, ok := hidppSend(h, devIdx, 0x00, 0x00, []byte{byte(feat >> 8), byte(feat)})
			if ok && res[2] != 0x8F && res[4] != 0 {
				batIdx = res[4]
				usedFeature = feat
				break
			}
		}
		if batIdx == 0 {
			continue
		}

		if usedFeature == 0x1000 {
			// Step 2a: GetBatteryLevelStatus (func=0)
			res, ok := hidppSend(h, devIdx, batIdx, 0x00, nil)
			if !ok {
				continue
			}
			level := int(res[4])
			sc := res[6]
			return BatteryInfo{
				Found:    true,
				Level:    level,
				Status:   statusText(sc),
				Charging: sc == BatStatusRecharging || sc == BatStatusSlowCharge,
				Stale:    true,
			}
		}

		// Step 2b: 0x1004 Unified Battery — GetBatteryStatus (func=1)
		// Retry up to 5× with 2 s each to wake sleeping mouse.
		const maxRetries = 5
		var res []byte
		var ok bool
		for attempt := 1; attempt <= maxRetries; attempt++ {
			res, ok = hidppSendTimeout(h, devIdx, batIdx, 0x01, nil, 2*time.Second)
			if ok {
				break
			}
		}
		if !ok {
			continue
		}

		// 0x1004 response: res[4]=level%, res[6]=statusCode
		// (0=discharge, 1=recharge, 2=almostFull, 3=full, 4=slowCharge, 5=invalid, 6=thermal)
		level := int(res[4])
		sr := res[6]
		statusStr := "Unknown"
		switch sr {
		case 0:
			statusStr = "Discharging"
		case 1:
			statusStr = "Recharging"
		case 2:
			statusStr = "Almost Full"
		case 3:
			statusStr = "Full"
		case 4:
			statusStr = "Slow Charge"
		case 5:
			statusStr = "Invalid Battery"
		case 6:
			statusStr = "Thermal Error"
		}
		return BatteryInfo{
			Found:    true,
			Level:    level,
			Status:   statusStr,
			Charging: sr == 1 || sr == 4,
			Stale:    false,
		}
	}
	return BatteryInfo{}
}

func statusText(code byte) string {
	switch code {
	case BatStatusDischarging:
		return "Discharging"
	case BatStatusRecharging:
		return "Recharging"
	case BatStatusAlmostFull:
		return "Almost Full"
	case BatStatusFull:
		return "Full"
	case BatStatusSlowCharge:
		return "Slow Charging"
	case BatStatusInvalid:
		return "Invalid Battery"
	case BatStatusThermalErr:
		return "Thermal Error"
	default:
		return fmt.Sprintf("Unknown (code=%d)", code)
	}
}
