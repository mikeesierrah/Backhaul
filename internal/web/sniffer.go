package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/net"

	"github.com/sirupsen/logrus"
)

type Usage struct {
	dataStore   sync.Map
	listenAddr  string
	shutdownCtx context.Context
	cancelFunc  context.CancelFunc
	server      *http.Server
	logger      *logrus.Logger
	snifferLog  string
	mu          sync.Mutex
}

type PortUsage struct {
	Port  int
	Usage uint64
}

func NewDataStore(listenAddr string, shutdownCtx context.Context, snifferLog string, logger *logrus.Logger) *Usage {
	ctx, cancel := context.WithCancel(shutdownCtx)
	u := &Usage{
		listenAddr:  listenAddr,
		shutdownCtx: ctx,
		cancelFunc:  cancel,
		logger:      logger,
		snifferLog:  snifferLog,
		mu:          sync.Mutex{},
	}
	return u
}

func (m *Usage) Monitor() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", m.handleIndex)    // handle index
	mux.HandleFunc("/data", m.handleData) // New route for JSON data
	mux.HandleFunc("/stats", m.statsHandler)

	m.server = &http.Server{
		Addr:    m.listenAddr,
		Handler: mux,
	}

	go func() {
		<-m.shutdownCtx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		// Attempt to gracefully shut down the server
		if err := m.server.Shutdown(shutdownCtx); err != nil {
			m.logger.Error("sniffer server shutdown error: %v", err)
		}
	}()

	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				go m.saveUsageData()
			case <-m.shutdownCtx.Done():
				return
			}
		}
	}()

	// Start the server
	m.logger.Info("sniffer server starting at", m.listenAddr)
	if err := m.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		m.logger.Error("sniffer server error: %v", err)
	}
}

//go:embed index.html
var indexHTML embed.FS

func (m *Usage) handleIndex(w http.ResponseWriter, r *http.Request) {
	usageData := m.getUsageFromFile()
	readableData := m.usageDataWithReadableUsage(usageData)

	tmpl, err := template.ParseFS(indexHTML, "index.html")
	if err != nil {
		m.logger.Error("error parsing template: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, readableData)
	if err != nil {
		m.logger.Error("error executing template: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (m *Usage) handleData(w http.ResponseWriter, r *http.Request) {
	usageData := m.getUsageFromFile()
	readableData := m.usageDataWithReadableUsage(usageData)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(readableData); err != nil {
		m.logger.Errorf("error encoding JSON response: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (m *Usage) statsHandler(w http.ResponseWriter, r *http.Request) {
	stats, err := getSystemStats()
	if err != nil {
		log.Println("Error fetching system stats:", err)
		http.Error(w, "Unable to fetch system stats", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(stats); err != nil {
		log.Println("Error encoding JSON:", err)
		http.Error(w, "Failed to encode JSON", http.StatusInternalServerError)
	}
}

func (m *Usage) AddOrUpdatePort(port int, usage uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Retrieve current usage data for the port
	value, ok := m.dataStore.Load(port)
	if ok {
		// Port exists, update usage
		portUsage := value.(PortUsage)
		portUsage.Usage += usage
		m.dataStore.Store(port, portUsage)
	} else {
		// Port does not exist, create new entry
		m.dataStore.Store(port, PortUsage{Port: port, Usage: usage})
	}
}

func (m *Usage) saveUsageData() {
	// Step 1: Load existing usage data from the JSON file
	var existingUsageData []PortUsage
	file, err := os.Open(m.snifferLog)
	if err == nil {
		// If the file exists, decode the JSON data into existingUsageData
		defer file.Close()
		err = json.NewDecoder(file).Decode(&existingUsageData)
		if err != nil {
			m.logger.Errorf("error decoding JSON data: %v", err)
			return
		}
	} else if !os.IsNotExist(err) {
		// Log any error except file not existing
		m.logger.Errorf("error opening JSON file: %v", err)
		return
	}

	// Step 2: Get current usage data from sync.Map
	currentUsageData := m.collectUsageDataFromSyncMap()

	// Step 3: Merge the existing and current usage data into a map to avoid duplicates
	usageMap := make(map[int]PortUsage)

	// Add existing usage data to the map
	for _, usage := range existingUsageData {
		usageMap[usage.Port] = usage
	}

	// Append or update current usage data in the map
	for _, usage := range currentUsageData {
		if existing, exists := usageMap[usage.Port]; exists {
			// Update existing port usage
			existing.Usage += usage.Usage
			usageMap[usage.Port] = existing
		} else {
			// Add new port usage
			usageMap[usage.Port] = usage
		}
	}

	// Step 4: Convert the map back to a slice
	var mergedUsageData []PortUsage
	for _, usage := range usageMap {
		mergedUsageData = append(mergedUsageData, usage)
	}

	// Step 5: Convert merged data to JSON
	data, err := json.MarshalIndent(mergedUsageData, "", "  ")
	if err != nil {
		m.logger.Errorf("error marshalling usage data: %v", err)
		return
	}

	// Step 6: Write JSON data to file
	err = os.WriteFile(m.snifferLog, data, 0644)
	if err != nil {
		m.logger.Errorf("error writing usage data to file: %v", err)
	}
}

func (m *Usage) getUsageFromFile() []PortUsage {
	var usageData []PortUsage

	// Open the JSON file
	file, err := os.Open(m.snifferLog)
	if err != nil {
		m.logger.Error("error opening JSON file: %v", err)
		return nil
	}
	defer file.Close()

	// Decode the JSON file into the usageData slice
	err = json.NewDecoder(file).Decode(&usageData)
	if err != nil {
		m.logger.Error("error decoding JSON data: %v", err)
		return nil
	}

	// Sort usageData by Port in ascending order
	sort.Slice(usageData, func(i, j int) bool {
		return usageData[i].Port < usageData[j].Port
	})

	return usageData
}

// converts the byte usage to a human-readable format
func (m *Usage) usageDataWithReadableUsage(usageData []PortUsage) []struct {
	Port          int
	ReadableUsage string
} {
	var result []struct {
		Port          int
		ReadableUsage string
	}

	for _, portUsage := range usageData {
		result = append(result, struct {
			Port          int
			ReadableUsage string
		}{
			Port:          portUsage.Port,
			ReadableUsage: convertBytesToReadable(int64(portUsage.Usage)),
		})
	}

	return result
}

// collectUsageDataFromSyncMap gathers data from sync.Map
func (m *Usage) collectUsageDataFromSyncMap() []PortUsage {
	m.mu.Lock()
	defer m.mu.Unlock()

	var usageData []PortUsage
	m.dataStore.Range(func(key, value interface{}) bool {
		if portUsage, ok := value.(PortUsage); ok {
			usageData = append(usageData, portUsage)
			m.dataStore.Delete(key)
		}
		return true
	})
	return usageData
}

// ConvertBytesToReadable converts bytes into a human-readable format (KB, MB, GB)
func convertBytesToReadable(bytes int64) string {
	const (
		KB = 1 << (10 * 1) // 1024 bytes
		MB = 1 << (10 * 2) // 1024 KB
		GB = 1 << (10 * 3) // 1024 MB
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes) // Bytes
	}
}

type SystemStats struct {
	CPUUsage       string `json:"cpuUsage"`
	RAMUsage       string `json:"ramUsage"`
	DiskUsage      string `json:"diskUsage"`
	SwapUsage      string `json:"swapUsage"`
	NetworkTraffic string `json:"networkTraffic"`
	UploadSpeed    string `json:"uploadSpeed"`
	DownloadSpeed  string `json:"downloadSpeed"`
}

func getSystemStats() (*SystemStats, error) {

	// Get initial network stats
	initialStats, err := getNetworkStats()
	if err != nil {
		return nil, err
	}

	// Wait for 1 second
	time.Sleep(1 * time.Second)

	// Get updated network stats
	finalStats, err := getNetworkStats()
	if err != nil {
		return nil, err
	}

	// Get CPU usage
	cpuPercent, err := cpu.Percent(0, false)
	if err != nil {
		return nil, err
	}

	// Get RAM usage
	memStats, err := mem.VirtualMemory()
	if err != nil {
		return nil, err
	}

	// Get Disk usage
	diskStats, err := disk.Usage("/")
	if err != nil {
		return nil, err
	}

	// Get Swap usage
	swapStats, err := mem.SwapMemory()
	if err != nil {
		return nil, err
	}

	// Get Network traffic
	netStats, err := net.IOCounters(false)
	if err != nil {
		return nil, err
	}

	// Calculate upload and download speeds
	uploadSpeed := float64(finalStats.BytesSent - initialStats.BytesSent)
	downloadSpeed := float64(finalStats.BytesRecv - initialStats.BytesRecv)

	stats := &SystemStats{
		CPUUsage:       formatFloat(cpuPercent[0]),
		RAMUsage:       formatBytes(memStats.Used),
		DiskUsage:      formatBytes(diskStats.Used),
		SwapUsage:      formatBytes(swapStats.Used),
		NetworkTraffic: formatBytes(netStats[0].BytesSent + netStats[0].BytesRecv),
		DownloadSpeed:  formatSpeed(downloadSpeed),
		UploadSpeed:    formatSpeed(uploadSpeed),
	}

	return stats, nil
}

func formatSpeed(bytesPerSec float64) string {
	if bytesPerSec >= 1e9 {
		return fmt.Sprintf("%.2f GB/s", bytesPerSec/1e9)
	} else if bytesPerSec >= 1e6 {
		return fmt.Sprintf("%.2f MB/s", bytesPerSec/1e6)
	} else if bytesPerSec >= 1e3 {
		return fmt.Sprintf("%.2f KB/s", bytesPerSec/1e3)
	}
	return fmt.Sprintf("%.2f B/s", bytesPerSec)
}

func formatFloat(value float64) string {
	return fmt.Sprintf("%.2f%%", value)
}

func formatBytes(bytes uint64) string {
	return fmt.Sprintf("%.2f GB", float64(bytes)/(1024*1024*1024))
}

func getNetworkStats() (*net.IOCountersStat, error) {
	ioCounters, err := net.IOCounters(false)
	if err != nil {
		return nil, err
	}
	if len(ioCounters) == 0 {
		return nil, fmt.Errorf("no network IO counters found")
	}
	return &ioCounters[0], nil
}
