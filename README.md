# FORCE Remote Browser

A simple file manager for the **Akai Force**, run from your PC. Copy, cut, paste, rename and delete files on your Force, and move them between its drives (internal SSD, SD card, USB), without taking the drive out.

FORCE Remote Browser runs as a small program on your computer and opens a two-pane browser page. Every file operation runs on the Force itself over SSH, so copying between drives on the Force never sends the files through your PC.

> **Unofficial.** This project is not made, endorsed or supported by Akai Professional or inMusic. Use it at your own risk and keep backups of anything important.

## Features

- Two panes side by side, so you can show one drive on the left and another on the right
- Name, Type, Size and Date modified columns; click a column header to sort by it
- **Back**, **Up** and **Open** buttons in each pane, plus a clickable folder path to jump to any higher level
- **Copy**, **Cut** and **Paste** between any folders, including across drives
- **Select all**, **Rename**, **New folder** and **Delete** (asks for confirmation)
- Never overwrites: if a file with the same name already exists, it tells you and skips it
- Remembers your Force's IP address, so later it connects straight away
- One program file with no installation or Python required

### Keyboard shortcuts

| Key | Action |
| --- | --- |
| Ctrl+A | Select all (press again to clear the selection) |
| Ctrl+C / Ctrl+X / Ctrl+V | Copy / Cut / Paste |
| F2 | Rename |
| Delete | Delete |
| F5 | Refresh |
| Enter | Open the selected folder |
| Backspace / Alt+Left | Back to the previous folder |
| Alt+Up | Up one level |
| Tab | Switch between the left and right pane |

## Requirements

- An Akai Force on **MPC OS 3.10.1 or later** with **developer mode** turned on. Developer mode starts the Force's SSH server.
- The Force and your PC on the same network.
- An SSH client on your PC. Windows 10 and 11 include one ("OpenSSH Client"); macOS and Linux have one built in. If Windows says ssh is missing, turn it on under **Settings > System > Optional features**.

## Usage

1. Download `FORCE Remote Browser.exe` from the [Releases](../../releases) page.
2. Double-click it. A console window opens, and the browser page opens in your web browser.
3. Enter your Force's IP address and click **Connect**. The default user is `root`; leave the password empty if your Force has none.
4. Select files, click **Copy** or **Cut**, open the destination folder in either pane, and click **Paste**.

Keep the console window open while you use FORCE Remote Browser. Closing it stops the program.

Windows may warn about an unknown app the first time because the program is not code-signed. Click **More info**, then **Run anyway**. If you would rather not run an unsigned program, build it yourself from source (see below).

### Settings

- **Folder that holds the drives**: FORCE Remote Browser only works inside this folder (default `/media`), so it cannot touch the Force's system files. Change it on the connect screen if your drives are mounted somewhere else.
- The IP address, user and drives folder are saved in `force_browser.json` next to the program. Your password is never saved.

## Building from source

You need [Go](https://go.dev/dl/) 1.22 or later. There are no other dependencies.

```sh
# Windows program, built from any operating system
GOOS=windows GOARCH=amd64 go build -ldflags "-s -w" -o "FORCE Remote Browser.exe" .

# For the computer you are on
go build -o force-remote-browser .
```

On Windows in PowerShell, set the variables first: `$env:GOOS="windows"; $env:GOARCH="amd64"`.

### Test mode

To try the interface without a Force, run it against a local folder (macOS/Linux):

```sh
FORCE_LOCAL=1 FORCE_ROOT=/path/to/a/test/folder go run .
```

## How it works

- `main.go` starts a small web server on `127.0.0.1` only, so other devices on your network cannot reach it. A random token protects it from other websites.
- Each action is turned into a shell command (`ls`, `cp -r`, `mv`, `mkdir`, `rm -rf`) and sent to the Force with the system's `ssh` program. File names are quoted safely, and every path is checked to stay inside the drives folder.
- `page.html` is the interface. It is built into the program at compile time.

## Security notes

- The program skips SSH host key checking, because the Force's key can change after a firmware update. Only use it on a network you trust.
- Turn developer mode off on the Force when you are not using it, especially on public or shared networks.

## License

[MIT](LICENSE)
