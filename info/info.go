package info

import (
	"os"
	"time"

	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/mem"
	psnet "github.com/shirou/gopsutil/net"
)

type NetworkInterface struct {
	Name        string  `json:"name"`
	BytesSentPS float64 `json:"bytes_sent_ps"`
	BytesRecvPS float64 `json:"bytes_recv_ps"`
}

type CPUTimes struct {
	User    float64 `json:"user"`
	System  float64 `json:"system"`
	Nice    float64 `json:"nice"`
	Idle    float64 `json:"idle"`
	IOWait  float64 `json:"iowait"`
	IRQ     float64 `json:"irq"`
	SoftIRQ float64 `json:"softirq"`
	Steal   float64 `json:"steal"`
}

type DeviceInfo struct {
	CPUModel    string    `json:"cpu_model"`
	CPUPhysical int       `json:"cpu_physical"`
	CPULogical  int       `json:"cpu_logical"`
	CPUUsages   []float64 `json:"cpu_usages"`
	CPUTimes    CPUTimes  `json:"cpu_times"`

	RAMTotal     uint64 `json:"ram_total"`
	RAMFree      uint64 `json:"ram_free"`
	RAMUsed      uint64 `json:"ram_used"`
	RAMBuffCache uint64 `json:"ram_buff_cache"`
	RAMAvailable uint64 `json:"ram_available"`

	SwapTotal uint64 `json:"swap_total"`
	SwapFree  uint64 `json:"swap_free"`
	SwapUsed  uint64 `json:"swap_used"`

	DiskUsed  uint64 `json:"disk_used"`
	DiskFree  uint64 `json:"disk_free"`
	DiskTotal uint64 `json:"disk_total"`

	NetworkInterfaces []NetworkInterface `json:"network_interfaces"`

	Battery *BatteryInfo `json:"battery,omitempty"`
}

func GetDeviceInfo() (*DeviceInfo, error) {
	physical, err := cpu.Counts(false)
	if err != nil {
		return nil, err
	}
	logical, err := cpu.Counts(true)
	if err != nil {
		return nil, err
	}

	cpuModel := ""
	if infos, err := cpu.Info(); err == nil && len(infos) > 0 {
		cpuModel = infos[0].ModelName
	}

	// Two readings 200ms apart to measure actual current CPU and network activity.
	perCPU1, err := cpu.Times(true)
	if err != nil {
		return nil, err
	}
	agg1, err := cpu.Times(false)
	if err != nil {
		return nil, err
	}
	netStats1, err := psnet.IOCounters(true)
	if err != nil {
		return nil, err
	}
	time.Sleep(200 * time.Millisecond)
	perCPU2, err := cpu.Times(true)
	if err != nil {
		return nil, err
	}
	agg2, err := cpu.Times(false)
	if err != nil {
		return nil, err
	}
	netStats2, err := psnet.IOCounters(true)
	if err != nil {
		return nil, err
	}

	// Per-CPU usages from the delta.
	var cpuUsages []float64
	for i := range perCPU1 {
		if i >= len(perCPU2) {
			break
		}
		t1, t2 := perCPU1[i], perCPU2[i]
		total := (t2.User - t1.User) + (t2.System - t1.System) + (t2.Nice - t1.Nice) +
			(t2.Idle - t1.Idle) + (t2.Iowait - t1.Iowait) + (t2.Irq - t1.Irq) +
			(t2.Softirq - t1.Softirq) + (t2.Steal - t1.Steal)
		if total > 0 {
			cpuUsages = append(cpuUsages, (1-(t2.Idle-t1.Idle)/total)*100)
		} else {
			cpuUsages = append(cpuUsages, 0)
		}
	}

	// Aggregate breakdown (us/sy/ni/id/wa/hi/si/st) from the combined delta.
	var times CPUTimes
	if len(agg1) > 0 && len(agg2) > 0 {
		t1, t2 := agg1[0], agg2[0]
		du := t2.User - t1.User
		ds := t2.System - t1.System
		dn := t2.Nice - t1.Nice
		di := t2.Idle - t1.Idle
		diw := t2.Iowait - t1.Iowait
		dhi := t2.Irq - t1.Irq
		dsi := t2.Softirq - t1.Softirq
		dst := t2.Steal - t1.Steal
		total := du + ds + dn + di + diw + dhi + dsi + dst
		if total > 0 {
			times = CPUTimes{
				User:    du / total * 100,
				System:  ds / total * 100,
				Nice:    dn / total * 100,
				Idle:    di / total * 100,
				IOWait:  diw / total * 100,
				IRQ:     dhi / total * 100,
				SoftIRQ: dsi / total * 100,
				Steal:   dst / total * 100,
			}
		}
	}

	vmStat, err := mem.VirtualMemory()
	if err != nil {
		return nil, err
	}

	swapStat, err := mem.SwapMemory()
	if err != nil {
		return nil, err
	}

	diskStat, err := disk.Usage("/")
	if err != nil {
		return nil, err
	}

	physicalNames, err := physicalInterfaces()
	if err != nil {
		return nil, err
	}
	netMap := make(map[string]psnet.IOCountersStat, len(netStats1))
	for _, s := range netStats1 {
		netMap[s.Name] = s
	}
	var netInterfaces []NetworkInterface
	for _, s2 := range netStats2 {
		if _, ok := physicalNames[s2.Name]; !ok {
			continue
		}
		s1, ok := netMap[s2.Name]
		if !ok {
			continue
		}
		netInterfaces = append(netInterfaces, NetworkInterface{
			Name:        s2.Name,
			BytesSentPS: float64(s2.BytesSent-s1.BytesSent) / 0.2,
			BytesRecvPS: float64(s2.BytesRecv-s1.BytesRecv) / 0.2,
		})
	}

	deviceInfo := &DeviceInfo{
		CPUModel:    cpuModel,
		CPUPhysical: physical,
		CPULogical:  logical,
		CPUUsages:   cpuUsages,
		CPUTimes:    times,

		RAMTotal:     vmStat.Total,
		RAMFree:      vmStat.Free,
		RAMUsed:      vmStat.Used,
		RAMBuffCache: vmStat.Buffers + vmStat.Cached,
		RAMAvailable: vmStat.Available,

		SwapTotal: swapStat.Total,
		SwapFree:  swapStat.Free,
		SwapUsed:  swapStat.Used,

		DiskUsed:  diskStat.Total - diskStat.Free,
		DiskFree:  diskStat.Free,
		DiskTotal: diskStat.Total,

		NetworkInterfaces: netInterfaces,
	}

	if battery, err := GetBatteryInfo(); err == nil {
		deviceInfo.Battery = battery
	}

	return deviceInfo, nil
}

// physicalInterfaces returns a set of interface names that correspond to real hardware (ethernet, WiFi).
func physicalInterfaces() (map[string]struct{}, error) {
	ifaces, err := psnet.Interfaces()
	if err != nil {
		return nil, err
	}
	names := make(map[string]struct{}, len(ifaces))
	for _, iface := range ifaces {
		if iface.HardwareAddr == "" {
			continue
		}
		if _, err := os.Lstat("/sys/class/net/" + iface.Name + "/device"); err == nil {
			names[iface.Name] = struct{}{}
		}
	}

	return names, nil
}
