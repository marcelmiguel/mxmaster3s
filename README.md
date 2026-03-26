## MXMaster 3S Tray Icon info for Windows

## Features

- Show battery level and status in tray icon
- Show battery level and status in tooltip
- Show battery level and status in menu
- Show battery level and status in notification
- Adaptive polling interval: 30 minutes when mouse is awake, 2 minutes when mouse is sleeping

## Build

```bash
go build -ldflags="-H windowsgui" -o mxmaster3-tray.exe .
```
