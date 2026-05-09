// Package solar estimates photovoltaic panel power output using the same
// irradiance primitives as the calcbright brightness package.
//
// The key reuse: IlluminanceOnTilt called with luminousEfficacy=1.0 returns
// W/m² (plane-of-array irradiance) instead of lux.
package solar

import (
	"math"
	"time"

	"github.com/icyavocado/calcbright/brightness"
)

// Panel describes a photovoltaic panel installation.
type Panel struct {
	AreaM2     float64 // panel area in m² (e.g. 1.7)
	Efficiency float64 // conversion efficiency 0–1 (e.g. 0.18 = 18%)
	PR         float64 // performance ratio 0–1 (e.g. 0.80)
	AzDeg      float64 // panel azimuth, degrees clockwise from North
	TiltDeg    float64 // panel tilt, degrees from horizontal
}

// DefaultPanel returns a sensible Panel for a given latitude.
// Tilt is set to abs(lat) (optimal annual tilt); azimuth is equator-facing.
func DefaultPanel(lat float64) Panel {
	az := 180.0
	if lat < 0 {
		az = 0.0
	}
	return Panel{
		AreaM2:     1.7,
		Efficiency: 0.18,
		PR:         0.80,
		AzDeg:      az,
		TiltDeg:    math.Abs(lat),
	}
}

// HourPoint is one hour's irradiance and power for the daily profile chart.
type HourPoint struct {
	Hour        int     // 0–23 UTC
	ClearPOA    float64 // W/m² clear-sky POA irradiance
	OWMPOA      float64 // W/m² cloud-attenuated POA irradiance
	ClearPowerW float64 // W clear-sky output
	OWMPowerW   float64 // W cloud-attenuated output
}

// Report holds the result of an Estimate call.
type Report struct {
	// Instantaneous values at time t
	ClearPOA    float64 // W/m²
	OWMPOA      float64 // W/m²
	ClearPowerW float64 // W
	OWMPowerW   float64 // W

	// Daily totals for the UTC calendar day containing t
	ClearDailyKWh float64
	OWMDailyKWh   float64

	// 24-point hourly profile (index = UTC hour 0–23)
	HourlyProfile []HourPoint
}

const albedo = 0.20 // fixed ground albedo (grass/dirt), matches brightness feature

// Estimate computes solar power output for the given panel at loc, at time t.
// cloudFrac (0–1) is used for the OWM-adjusted model; pass 0 for clear-sky only.
// The daily profile holds constant cloudFrac across all 24 hours.
func Estimate(t time.Time, loc brightness.Location, panel Panel, cloudFrac float64) (Report, error) {
	// --- Instantaneous ---
	sunAz, sunZen, err := brightness.SunPosition(t, loc)
	if err != nil {
		return Report{}, err
	}

	dni, dhi, ghi, err := brightness.ClearSkyIrradiance(t, loc)
	if err != nil {
		return Report{}, err
	}
	dniA, dhiA, ghiA := brightness.ApplyCloudAttenuation(dni, dhi, ghi, cloudFrac)

	clearPOA := poa(dni, dhi, ghi, sunAz, sunZen, panel)
	owmPOA := poa(dniA, dhiA, ghiA, sunAz, sunZen, panel)

	clearPowerW := power(clearPOA, panel)
	owmPowerW := power(owmPOA, panel)

	// --- Daily profile ---
	// Truncate t to midnight UTC so all 24 hour-steps are on the same day.
	dayStart := time.Date(t.UTC().Year(), t.UTC().Month(), t.UTC().Day(), 0, 0, 0, 0, time.UTC)

	var profile []HourPoint
	var clearDailyWh, owmDailyWh float64

	for h := 0; h < 24; h++ {
		tH := dayStart.Add(time.Duration(h) * time.Hour)

		sunAzH, sunZenH, err := brightness.SunPosition(tH, loc)
		if err != nil {
			// Non-fatal: append zero point and continue.
			profile = append(profile, HourPoint{Hour: h})
			continue
		}

		dniH, dhiH, ghiH, err := brightness.ClearSkyIrradiance(tH, loc)
		if err != nil {
			profile = append(profile, HourPoint{Hour: h})
			continue
		}
		dniAH, dhiAH, ghiAH := brightness.ApplyCloudAttenuation(dniH, dhiH, ghiH, cloudFrac)

		cPOA := poa(dniH, dhiH, ghiH, sunAzH, sunZenH, panel)
		oPOA := poa(dniAH, dhiAH, ghiAH, sunAzH, sunZenH, panel)
		cW := power(cPOA, panel)
		oW := power(oPOA, panel)

		clearDailyWh += cW // each point = 1 hour → Wh
		owmDailyWh += oW

		profile = append(profile, HourPoint{
			Hour:        h,
			ClearPOA:    cPOA,
			OWMPOA:      oPOA,
			ClearPowerW: cW,
			OWMPowerW:   oW,
		})
	}

	return Report{
		ClearPOA:      clearPOA,
		OWMPOA:        owmPOA,
		ClearPowerW:   clearPowerW,
		OWMPowerW:     owmPowerW,
		ClearDailyKWh: clearDailyWh / 1000.0,
		OWMDailyKWh:   owmDailyWh / 1000.0,
		HourlyProfile: profile,
	}, nil
}

// poa computes Plane-Of-Array irradiance in W/m² using IlluminanceOnTilt
// with luminousEfficacy=1.0 (passes irradiance through unchanged).
func poa(dni, dhi, ghi, sunAz, sunZen float64, panel Panel) float64 {
	_, _, _, total := brightness.IlluminanceOnTilt(
		dni, dhi, ghi,
		sunAz, sunZen,
		panel.AzDeg, panel.TiltDeg,
		albedo,
		1.0, // luminousEfficacy=1 → W/m² not lux
	)
	return total
}

// power converts POA irradiance to AC power output in watts.
func power(poaWm2 float64, panel Panel) float64 {
	return poaWm2 * panel.AreaM2 * panel.Efficiency * panel.PR
}
