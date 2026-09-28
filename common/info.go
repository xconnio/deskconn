package common

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
