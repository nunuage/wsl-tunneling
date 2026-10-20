# wsl-tunneling

**Your Windows connection. Inside WSL.**

Windows is connected, but your Linux tools cannot reach the network? **wsl-tunneling** routes WSL traffic through Windows, helping you keep working when a VPN gets in the way.

Start it from the system tray. Get back to your terminal. Stop it when you're done—without editing routes or DNS by hand.

**[Download for Windows](https://github.com/Clooooud/wsl-tunneling/releases/latest/download/wsl-tunneling-windows-amd64.zip)** · [Get started](#get-started) · [Get help](https://github.com/Clooooud/wsl-tunneling/issues)

## Why wsl-tunneling?

- **One-click control.** Start and stop the tunnel from a small tray panel, with a clear connection status.
- **Windows DNS, automatically.** Reuse Windows DNS search suffixes for internal hostnames without copying them yourself.
- **Easy to undo.** Stopping the tunnel restores your saved WSL network settings.

## Get started

You'll need **Windows with WSL 2** and an installed Linux distribution. Keep WSL up to date, and allow access to GitHub for the first-run downloads.

### 1. Download and open

Download the [Windows x64 app](https://github.com/Clooooud/wsl-tunneling/releases/latest/download/wsl-tunneling-windows-amd64.zip).

Extract the archive to a folder you want to keep. Leave both executables together, then double-click **wsl-tunneling.exe**. Look for its icon in the Windows system tray, including the hidden icons area.

### 2. Choose your WSL distribution

Click the tray icon and select **Open config folder**. The app creates a configuration file if one doesn't exist.

Open **config.json** and set `distro` to your distribution's name, for example:

```json
{
	"distro": "Ubuntu-24.04"
}
```

Not sure of the name? Run `wsl --list --quiet` in PowerShell. Use the exact name shown, save the file, and leave the other settings at their defaults.

### 3. Start the tunnel

Open the tray panel and click **Start**. The app downloads its networking helpers automatically on the first start, so that one may take longer.

That's it—carry on using your Linux tools as usual.

| Tray action | What it does |
| --- | --- |
| **Start** | Connects WSL through Windows. |
| **Stop** | Stops the tunnel and restores the previous WSL networking. |
| **Start on boot** | Opens the tray app when you sign in to Windows; it does not automatically start the tunnel. |
| **Quit** | Closes the tray app. Use **Stop** first if you also want to disconnect. |

## A few things to know

- This app uses your existing Windows connection. It is **not a VPN service**, and VPN compatibility depends on your setup.
- WSL networking is shared between distributions, so the tunnel is not isolated to just the distribution selected in the config.
- Your Linux distribution needs Bash, standard networking tools, and Windows interoperability enabled.
- `ping` is not a reliable connection check for this tunnel. Try accessing a website or the service you need instead.

## Need a hand?

If the tunnel won't start, check the distribution name in your config. For a quick diagnosis, open PowerShell in the app's folder and run:

```powershell
.\wsl-tunneling-cli.exe doctor
```

To return to your previous networking, choose **Stop** in the tray panel. You can find advanced settings in the [example configuration](configs/example.json).

Still stuck? [Open an issue](https://github.com/Clooooud/wsl-tunneling/issues) with your Windows and WSL versions and a description of what happened.

<details>
<summary><strong>Prefer the command line?</strong></summary>

Run these commands from the folder where you extracted the app:

| Action | PowerShell command |
| --- | --- |
| Start | `.\wsl-tunneling-cli.exe start` |
| Check status | `.\wsl-tunneling-cli.exe status` |
| Stop | `.\wsl-tunneling-cli.exe stop` |
| Restart | `.\wsl-tunneling-cli.exe restart` |
| View logs | `.\wsl-tunneling-cli.exe logs` |

</details>

<details>
<summary><strong>Build from source</strong></summary>

With Go 1.22 or newer installed, run this from the repository in PowerShell:

```powershell
.\scripts\build.ps1
```

Both executables are written to the **bin** folder. To run the tests:

```powershell
go test ./...
```

</details>

Powered by [gvisor-tap-vsock](https://github.com/containers/gvisor-tap-vsock).
