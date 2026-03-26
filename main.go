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
	SoftwareID = byte(0x01) // arbitrary (1-15)

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

func main() {
	fmt.Println("Scanning for MX Master 3S...")

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
		*(*uint32)(unsafe.Pointer(&detailBuf[0])) = 8 // cbSize of SP_DEVICE_INTERFACE_DETAIL_DATA_W
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

		// Filter: VID/PID must match MX Master 3S
		attrs := hidAttrs{Size: 12}
		procHidGetAttributes.Call(uintptr(h), uintptr(unsafe.Pointer(&attrs)))
		if attrs.VendorID != LogitechVID ||
			(attrs.ProductID != PID_MX3S_BT && attrs.ProductID != PID_MX3S_USB) {
			syscall.CloseHandle(h)
			continue
		}

		// Filter: vendor-specific usage page (HID++ 2.0)
		var prep uintptr
		procHidGetPreparsedData.Call(uintptr(h), uintptr(unsafe.Pointer(&prep)))
		var caps hidCaps
		procHidGetCaps.Call(prep, uintptr(unsafe.Pointer(&caps)))
		procHidFreePreparsedData.Call(prep)

		if caps.UsagePage < 0xFF00 {
			syscall.CloseHandle(h)
			continue
		}

		// ── KEY FIX ──────────────────────────────────────────────────────────
		// The Bolt receiver exposes TWO vendor-specific HID interfaces:
		//   • Short-report  (OutLen = 7)  — does NOT handle long packets
		//   • Long-report   (OutLen = 20) — the HID++ 2.0 command channel
		// Only the long-report interface responds; the other blocks forever.
		if caps.OutLen != 20 {
			syscall.CloseHandle(h)
			continue
		}

		fmt.Printf("Found MX Master 3S (VID=%04X PID=%04X) — reading battery...\n",
			attrs.VendorID, attrs.ProductID)

		if fetchBattery(h, attrs.ProductID) {
			syscall.CloseHandle(h)
			return
		}
		syscall.CloseHandle(h)
	}

	fmt.Println("MX Master 3S not found or battery not available.")
}

// hidppSendTimeout sends a HID++ 2.0 long report and waits for a matching
// response for at most the given duration before cancelling.
//
// Packet layout: [reportId, devIdx, featIdx, funcIdx<<4|swId, params...]
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

	// Read in a goroutine so we can apply a timeout.
	// If we time out we close the handle (caller's responsibility), which
	// unblocks the goroutine's ReadFile call with an error — no goroutine leak.
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
				// Verify this packet is for our device and feature before accepting it.
				// Format: [Report ID, DeviceIdx, FeatureIdx, ...] or [Report ID, DeviceIdx, 0x8F, FeatureIdx, ...]
				if res[0] == ReportLong && res[1] == devIdx {
					if res[2] == featIdx || (res[2] == 0x8F && res[3] == featIdx) {
						ch <- result{res, true}
						return
					}
				}
				// Otherwise, loop and read the next packet in the buffer
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
		// Cancel the pending ReadFile operation
		procCancelIoEx.Call(uintptr(h), 0)
		return nil, false
	}
}

// hidppSend is a convenience wrapper using a 500 ms timeout (fast scan for most requests).
func hidppSend(h syscall.Handle, devIdx, featIdx, funcIdx byte, params []byte) ([]byte, bool) {
	return hidppSendTimeout(h, devIdx, featIdx, funcIdx, params, 500*time.Millisecond)
}

// fetchBattery tries device indices 0xFF (direct/BT) and 0x01-0x06 (Bolt receiver slots)
// because the mouse might be paired to any slot on the receiver.
func fetchBattery(h syscall.Handle, pid uint16) bool {
	devIndices := []byte{0xFF, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
	if pid == PID_MX3S_BT {
		devIndices = []byte{0xFF}
	}

	for _, devIdx := range devIndices {
		// ── Step 1: Discover battery feature index ──
		// Try 0x1004 (Unified Battery) first, then fallback to 0x1000 (Battery Status)
		featureToTry := []uint16{0x1004, 0x1000}
		var batIdx byte
		var usedFeature uint16

		for _, feat := range featureToTry {
			featHi := byte(feat >> 8)
			featLo := byte(feat & 0xFF)
			res, ok := hidppSend(h, devIdx, 0x00, 0x00, []byte{featHi, featLo})

			if ok && res[2] != byte(0x8F) && res[4] != 0 {
				batIdx = res[4]
				usedFeature = feat
				break
			}
		}

		if batIdx == 0 {
			fmt.Printf("  devIdx=0x%02X: battery features (0x1004/0x1000) not supported.\n", devIdx)
			continue
		}

		if usedFeature == 0x1000 {
			// ── Step 2: GetBatteryLevelStatus (featIdx=batIdx, func=0) ──
			// WARNING: data returned here is the Bolt receiver's CACHED state — it
			// may be stale if the mouse is in power-save mode. The status byte is
			// especially unreliable; wake the mouse (move it) for accurate results.
			res, ok := hidppSend(h, devIdx, batIdx, 0x00, nil)
			if !ok {
				fmt.Printf("  devIdx=0x%02X: no response to battery query.\n", devIdx)
				continue
			}
			level := res[4]
			statusCode := res[6]
			fmt.Printf("\n--- MX Master 3S Battery (cached — mouse is sleeping) ---\n")
			fmt.Printf("Battery: %d%%\n", level)
			fmt.Printf("Status:  %s (may be stale — move the mouse for live data)\n", statusText(statusCode))
			return true
		}

		// ── 0x1004 Unified Battery ──────────────────────────────────────────
		// The mouse may be asleep; the Bolt receiver must forward our packet
		// and wait for it to wake, which can take 1–3 s.
		// Retry up to 5 times with a 2-second timeout each attempt.
		fmt.Printf("  devIdx=0x%02X: found 0x1004 (Unified Battery), waking mouse...\n", devIdx)
		const maxRetries = 5
		var res []byte
		var ok bool
		for attempt := 1; attempt <= maxRetries; attempt++ {
			res, ok = hidppSendTimeout(h, devIdx, batIdx, 0x01, nil, 2*time.Second)
			if ok {
				break
			}
			if attempt < maxRetries {
				fmt.Printf("  devIdx=0x%02X: attempt %d/%d timed out, retrying...\n", devIdx, attempt, maxRetries)
			}
		}
		if !ok {
			fmt.Printf("  devIdx=0x%02X: mouse did not wake in time.\n", devIdx)
			continue
		}

		// Unified Battery (0x1004) GetBatteryStatus response:
		//   res[4] = charge   (%)
		//   res[5] = flags    (bit 0 = wireless charging, bit 1 = wired charging)
		//   res[6] = status   (0=discharge, 1=recharge, 2=almost full, 3=full, 4=slow, 5=invalid, 6=thermal err)
		level := res[4]
		statusRaw := res[6]
		statusStr := "Unknown"
		switch statusRaw {
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
		fmt.Printf("\n--- MX Master 3S Battery ---\n")
		fmt.Printf("Battery: %d%%\n", level)
		fmt.Printf("Status:  %s\n", statusStr)
		return true
	}
	return false
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
