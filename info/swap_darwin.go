package info

import "github.com/shirou/gopsutil/mem"

func swapMemory() (*mem.SwapMemoryStat, error) {
	return mem.SwapMemory()
}
