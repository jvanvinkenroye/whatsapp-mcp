#!/usr/bin/env bash
# Monitors WhatsApp Bridge and sends notification on logout

LOG_FILE="${WA_DATA_DIR:-$HOME/.local/share/wa}/bridge.log"

tail -F "$LOG_FILE" 2>/dev/null | while read -r line; do
    if echo "$line" | grep -qi "logged out\|scan QR code"; then
        osascript -e 'display notification "WhatsApp Session abgelaufen - QR-Code scannen!" with title "WhatsApp Bridge"'
    fi
done
