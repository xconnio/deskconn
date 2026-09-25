package common

import (
	"fmt"
	"os"
	"time"
)

func PrintProgress(name string, received, total int64, elapsed time.Duration) {
	const nameWidth = 40
	runes := []rune(name)
	display := name
	if len(runes) > nameWidth {
		display = "..." + string(runes[len(runes)-(nameWidth-3):])
	}

	if total <= 0 {
		fmt.Fprintf(os.Stderr, "\r%-*s   --%%  %s", nameWidth, display, formatBytes(received))
		return
	}

	pct := int64(100) * received / total
	if received >= total {
		pct = 100
	}

	rateStr := ""
	etaStr := "--:--"
	if elapsed.Seconds() >= 0.1 && received > 0 {
		rate := float64(received) / elapsed.Seconds()
		rateStr = formatBytes(int64(rate)) + "/s"
		if received < total {
			sec := int(float64(total-received) / rate)
			etaStr = fmt.Sprintf("%02d:%02d", sec/60, sec%60)
		} else {
			sec := int(elapsed.Seconds())
			etaStr = fmt.Sprintf("%02d:%02d", sec/60, sec%60)
		}
	}

	fmt.Fprintf(os.Stderr, "\r%-*s %3d%% %9s  %-12s  %s",
		nameWidth, display, pct, formatBytes(received), rateStr, etaStr)
}

func formatBytes(b int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	fb := float64(b)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1fGB", fb/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1fMB", fb/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1fKB", fb/float64(kb))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
