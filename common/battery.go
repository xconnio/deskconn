package common

type BatteryInfo struct {
	Present      bool    `json:"present"`
	Percentage   int     `json:"percentage"`
	Status       string  `json:"status"`
	Technology   string  `json:"technology,omitempty"`
	Manufacturer string  `json:"manufacturer,omitempty"`
	Model        string  `json:"model,omitempty"`
	CycleCount   int     `json:"cycle_count,omitempty"`
	VoltageNow   float64 `json:"voltage_now"`
	PowerNow     float64 `json:"power_now"`

	EnergyNow        float64 `json:"energy_now"`
	EnergyFull       float64 `json:"energy_full"`
	EnergyFullDesign float64 `json:"energy_full_design"`
	HealthPercent    float64 `json:"health_percent,omitempty"`

	TimeRemainingMins int `json:"time_remaining_mins,omitempty"`
}
