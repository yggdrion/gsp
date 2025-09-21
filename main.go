package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ShellyStatus represents the status response from a Shelly device
type ShellyStatus struct {
	Meters []struct {
		Power     float64   `json:"power"`
		IsValid   bool      `json:"is_valid"`
		Timestamp int64     `json:"timestamp"`
		Counters  []float64 `json:"counters"`
	} `json:"meters"`
	Relays []struct {
		IsOn           bool   `json:"ison"`
		HasTimer       bool   `json:"has_timer"`
		TimerStarted   int64  `json:"timer_started"`
		TimerDuration  int    `json:"timer_duration"`
		TimerRemaining int    `json:"timer_remaining"`
		Overpower      bool   `json:"overpower"`
		Source         string `json:"source"`
	} `json:"relays"`
}

// ShellyInfo represents device info from a Shelly device
type ShellyInfo struct {
	Type        string `json:"type"`
	Mac         string `json:"mac"`
	AuthEnabled bool   `json:"auth_en"`
	FwVersion   string `json:"fw"`
	DeviceID    string `json:"id"`
}

// ShellyExporter implements prometheus.Collector
type ShellyExporter struct {
	powerGauge   *prometheus.GaugeVec
	deviceInfo   *prometheus.GaugeVec
	scanDuration prometheus.Gauge
	devicesFound prometheus.Gauge
	mutex        sync.RWMutex
	networkRange string
	scanInterval time.Duration
}

// NewShellyExporter creates a new Shelly exporter
func NewShellyExporter(networkRange string, scanInterval time.Duration) *ShellyExporter {
	return &ShellyExporter{
		powerGauge: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "shelly_power_watts",
				Help: "Current power consumption in watts from Shelly devices",
			},
			[]string{"device_id", "device_type", "mac_address", "ip_address", "meter_id"},
		),
		deviceInfo: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "shelly_device_info",
				Help: "Information about Shelly devices (always 1)",
			},
			[]string{"device_id", "device_type", "mac_address", "ip_address", "firmware_version"},
		),
		scanDuration: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "shelly_scan_duration_seconds",
				Help: "Duration of the last network scan in seconds",
			},
		),
		devicesFound: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "shelly_devices_found_total",
				Help: "Total number of Shelly devices found in the last scan",
			},
		),
		networkRange: networkRange,
		scanInterval: scanInterval,
	}
}

// Describe implements prometheus.Collector
func (e *ShellyExporter) Describe(ch chan<- *prometheus.Desc) {
	e.powerGauge.Describe(ch)
	e.deviceInfo.Describe(ch)
	e.scanDuration.Describe(ch)
	e.devicesFound.Describe(ch)
}

// Collect implements prometheus.Collector
func (e *ShellyExporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.RLock()
	defer e.mutex.RUnlock()

	e.powerGauge.Collect(ch)
	e.deviceInfo.Collect(ch)
	e.scanDuration.Collect(ch)
	e.devicesFound.Collect(ch)
}

// scanNetwork scans the network for Shelly devices
func (e *ShellyExporter) scanNetwork(ctx context.Context) {
	log.Printf("Starting network scan for Shelly devices...")
	start := time.Now()

	e.mutex.Lock()
	// Reset metrics
	e.powerGauge.Reset()
	e.deviceInfo.Reset()
	e.mutex.Unlock()

	var wg sync.WaitGroup
	foundDevices := 0
	var foundMutex sync.Mutex

	// Get local network range
	ips := e.getIPRange()

	// Scan each IP address
	for _, ip := range ips {
		wg.Add(1)
		go func(ipAddr string) {
			defer wg.Done()

			select {
			case <-ctx.Done():
				return
			default:
			}

			if e.isShellyDevice(ipAddr) {
				foundMutex.Lock()
				foundDevices++
				foundMutex.Unlock()

				e.collectShellyMetrics(ipAddr)
			}
		}(ip)
	}

	wg.Wait()

	duration := time.Since(start).Seconds()
	e.scanDuration.Set(duration)
	e.devicesFound.Set(float64(foundDevices))

	log.Printf("Network scan completed in %.2f seconds, found %d Shelly devices", duration, foundDevices)
}

// getIPRange returns a list of IP addresses in the local network range
func (e *ShellyExporter) getIPRange() []string {
	var ips []string

	// Parse the network range (assuming CIDR notation like 192.168.1.0/24)
	_, ipNet, err := net.ParseCIDR(e.networkRange)
	if err != nil {
		log.Printf("Error parsing network range %s: %v", e.networkRange, err)
		return ips
	}

	// Generate IP addresses in the range
	for ip := ipNet.IP.Mask(ipNet.Mask); ipNet.Contains(ip); inc(ip) {
		ips = append(ips, ip.String())
	}

	// Remove network and broadcast addresses
	if len(ips) > 2 {
		return ips[1 : len(ips)-1]
	}
	return ips
}

// inc increments an IP address
func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

// isShellyDevice checks if the given IP is a Shelly device
func (e *ShellyExporter) isShellyDevice(ip string) bool {
	client := &http.Client{Timeout: 2 * time.Second}

	resp, err := client.Get(fmt.Sprintf("http://%s/shelly", ip))
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	var info ShellyInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return false
	}

	return info.Type != ""
}

// collectShellyMetrics collects metrics from a Shelly device
func (e *ShellyExporter) collectShellyMetrics(ip string) {
	client := &http.Client{Timeout: 5 * time.Second}

	// Get device info
	infoResp, err := client.Get(fmt.Sprintf("http://%s/shelly", ip))
	if err != nil {
		log.Printf("Error getting device info from %s: %v", ip, err)
		return
	}
	defer infoResp.Body.Close()

	var info ShellyInfo
	if err := json.NewDecoder(infoResp.Body).Decode(&info); err != nil {
		log.Printf("Error decoding device info from %s: %v", ip, err)
		return
	}

	// Get device status
	statusResp, err := client.Get(fmt.Sprintf("http://%s/status", ip))
	if err != nil {
		log.Printf("Error getting status from %s: %v", ip, err)
		return
	}
	defer statusResp.Body.Close()

	var status ShellyStatus
	if err := json.NewDecoder(statusResp.Body).Decode(&status); err != nil {
		log.Printf("Error decoding status from %s: %v", ip, err)
		return
	}

	e.mutex.Lock()
	defer e.mutex.Unlock()

	// Set device info metric
	e.deviceInfo.WithLabelValues(
		info.DeviceID,
		info.Type,
		info.Mac,
		ip,
		info.FwVersion,
	).Set(1)

	// Set power metrics for each meter
	for i, meter := range status.Meters {
		if meter.IsValid {
			e.powerGauge.WithLabelValues(
				info.DeviceID,
				info.Type,
				info.Mac,
				ip,
				strconv.Itoa(i),
			).Set(meter.Power)
		}
	}

	log.Printf("Collected metrics from Shelly device %s (%s) at %s", info.DeviceID, info.Type, ip)
}

// startPeriodicScan starts the periodic network scanning
func (e *ShellyExporter) startPeriodicScan(ctx context.Context) {
	// Initial scan
	e.scanNetwork(ctx)

	ticker := time.NewTicker(e.scanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.scanNetwork(ctx)
		}
	}
}

// getEnv gets an environment variable with a default value
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func main() {
	// Configuration - can be overridden by environment variables
	networkRange := getEnv("NETWORK_RANGE", "10.10.10.0/24")
	scanIntervalStr := getEnv("SCAN_INTERVAL", "30s")
	port := getEnv("HTTP_PORT", ":8080")

	// Parse scan interval
	scanInterval, err := time.ParseDuration(scanIntervalStr)
	if err != nil {
		log.Fatalf("Invalid scan interval '%s': %v", scanIntervalStr, err)
	}

	// Ensure port starts with ':'
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	log.Printf("Starting Shelly Prometheus Exporter")
	log.Printf("Network range: %s", networkRange)
	log.Printf("Scan interval: %s", scanInterval)
	log.Printf("Metrics endpoint: http://localhost%s/metrics", port)

	// Create exporter
	exporter := NewShellyExporter(networkRange, scanInterval)

	// Register with Prometheus
	prometheus.MustRegister(exporter)

	// Start periodic scanning in background
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go exporter.startPeriodicScan(ctx)

	// Setup HTTP server for metrics
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `
<html>
<head><title>Shelly Prometheus Exporter</title></head>
<body>
<h1>Shelly Prometheus Exporter</h1>
<p><a href="/metrics">Metrics</a></p>
<p>Network range: %s</p>
<p>Scan interval: %s</p>
</body>
</html>`, networkRange, scanInterval)
	})

	log.Printf("Starting HTTP server on %s", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatalf("Error starting HTTP server: %v", err)
	}
}
