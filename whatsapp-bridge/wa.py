#!/usr/bin/env python3
"""WhatsApp CLI Tool - Send messages via the WhatsApp Bridge API."""

import argparse
import json
import os
import sqlite3
import subprocess
import sys
import tempfile
import time
from pathlib import Path

try:
    from rich.console import Console
    from rich.table import Table
    from rich.panel import Panel
    from rich import box
except ImportError:
    print("Error: rich is required. Install with: pip install rich")
    sys.exit(1)

try:
    import requests
except ImportError:
    print("Error: requests is required. Install with: pip install requests")
    sys.exit(1)

# Configuration
BRIDGE_DIR = Path(os.environ.get("WA_BRIDGE_DIR", Path(__file__).parent)).resolve()
DATA_DIR = Path(os.environ.get("WA_DATA_DIR", Path.home() / ".local" / "share" / "wa")).resolve()
DB_PATH = os.environ.get("WA_DB_PATH", str(DATA_DIR / "messages.db"))
API_URL = os.environ.get("WA_API_URL", "http://localhost:8080")
BRIDGE_PID_FILE = DATA_DIR / "bridge.pid"
BRIDGE_LOG_FILE = DATA_DIR / "bridge.log"

console = Console()


def is_bridge_running() -> bool:
    """Check if the bridge API is reachable."""
    try:
        response = requests.get(f"{API_URL}/api/send", timeout=2)
        return True  # Any response means it's running
    except requests.exceptions.RequestException:
        return False


def start_bridge() -> bool:
    """Start the bridge in the background."""
    bridge_binary = BRIDGE_DIR / "whatsapp-bridge"

    # Ensure data directory exists
    DATA_DIR.mkdir(parents=True, exist_ok=True)

    if not bridge_binary.exists():
        console.print("[yellow]Building bridge...[/yellow]")
        result = subprocess.run(
            ["go", "build", "-o", "whatsapp-bridge", "main.go"],
            cwd=BRIDGE_DIR,
            capture_output=True,
        )
        if result.returncode != 0:
            console.print(f"[red]Error:[/red] Failed to build bridge")
            console.print(result.stderr.decode())
            return False

    console.print("[cyan]Starting WhatsApp Bridge...[/cyan]")
    console.print(f"[dim]Data directory: {DATA_DIR}[/dim]")

    # Start bridge in background with WA_DATA_DIR set
    env = os.environ.copy()
    env["WA_DATA_DIR"] = str(DATA_DIR)

    log_file = open(BRIDGE_LOG_FILE, "a")
    process = subprocess.Popen(
        [str(bridge_binary)],
        cwd=BRIDGE_DIR,
        stdout=log_file,
        stderr=log_file,
        start_new_session=True,
        env=env,
    )

    # Save PID
    BRIDGE_PID_FILE.write_text(str(process.pid))

    # Wait for bridge to be ready
    for i in range(30):  # Wait up to 30 seconds
        time.sleep(1)
        if is_bridge_running():
            console.print("[green]✓[/green] Bridge started")
            return True
        # Check if process is still running
        if process.poll() is not None:
            console.print("[red]Error:[/red] Bridge crashed. Check logs:")
            console.print(f"[dim]tail {BRIDGE_LOG_FILE}[/dim]")
            return False
        console.print(f"[dim]Waiting for bridge... ({i+1}s)[/dim]")

    console.print("[red]Error:[/red] Bridge did not start in time")
    return False


def ensure_bridge() -> bool:
    """Ensure bridge is running, start if needed."""
    if is_bridge_running():
        return True
    return start_bridge()


def stop_bridge():
    """Stop the bridge if running."""
    if BRIDGE_PID_FILE.exists():
        pid = int(BRIDGE_PID_FILE.read_text().strip())
        try:
            os.kill(pid, 15)  # SIGTERM
            console.print(f"[green]✓[/green] Bridge stopped (PID {pid})")
            BRIDGE_PID_FILE.unlink()
        except ProcessLookupError:
            console.print("[yellow]Bridge was not running[/yellow]")
            BRIDGE_PID_FILE.unlink()
    else:
        console.print("[yellow]No PID file found[/yellow]")


def get_chats() -> list[tuple[str, str, str]]:
    """Get all chats from database, returns list of (jid, name, type)."""
    if not Path(DB_PATH).exists():
        console.print(f"[red]Error:[/red] Database not found at {DB_PATH}")
        sys.exit(1)

    conn = sqlite3.connect(DB_PATH)
    cursor = conn.execute("""
        SELECT jid, COALESCE(name, jid), last_message_time
        FROM chats
        ORDER BY last_message_time DESC
    """)

    chats = []
    for jid, name, _ in cursor.fetchall():
        if "@g.us" in jid:
            chat_type = "Gruppe"
        elif "@s.whatsapp.net" in jid:
            chat_type = jid.replace("@s.whatsapp.net", "")
        elif "@newsletter" in jid:
            chat_type = "Newsletter"
        elif "@broadcast" in jid:
            chat_type = "Broadcast"
        else:
            chat_type = jid
        chats.append((jid, name, chat_type))

    conn.close()
    return chats


def list_chats():
    """Display chats in a rich table."""
    chats = get_chats()

    table = Table(
        title="WhatsApp Chats",
        box=box.ROUNDED,
        header_style="bold cyan",
        title_style="bold white",
    )

    table.add_column("#", style="dim", width=4)
    table.add_column("Name", style="white", no_wrap=True)
    table.add_column("Typ/Nummer", style="dim")

    for i, (jid, name, chat_type) in enumerate(chats, 1):
        if chat_type == "Gruppe":
            type_style = "[blue]Gruppe[/blue]"
        elif chat_type == "Newsletter":
            type_style = "[magenta]Newsletter[/magenta]"
        elif chat_type == "Broadcast":
            type_style = "[yellow]Broadcast[/yellow]"
        else:
            type_style = f"[dim]{chat_type}[/dim]"

        table.add_row(str(i), name, type_style)

    console.print(table)
    console.print(f"\n[dim]Total: {len(chats)} chats[/dim]")


def select_contact_fzf() -> str | None:
    """Use fzf to select a contact, returns JID."""
    chats = get_chats()

    # Format for fzf: JID\tName (Type)
    fzf_input = "\n".join(
        f"{jid}\t{name} ({chat_type})"
        for jid, name, chat_type in chats
    )

    try:
        result = subprocess.run(
            [
                "fzf",
                "--with-nth=2",
                "--delimiter=\t",
                "--header=Select contact (ESC to cancel)",
                "--height=80%",
                "--reverse",
                "--ansi",
            ],
            input=fzf_input,
            capture_output=True,
            text=True,
        )

        if result.returncode != 0:
            return None

        return result.stdout.strip().split("\t")[0]

    except FileNotFoundError:
        console.print("[red]Error:[/red] fzf is required for contact selection.")
        console.print("Install with: brew install fzf")
        sys.exit(1)


def record_voice() -> str | None:
    """Record voice message using ffmpeg, returns path to ogg file."""
    # Check for ffmpeg
    try:
        subprocess.run(["ffmpeg", "-version"], capture_output=True, check=True)
    except (FileNotFoundError, subprocess.CalledProcessError):
        console.print("[red]Error:[/red] ffmpeg is required for voice recording.")
        console.print("Install with: brew install ffmpeg")
        sys.exit(1)

    # Create temp file
    temp_file = tempfile.NamedTemporaryFile(suffix=".ogg", delete=False)
    temp_path = temp_file.name
    temp_file.close()

    console.print("[cyan]🎤 Recording...[/cyan] Press [bold]Enter[/bold] to stop")

    # Detect input device based on OS
    if sys.platform == "darwin":
        input_device = ["-f", "avfoundation", "-i", ":0"]
    elif sys.platform == "linux":
        input_device = ["-f", "pulse", "-i", "default"]
    else:
        console.print("[red]Error:[/red] Voice recording not supported on this OS")
        return None

    # Start recording in background
    ffmpeg_cmd = [
        "ffmpeg", "-y",
        *input_device,
        "-ac", "1",              # Mono
        "-ar", "48000",          # 48kHz (Opus standard)
        "-c:a", "libopus",       # Opus codec
        "-b:a", "32k",           # Bitrate
        "-application", "voip",  # Optimize for voice
        temp_path,
    ]

    process = subprocess.Popen(
        ffmpeg_cmd,
        stdin=subprocess.PIPE,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )

    # Wait for Enter key
    try:
        input()
    except KeyboardInterrupt:
        pass

    # Stop recording
    process.terminate()
    process.wait()

    # Check if file was created and has content
    if Path(temp_path).exists() and Path(temp_path).stat().st_size > 0:
        console.print(f"[green]✓[/green] Recorded to {temp_path}")
        return temp_path
    else:
        console.print("[red]Error:[/red] Recording failed")
        return None


def send_message(recipient: str, message: str, media_path: str | None = None):
    """Send a message via the API."""
    payload = {
        "recipient": recipient,
        "message": message,
    }

    if media_path:
        media_path = str(Path(media_path).resolve())
        if not Path(media_path).exists():
            console.print(f"[red]Error:[/red] Media file not found: {media_path}")
            sys.exit(1)
        payload["media_path"] = media_path

    try:
        response = requests.post(
            f"{API_URL}/api/send",
            json=payload,
            timeout=30,
        )
        data = response.json()

        if data.get("success"):
            console.print(f"[green]✓[/green] Message sent to [cyan]{recipient}[/cyan]")
        else:
            console.print(f"[red]✗[/red] Failed: {data.get('error', 'Unknown error')}")
            sys.exit(1)

    except requests.exceptions.ConnectionError:
        console.print("[red]Error:[/red] Cannot connect to WhatsApp Bridge")
        console.print(f"[dim]Make sure the bridge is running at {API_URL}[/dim]")
        sys.exit(1)
    except Exception as e:
        console.print(f"[red]Error:[/red] {e}")
        sys.exit(1)


def main():
    parser = argparse.ArgumentParser(
        description="Send WhatsApp messages via the bridge API.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  wa                           Select contact with fzf, then type message
  wa "Hello!"                  Select contact with fzf, send message
  wa -t 49123456789 "Hi"       Send to phone number directly
  wa -t "group@g.us" "Hi all"  Send to group JID
  wa -m photo.jpg "Check this" Send with media attachment
  wa -v                        Record and send voice message
  wa -m voice.ogg              Send existing voice file
  wa -l                        List all chats
  wa --start                   Start bridge in background
  wa --stop                    Stop bridge
  wa --status                  Check bridge status

Environment:
  WA_DB_PATH    Path to messages.db (default: store/messages.db)
  WA_API_URL    Bridge API URL (default: http://localhost:8080)
        """,
    )

    parser.add_argument("message", nargs="?", help="Message to send")
    parser.add_argument("-t", "--to", dest="recipient", help="Recipient JID or phone number")
    parser.add_argument("-m", "--media", dest="media_path", help="Media file to attach")
    parser.add_argument("-v", "--voice", action="store_true", help="Record and send voice message")
    parser.add_argument("-l", "--list", action="store_true", help="List all chats")
    parser.add_argument("--start", action="store_true", help="Start bridge in background")
    parser.add_argument("--stop", action="store_true", help="Stop bridge")
    parser.add_argument("--status", action="store_true", help="Check bridge status")

    args = parser.parse_args()

    # Bridge management commands
    if args.status:
        if is_bridge_running():
            console.print("[green]✓[/green] Bridge is running")
        else:
            console.print("[red]✗[/red] Bridge is not running")
        return

    if args.start:
        if is_bridge_running():
            console.print("[green]✓[/green] Bridge is already running")
        else:
            start_bridge()
        return

    if args.stop:
        stop_bridge()
        return

    # List mode (doesn't need bridge)
    if args.list:
        list_chats()
        return

    # For sending messages, ensure bridge is running
    if not ensure_bridge():
        console.print("[red]Error:[/red] Could not start bridge")
        console.print("[dim]Try running manually: ./whatsapp-bridge[/dim]")
        sys.exit(1)

    # Get recipient
    recipient = args.recipient
    if not recipient:
        recipient = select_contact_fzf()
        if not recipient:
            console.print("[yellow]Cancelled[/yellow]")
            return

    # Handle voice recording
    media_path = args.media_path
    if args.voice:
        media_path = record_voice()
        if not media_path:
            return
        # Voice messages don't need text
        message = args.message or ""
    else:
        # Get message
        message = args.message
        if not message:
            message = console.input("[cyan]Message:[/cyan] ")
            if not message.strip() and not media_path:
                console.print("[red]Error:[/red] Message cannot be empty")
                sys.exit(1)

    # Send
    send_message(recipient, message, media_path)


if __name__ == "__main__":
    main()
