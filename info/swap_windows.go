package info

import (
	"errors"
	"unsafe"

	"github.com/shirou/gopsutil/mem"
	"golang.org/x/sys/windows"
)

// windowsPageSize is the base page size on every architecture deskconn ships for (x86, amd64,
// arm64) - large-page support is a separate opt-in feature and doesn't apply here.
const windowsPageSize = 4096

// systemPageFileInformation mirrors the undocumented but stable SYSTEM_PAGEFILE_INFORMATION
// struct returned by NtQuerySystemInformation(SystemPageFileInformation, ...). There's one entry
// per configured page file, chained via NextEntryOffset (0 marks the last entry).
type systemPageFileInformation struct {
	NextEntryOffset uint32
	TotalSize       uint32 // pages
	TotalInUse      uint32 // pages
	PeakUsage       uint32 // pages
	PageFileName    struct {
		Length        uint16
		MaximumLength uint16
		Buffer        uintptr
	}
}

// swapMemory returns actual Windows page-file usage, summed across every configured page file.
// gopsutil's SwapMemory on Windows instead reports the system commit charge (total committed
// virtual memory, whether or not it's actually backed by the page file) via GetPerformanceInfo -
// a much larger, different number that isn't page-file usage, so it isn't used here: on a
// machine with 16GiB RAM and a lightly used page file, it reports tens of GB of "swap used" when
// the page file itself has only a few hundred MB actually written to it.
func swapMemory() (*mem.SwapMemoryStat, error) {
	buf := make([]byte, 4096)
	var retLen uint32
	for {
		// buf is grown in bounded, small doublings below (capped at 1<<20), so it never
		// approaches uint32's range.
		statusErr := windows.NtQuerySystemInformation(windows.SystemPageFileInformation,
			unsafe.Pointer(&buf[0]), uint32(len(buf)), &retLen) //nolint:gosec
		if statusErr == nil {
			break
		}
		if !errors.Is(statusErr, windows.STATUS_INFO_LENGTH_MISMATCH) || len(buf) >= 1<<20 {
			return nil, statusErr
		}
		buf = make([]byte, len(buf)*2)
	}
	if retLen == 0 {
		return &mem.SwapMemoryStat{}, nil // no page file configured
	}

	var total, used uint64
	entrySize := uint32(unsafe.Sizeof(systemPageFileInformation{}))
	for offset := uint32(0); offset+entrySize <= uint32(len(buf)); { //nolint:gosec
		entry := (*systemPageFileInformation)(unsafe.Pointer(&buf[offset]))
		total += uint64(entry.TotalSize) * windowsPageSize
		used += uint64(entry.TotalInUse) * windowsPageSize
		if entry.NextEntryOffset == 0 {
			break
		}
		offset += entry.NextEntryOffset
	}

	stat := &mem.SwapMemoryStat{Total: total, Used: used, Free: total - used}
	if total > 0 {
		stat.UsedPercent = float64(used) / float64(total) * 100
	}
	return stat, nil
}
